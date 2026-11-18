package core

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Desk-phone features of the RingQ backend, the Go versions of
// ringq_desk_park.lua, ringq_desk_aux.lua and ringq_blf_pickup.lua.

func init() {
	RegisterScript("ringq_desk_park", scriptDeskPark)
	RegisterScript("ringq_desk_aux", scriptDeskAux)
	RegisterScript("ringq_blf_pickup", scriptBLFPickup)
}

func deskSafe(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', strings.ContainsRune("._-", r):
			return r
		}
		return -1
	}, v)
}

func chAlive(c *Core, uuid string) *Channel {
	if uuid == "" {
		return nil
	}
	if ch := c.Channel(uuid); ch != nil && !ch.IsHungUp() {
		return ch
	}
	return nil
}

// ---------------------------------------------------------------------------
// ringq_desk_park: shared parking from desk phones
//
// The RingQ apps park calls in 5 shared slots: the Redis list "<domain>:parked"
// (a slot holds the parked call's uuid or ""). A desk phone uses the same slots
// through the dialplan ringq_desk_park:
//
//	*591 .. *595, called by a call the phone transferred there -> park it in that slot
//	                                                             (the next free one if taken)
//	*591 .. *595, dialled by a phone with no call              -> pick up the call parked there
//	lamps <domain>   (luarun ringq_desk_park.lua lamps <domain>)
//	                 sends every slot's state to the phones whose BLF key watches *59N
//
// After each change it fires CUSTOM ringq::park, so the RingQ server sends the
// apps the new slot list.

const deskSlots = 5

func deskList(domain string) string { return deskSafe(domain) + ":parked" }

func deskEnsure(c *Core, domain string) {
	if c.redisStr("EXISTS", deskList(domain)) != "1" {
		c.redisStr("RPUSH", deskList(domain), "", "", "", "", "")
	}
}

func deskGet(c *Core, domain string, n int) string {
	return strings.TrimSpace(c.redisStr("LINDEX", deskList(domain), strconv.Itoa(n-1)))
}

func deskSet(c *Core, domain string, n int, uuid string) {
	c.redisStr("LSET", deskList(domain), strconv.Itoa(n-1), deskSafe(uuid))
}

// deskLamp lights (a call is parked there) or clears the BLF lamp of slot n:
// the slots are entries of the park lot, which the presence service reports.
func deskLamp(c *Core, domain string, n int, ch *Channel) {
	key := strings.ToLower(domain + "/*59" + strconv.Itoa(n))
	c.park.mu.Lock()
	if c.park.slots == nil {
		c.park.slots = map[string]*Channel{}
		c.park.since = map[string]time.Time{}
	}
	if ch != nil {
		if c.park.slots[key] != ch {
			c.park.since[key] = time.Now()
		}
		c.park.slots[key] = ch
	} else {
		delete(c.park.slots, key)
		delete(c.park.since, key)
	}
	c.park.mu.Unlock()
	c.presenceKick()
}

// deskChanged tells the RingQ server the slots changed (it sends the apps parkStatus).
func deskChanged(c *Core, domain string) {
	e := NewEvent(EvCustom, "ringq::park")
	e.Add("domain", domain)
	c.events.Publish(e)
}

func deskAllLamps(c *Core, domain string) {
	deskEnsure(c, domain)
	for n := 1; n <= deskSlots; n++ {
		uuid := deskGet(c, domain, n)
		ch := chAlive(c, uuid)
		if uuid != "" && ch == nil {
			deskSet(c, domain, n, "")
		}
		deskLamp(c, domain, n, ch)
	}
}

func scriptDeskPark(s *Script) error {
	c := s.Core
	if s.Arg(1) == "lamps" {
		if d := s.Arg(2); d != "" {
			deskAllLamps(c, d)
		}
		return nil
	}
	if !s.Ready() {
		return nil
	}
	n, err := strconv.Atoi(s.Arg(1))
	domain := s.Var("domain_name")
	if err != nil || n < 1 || n > deskSlots || domain == "" {
		s.Log("warn", "bad slot or domain", "slot", s.Arg(1), "domain", domain)
		return nil
	}
	deskEnsure(c, domain)
	me := s.UUID()
	current := deskGet(c, domain, n)
	// a call that was talking to someone and got transferred here (blind transfer / park key during
	// a call), as opposed to a phone dialling the slot with no call
	history := s.Var("transfer_history")
	transferred := s.Var("last_bridge_to") != "" || (strings.Contains(history, "bl_xfer") && strings.Contains(history, "*59"))

	if !transferred {
		// a phone with no call pressed the key: pick up what is parked there
		if parked := chAlive(c, current); parked != nil && current != me {
			s.Log("info", "slot picked up", "slot", n, "by", s.Var("caller_id_number"))
			deskSet(c, domain, n, "")
			parked.SetVar("ringq_park_taken", "true")
			deskLamp(c, domain, n, nil)
			deskChanged(c, domain)
			return c.takeOver(s.Ctx, s.Ch, parked)
		}
		if current != "" {
			deskSet(c, domain, n, "")
			deskLamp(c, domain, n, nil)
			deskChanged(c, domain)
		}
		_ = s.Execute("playback", "tone_stream://%(500,500,480,620);loops=3")
		s.Hangup("NORMAL_CLEARING")
		return nil
	}

	// a call was transferred here: park it (the next free slot when this one is taken)
	if chAlive(c, current) != nil && current != me {
		free := 0
		for i := 1; i <= deskSlots; i++ {
			if chAlive(c, deskGet(c, domain, i)) == nil {
				free = i
				break
			}
		}
		if free == 0 {
			s.Log("warn", "all parking slots are taken")
			_ = s.Execute("playback", "tone_stream://%(250,250,480,620);loops=8")
			s.Hangup("NORMAL_CIRCUIT_CONGESTION")
			return nil
		}
		n = free
	}
	if err := s.Answer(); err != nil {
		return err
	}
	s.SetVar("ringq_park_slot", strconv.Itoa(n))
	deskSet(c, domain, n, me)
	deskLamp(c, domain, n, s.Ch)
	deskChanged(c, domain)
	s.Log("info", "parked", "uuid", me, "slot", n)

	_ = s.Execute("playback", "misc/parkslot.wav")
	moh := s.Var("hold_music")
	mctx, stopMusic := context.WithCancel(s.Ctx)
	done := c.startMOH(mctx, s.Ch, moh, "", 0, "", nil)
	defer func() { stopMusic(); <-done }()
	for s.Ready() {
		s.Sleep(300)
		// picked up (desk phone or app) or moved: the slot no longer holds this call
		if s.Var("ringq_park_taken") == "true" || s.Var("park_pickup") != "" || deskGet(c, domain, n) != me {
			return nil
		}
	}
	// the parked caller hung up
	if deskGet(c, domain, n) == me {
		deskSet(c, domain, n, "")
		deskLamp(c, domain, n, nil)
		deskChanged(c, domain)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ringq_desk_aux: Agent Status / Change Status keys on desk phones
//
// The dialplan ringq_desk_aux: *26<aux code id> runs this with the id. It asks the
// RingQ web app on this server (IndexController deskAux) to set the caller's aux
// code as the dialer's Agent Status does, then plays a short confirmation tone
// (or an error tone).

func scriptDeskAux(s *Script) error {
	if !s.Ready() {
		return nil
	}
	aux, err := strconv.Atoi(strings.TrimSpace(s.Arg(1)))
	domain := deskSafe(s.Var("domain_name"))
	user := deskSafe(firstNonEmpty(s.Var("sip_from_user"), s.Var("caller_id_number")))
	if err := s.Answer(); err != nil {
		return err
	}
	if err != nil || domain == "" || user == "" {
		_ = s.Execute("playback", "tone_stream://%(250,250,480,620);loops=4")
		return nil
	}
	// the tenant's own https site (as the phones fetch their config), answered on this server
	u := fmt.Sprintf("https://%s:8443/provision/000000000000/aux?rq_desk_aux=1&domain=%s&user=%s&aux=%d",
		domain, url.QueryEscape(domain), url.QueryEscape(user), aux)
	// The connection is forced to this server, but the certificate is checked as for any client: the tenant's
	// domain is the server name, as in the URL, and the site's own certificate has to be valid for it.
	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, "127.0.0.1:8443")
		},
	}}
	code := 0
	if req, err := http.NewRequestWithContext(s.Ctx, http.MethodGet, u, nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			code = resp.StatusCode
			resp.Body.Close()
		} else {
			s.Log("warn", "aux request failed", "err", err)
		}
	}
	s.Log("info", "aux", "user", user+"@"+domain, "aux", aux, "http", code)
	if code == 200 {
		_ = s.Execute("playback", "tone_stream://%(150,100,880);%(150,100,1320)")
	} else {
		_ = s.Execute("playback", "tone_stream://%(250,250,480,620);loops=4")
	}
	s.Hangup("NORMAL_CLEARING")
	return nil
}

// ---------------------------------------------------------------------------
// ringq_blf_pickup: BLF key pickup on desk phones
//
// A desk phone's BLF key dials *8<SIP user of the watched extension>; the global
// dialplan blf_sip_user_pickup runs `ringq_blf_pickup.lua <SIP user>`, which picks up:
//  1. the call last dialled to that extension (hash <domain>-last_dial_ext, by
//     extension number, then by SIP user), while it is still ringing it;
//  2. a call ringing that phone right now. Queue calls end up here: the queue
//     rings its agents itself, so they never reach last_dial_ext. The queue caller
//     is taken out of the queue; any other ringing leg: its originator.
//
// A call that is already answered is never taken.

func isSIPUser(v string) bool {
	if len(v) != 20 {
		return false
	}
	for _, r := range v {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func scriptBLFPickup(s *Script) error {
	if s.Arg(1) == "show" {
		return nil // the display refresh after a pickup is not needed here
	}
	if !s.Ready() {
		return nil
	}
	c := s.Core
	sipUser := strings.ToLower(strings.TrimSpace(s.Arg(1)))
	if !isSIPUser(sipUser) {
		return nil
	}
	domain := s.Var("domain_name")
	own := s.UUID()
	number := ""
	if u := c.config().Directory.LookupIn(domain, sipUser); u != nil {
		number = u.ID
	}
	tried := map[string]bool{}
	try := func(target *Channel, why string) bool {
		if target == nil || target.UUID == own || tried[target.UUID] || target.IsHungUp() || target.IsAnswered() {
			return false // an answered call is never taken
		}
		tried[target.UUID] = true
		s.Log("info", "pickup", "key", "*8"+sipUser, "ext", number, "takes", target.UUID, "why", why)
		_ = c.takeOver(s.Ctx, s.Ch, target)
		return s.Ch.IsHungUp() || s.Ctx.Err() != nil // taken: the call is on and this leg is done
	}

	// 1. the call last dialled to the extension, while it is still ringing it
	for _, key := range []string{number, sipUser} {
		if key == "" {
			continue
		}
		uuid, _ := c.hashes.get("hash", domain+"-last_dial_ext", key)
		if t := chAlive(c, uuid); t != nil && chApp(t) == "bridge" && try(t, "dialled "+key) {
			return nil
		}
	}
	// 2. a call ringing the phone now
	for _, leg := range c.Channels() {
		v := leg.Vars()
		if leg.Direction != DirectionOutbound || leg.IsHungUp() || leg.IsAnswered() || !strings.EqualFold(v["dialed_domain"], domain) ||
			(v["dialed_user"] != number && v["dialed_user"] != sipUser) {
			continue
		}
		if member := v["cc_member_session_uuid"]; member != "" {
			if try(chAlive(c, member), "queue "+v["cc_queue"]) {
				return nil
			}
		} else if try(chAlive(c, v["originator"]), "ringing "+leg.UUID) {
			return nil
		}
	}
	s.Log("info", "no ringing call to pick up", "key", "*8"+sipUser, "ext", number)
	s.Hangup("NORMAL_CLEARING")
	return nil
}

// chApp is the application a channel is running ("" when none).
func chApp(ch *Channel) string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.app
}
