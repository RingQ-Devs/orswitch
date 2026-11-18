package core

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ringq-devs/orswitch/internal/sip"
)

// Ring push: the equivalent of the mod_ringq endpoint of the FreeSWITCH based
// RingQ backend (ringq_core/docs/ring-push-design.md).
//
// When a domain's setting ringq / ring_push_mode is "wake", every dial of an
// extension (user/<ext>@<domain>: direct calls, queues, IVR options, ring
// groups, forwarding, transfers) goes through ringWakeBegin before the
// extension's registrations are looked up:
//
//  1. fire CUSTOM ringq::ring (extension, domain, ring id, caller, queue).
//     node/ringPush.js pushes the extension's phones (FCM / APNs) and answers
//     with `hash insert/ringq_ring/<ring id>/<pushed>/<iPhones pushed>`.
//  2. if phones were pushed (and the dial is not forked, so no other leg is held
//     back), wait for the woken app to register, up to ringq_ring_wake_wait_ms
//     (default 15 s); a desk phone that is online shortens the wait.
//  3. the leg is then dialed as usual, with the ring id as its SIP Call-ID, so the
//     app matches the INVITE to the push it got.
//  4. when the originate ends, fire CUSTOM ringq::ring_end (cause, answered,
//     registered) so ringPush.js can send the stop push.
//
// No extra call leg is created.

const (
	ringWakeDefaultWait = 15 * time.Second // wait for the woken app
	ringWakeDeskWait    = 3 * time.Second  // ... with a desk phone online
	ringWakeAllIOSWait  = 8 * time.Second  // once one pushed phone is up, the rest get this long
	ringWakePoll        = 250 * time.Millisecond
	ringWakeAnswerWait  = time.Second // for ringPush.js's answer
)

// wakeRing is one ring started for a user/ leg.
type wakeRing struct {
	id, ext, domain string
	legged          bool // a registered phone was dialed for it
}

// ringWakeBegin runs the push and wait for a user/<ext>[@<domain>] destination.
// It returns nil when the destination is not subject to ring push (other
// endpoints, the domain's mode is not "wake", an unknown extension, DND or
// forward-all, which resolveUser answers itself).
func (c *Core) ringWakeBegin(ctx context.Context, a *Channel, spec string, vars map[string]string, forked bool) *wakeRing {
	if !strings.HasPrefix(spec, "user/") {
		return nil
	}
	id, domain, _ := strings.Cut(strings.TrimPrefix(spec, "user/"), "@")
	if domain == "" {
		domain = c.legDomain(a, vars)
	}
	dom := c.config().Directory.Resolve(domain)
	if dom == nil || dom.RingPushMode != "wake" {
		return nil
	}
	u := dom.Lookup(id)
	if u == nil || u.Disabled {
		return nil
	}
	uv := c.userVars(u)
	if truthy(uv["do_not_disturb"]) || c.cc.blocksDirect(u.ID, u.Domain) {
		return nil
	}
	if truthy(uv["forward_all_enabled"]) {
		if dest := uv["forward_all_destination"]; dest != "" && dest != u.ID {
			return nil
		}
	}

	ring := &wakeRing{id: sip.NewUUID(), ext: u.ID, domain: u.Domain}
	mobile, others := c.wakeRegistrations(u.ID, u.Domain)
	registered := mobile+others > 0 || len(c.reg.Lookup(u.ID, u.Domain)) > 0

	aUUID, callerNum, callerName := "", firstNonEmpty(vars["origination_caller_id_number"]), firstNonEmpty(vars["origination_caller_id_name"])
	aVar := func(string) string { return "" }
	if a != nil {
		aUUID = a.UUID
		aVar = a.Var
		callerNum = firstNonEmpty(callerNum, aVar("effective_caller_id_number"), aVar("caller_id_number"))
		callerName = firstNonEmpty(callerName, aVar("effective_caller_id_name"), aVar("caller_id_name"))
	}
	queue := firstNonEmpty(vars["cc_queue"], aVar("cc_queue"))
	if q, _, ok := strings.Cut(queue, "@"); ok {
		queue = q
	}

	e := NewEvent(EvCustom, "ringq::ring")
	e.Add("ringq-ext", ring.ext)
	e.Add("ringq-domain", ring.domain)
	e.Add("ringq-ring-id", ring.id)
	e.Add("ringq-mode", "wake")
	e.Add("ringq-a-uuid", aUUID)
	e.Add("ringq-registered", strconv.FormatBool(registered))
	e.Add("ringq-caller-number", callerNum)
	e.Add("ringq-caller-name", callerName)
	e.Add("ringq-queue", queue)
	c.events.Publish(e)
	c.log.Info("ring push: ring", "ext", ring.ext, "domain", ring.domain, "ring", ring.id, "registered", registered, "forked", forked)

	// node's answer: "<phones pushed>/<iPhones pushed>"
	pushed, ios := -1, 0
	for deadline := time.Now().Add(ringWakeAnswerWait); pushed < 0 && time.Now().Before(deadline); {
		if v, ok := c.hashes.get("hash", "ringq_ring", ring.id); ok && strings.TrimSpace(v) != "" {
			n, rest, _ := strings.Cut(strings.TrimSpace(v), "/")
			pushed, _ = strconv.Atoi(n)
			ios, _ = strconv.Atoi(rest)
			break
		}
		select {
		case <-ctx.Done():
			return ring
		case <-time.After(50 * time.Millisecond):
		}
	}
	c.hashes.del("hash", "ringq_ring", ring.id)
	if pushed <= 0 || forked {
		return ring
	}

	wait := ringWakeDefaultWait
	if n, err := strconv.Atoi(firstNonEmpty(vars["ringq_ring_wake_wait_ms"], aVar("ringq_ring_wake_wait_ms"))); err == nil && n > 0 {
		wait = time.Duration(n) * time.Millisecond
	}
	deskWait := ringWakeDeskWait
	if n, err := strconv.Atoi(firstNonEmpty(vars["ringq_ring_wake_wait_desk_ms"], aVar("ringq_ring_wake_wait_desk_ms"))); err == nil && n >= 0 {
		deskWait = time.Duration(n) * time.Millisecond
	}
	start := time.Now()
	for {
		mobile, others = c.wakeRegistrations(ring.ext, ring.domain)
		waited := time.Since(start)
		limit := wait
		if others > 0 && limit > deskWait {
			limit = deskWait
		}
		done := false
		switch {
		case ios > 0:
			// wait for the pushed phones themselves, even if a desk phone is up; once one is up the rest
			// get until ringWakeAllIOSWait so a slow or switched-off phone does not hold the call back
			done = mobile >= ios || (mobile > 0 && waited >= ringWakeAllIOSWait)
		default:
			done = mobile+others > 0
		}
		if done || waited >= limit {
			break
		}
		select {
		case <-ctx.Done():
			return ring
		case <-time.After(ringWakePoll):
		}
	}
	c.log.Info("ring push: waited", "ext", ring.ext, "ring", ring.id, "ms", time.Since(start).Milliseconds(),
		"pushed", pushed, "mobile_registered", mobile)
	return ring
}

// ringWakeEnd tells ringPush.js how the ring ended (for the stop push).
func (c *Core) ringWakeEnd(w *wakeRing, winner *Channel, cause Cause) {
	answered := winner != nil && winner.Var("ringq_ring_id") == w.id
	if winner != nil && !answered {
		cause = CauseLoseRace // another destination answered
	} else if answered {
		cause = CauseNone
	}
	e := NewEvent(EvCustom, "ringq::ring_end")
	e.Add("ringq-ext", w.ext)
	e.Add("ringq-domain", w.domain)
	e.Add("ringq-ring-id", w.id)
	e.Add("ringq-cause", cause.String())
	e.Add("ringq-answered", strconv.FormatBool(answered))
	e.Add("ringq-registered", strconv.FormatBool(w.legged))
	c.events.Publish(e)
}

// wakeRegistrations counts the RingQ mobile app registrations of an extension
// (User-Agent "RingQ iOS - <device>" or "RingQ android - <device>") and its other
// phones that are not RingQ's own apps (desk phones, softphones).
func (c *Core) wakeRegistrations(user, domain string) (mobile, others int) {
	for _, b := range c.reg.Lookup(user, domain) {
		ua := strings.ToLower(b.UserAgent)
		switch {
		case strings.Contains(ua, "ringq ios") || strings.Contains(ua, "ringq android"):
			mobile++
		case !strings.Contains(ua, "ringq"):
			others++
		}
	}
	return mobile, others
}
