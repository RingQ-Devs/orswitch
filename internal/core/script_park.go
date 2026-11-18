package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FusionPBX call park: park.lua and park_monitor.lua.
//
// The dialplan sets park_extension (the lot, or the first lot of park_range), park_direction
// ("in" parks the call, "out" picks up what is parked there), park_music, park_timeout_type,
// park_timeout_seconds, park_timeout_destination and runs park.lua. The lots are kept in the
// "park" table of FusionPBX's park.db (lot, domain, uuid): here in memory (parkEntry). A parked
// lot lights its BLF lamp (TurnLamp); park_monitor.lua (started by park.lua with luarun) watches
// the parked call: when it is picked up, hangs up or times out it clears the lamp and the lot,
// sets park_status (parked, unparked, cancelled, timeout) and, on a timeout with
// park_timeout_type=transfer, sends the call to park_timeout_destination.
//
// The native valet_park application (pbx.go: valet_park <lot> <slot>) is what the portal's park
// dialplans (*591-*595 and *59NN) use; it needs no script. park.lua is the older FusionPBX way
// and is kept for tenants whose dialplans call it.

type parkEntry struct{ uuid string }

func parkKey(domain, lot string) string { return strings.ToLower(domain + "|" + lot) }

func (c *Core) parkGet(domain, lot string) (string, bool) {
	c.callfeat.mu.Lock()
	defer c.callfeat.mu.Unlock()
	e := c.callfeat.lots[parkKey(domain, lot)]
	if e == nil {
		return "", false
	}
	return e.uuid, true
}

func (c *Core) parkSet(domain, lot, uuid string) {
	c.callfeat.mu.Lock()
	defer c.callfeat.mu.Unlock()
	if c.callfeat.lots == nil {
		c.callfeat.lots = map[string]*parkEntry{}
	}
	c.callfeat.lots[parkKey(domain, lot)] = &parkEntry{uuid: uuid}
}

func (c *Core) parkDel(domain, lot string) {
	c.callfeat.mu.Lock()
	delete(c.callfeat.lots, parkKey(domain, lot))
	c.callfeat.mu.Unlock()
}

func init() {
	RegisterScript("park", scriptPark)
	RegisterScript("park_monitor", scriptParkMonitor)
}

func scriptPark(s *Script) error {
	if s.Ch == nil {
		return nil
	}
	c, uuid := s.Core, s.UUID()
	direction := s.Var("park_direction")
	domain := s.Var("domain_name")
	lot := s.Var("park_extension")
	rng := s.Var("park_range")
	music := s.Var("park_music")
	timeoutType := firstNonEmpty(s.Var("park_timeout_type"), "transfer")
	timeoutSecs := firstNonEmpty(s.Var("park_timeout_seconds"), "300")
	timeoutDest := s.Var("park_timeout_destination")
	if domain == "" || lot == "" {
		s.Log("warn", "park: no domain or park_extension")
		return nil
	}
	if err := s.Answer(); err != nil {
		return err
	}

	// park_range: the first lot of the range that is free
	if rng != "" {
		start, err1 := strconv.Atoi(lot)
		n, err2 := strconv.Atoi(rng)
		if err1 == nil && err2 == nil {
			for ext := start; ext <= start+n-1; ext++ {
				if _, taken := c.parkGet(domain, strconv.Itoa(ext)); !taken {
					lot = strconv.Itoa(ext)
					break
				}
			}
		}
	}
	lotUUID, occupied := c.parkGet(domain, lot)

	if direction == "out" {
		// pick up: connect this call to the parked one
		if occupied {
			c.chSetVar(lotUUID, "park_status", "unparked")
			s.Log("info", "park status: unparked", "lot", lot)
			s.API("uuid_bridge " + uuid + " " + lotUUID)
		}
		return nil
	}

	if occupied && c.Channel(lotUUID) == nil {
		// the parked call is gone: free the lot
		c.TurnLamp(lot, domain, false, lotUUID)
		c.parkDel(domain, lot)
		occupied = false
	}
	if !occupied {
		// park the call
		s.SetVar("park_status", "parked")
		s.Log("info", "park status: parked", "lot", lot)
		c.parkSet(domain, lot, uuid)
		c.TurnLamp(lot, domain, true, uuid)
		s.API(fmt.Sprintf("luarun park_monitor.lua %s %s %s %s %s %s", uuid, domain, lot, timeoutType, timeoutSecs, timeoutDest))
		// the call waits here (with its music) until it is picked up, transferred or hangs up
		pctx, cancel := context.WithCancel(s.Ctx)
		defer cancel()
		if music != "" {
			done := c.startMOH(pctx, s.Ch, music, "", 0, "", nil)
			defer func() { cancel(); <-done }()
		}
		select {
		case <-s.Ctx.Done():
		case <-s.Ch.Done():
		}
		return nil
	}

	// someone dials a lot that has a call parked: a local extension picks it up, anyone else goes back to the callee
	callerID := s.Var("caller_id_number")
	if c.userExists(callerID, domain) {
		c.TurnLamp(lot, domain, false, uuid)
		c.parkDel(domain, lot)
		c.chSetVar(lotUUID, "park_status", "unparked")
		s.Log("info", "park status: unparked", "lot", lot)
		s.API("uuid_bridge " + uuid + " " + lotUUID)
		return nil
	}
	if dialed := s.Var("dialed_user"); dialed != "" {
		return s.Execute("transfer", dialed+" XML "+s.Var("context"))
	}
	return nil
}

func (c *Core) chSetVar(uuid, name, value string) {
	if ch := c.Channel(uuid); ch != nil {
		ch.SetVar(name, value)
	}
}

// park_monitor <uuid> <domain> <lot> <timeout type> <timeout seconds> <timeout destination>
func scriptParkMonitor(s *Script) error {
	c := s.Core
	uuid, domain, lot, ttype := s.Arg(1), s.Arg(2), s.Arg(3), s.Arg(4)
	secs, dest := optNum(s.Arg(5), 300), s.Arg(6)
	if uuid == "" {
		return nil
	}
	end := func(status string) {
		c.TurnLamp(lot, domain, false, uuid)
		c.parkDel(domain, lot)
		c.chSetVar(uuid, "park_status", status)
		s.Log("info", "park status: "+status, "lot", lot)
	}
	for x := 0; ; {
		select {
		case <-s.Ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		ch := c.Channel(uuid)
		if ch == nil || ch.IsHungUp() {
			end("cancelled")
			break
		}
		if ch.Var("park_status") == "parked" {
			c.TurnLamp(lot, domain, true, uuid) // still parked: keep the lamp lit
		} else {
			end("unparked")
			break
		}
		x++
		if x > secs {
			end("timeout")
			break
		}
	}
	ch := c.Channel(uuid)
	if ch == nil {
		return nil
	}
	if prefix := ch.Var("park_caller_id_prefix"); prefix != "" {
		ch.SetVar("effective_caller_id_name", prefix+"#"+ch.Var("effective_caller_id_name"))
	}
	if ttype == "transfer" && ch.Var("park_status") == "timeout" && dest != "" {
		ch.SetVar("park_status", "unparked")
		s.Log("info", "park timeout: transferring", "to", dest)
		s.API("uuid_transfer " + uuid + " " + dest)
	}
	return nil
}
