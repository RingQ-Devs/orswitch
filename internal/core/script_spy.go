package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/ringq-devs/orswitch/internal/media"
)

// Listening in on an extension's call from a phone: eavesdrop.lua and three_way.lua, and the
// eavesdrop / three_way dialplan applications they run.
//
//	eavesdrop.lua <extension>   the caller (after the PIN, when the dialplan sets pin_number) hears the
//	                            extension's call; nobody hears the caller
//	three_way.lua <extension>   the same, but only a connected call, and the caller is heard by both (barge)
//	eavesdrop <uuid>            application: listen to the call of that channel
//	three_way <uuid>            application: barge into the call of that channel
//
// The supervisor's phone is mixed into the call by the same code the web monitor uses (spy.go):
// the call's two legs go through the switch (G.711 only), and the call is reconnected when the
// supervisor hangs up. While listening, 1 = listen, 2 = whisper to the agent, 3 = barge (the RingQ
// monitor's keys; FreeSWITCH's eavesdrop used 1/2/3 for whisper to leg A / leg B / both).

func init() {
	RegisterScript("eavesdrop", func(s *Script) error { return scriptSpy(s, "listen") })
	RegisterScript("three_way", func(s *Script) error { return scriptSpy(s, "barge") })
	for name, mode := range map[string]string{"eavesdrop": "listen", "three_way": "barge"} {
		mode := mode
		builtinAppsExtra[name] = func(ctx context.Context, c *Core, ch *Channel, data string) error {
			return c.joinSpy(ctx, ch, strings.TrimSpace(data), mode)
		}
	}
}

// presenceChannels are the live channels of an extension (presence_id <ext>@<domain>).
func (c *Core) presenceChannels(ext, domain string) []*Channel {
	var out []*Channel
	id := strings.ToLower(ext + "@" + domain)
	for _, ch := range c.Channels() {
		if !ch.IsHungUp() && strings.ToLower(ch.Var("presence_id")) == id {
			out = append(out, ch)
		}
	}
	return out
}

func scriptSpy(s *Script, mode string) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	ext := s.Arg(1)
	_ = s.Answer()
	pin := s.Var("pin_number")
	domain := firstNonEmpty(s.Var("domain_name"), s.Var("sip_auth_realm"))
	if pin != "" {
		digits := s.GetDigits(len(pin), len(pin)+1, 3, 5000, "ivr/ivr-please_enter_pin_followed_by_pound.wav", `\d+`)
		if digits != pin {
			s.PlaySound("voicemail/vm-fail_auth.wav")
			s.Hangup("NORMAL_CLEARING")
			return nil
		}
	}
	if !s.Ready() {
		return nil
	}
	// the channel to listen to: the last one of the extension (the Lua query had no order); for
	// three_way only a connected call (callstate ACTIVE)
	var target *Channel
	for _, ch := range c.presenceChannels(ext, domain) {
		if ch == s.Ch || (mode == "barge" && !ch.IsAnswered()) {
			continue
		}
		target = ch
	}
	if target == nil {
		s.Log("info", "nothing to listen to", "ext", ext, "domain", domain)
		return nil
	}
	if err := c.joinSpy(s.Ctx, s.Ch, target.UUID, mode); err != nil {
		s.Log("warn", "can not join the call", "ext", ext, "err", err)
	}
	return nil
}

// joinSpy puts the channel sup into the call of the channel uuid (listen, whisper or barge) and
// returns when the supervisor or the call ends.
func (c *Core) joinSpy(ctx context.Context, sup *Channel, uuid, mode string) error {
	if !validSpyMode(mode) {
		return fmt.Errorf("mode: listen, whisper or barge")
	}
	a := c.Channel(uuid)
	if a == nil {
		return fmt.Errorf("no such channel %s", uuid)
	}
	b := a.Partner()
	if b == nil || !a.Answered() || !b.Answered() {
		return fmt.Errorf("the call is not connected")
	}
	if sup == a || sup == b {
		return fmt.Errorf("cannot listen to its own call")
	}
	for _, ch := range []*Channel{a, b, sup} {
		if ch.ep == nil {
			return fmt.Errorf("the call has no media")
		}
	}
	if err := startMedia(sup, false); err != nil {
		return err
	}
	for _, ch := range []*Channel{a, b, sup} {
		if cd, ok := ch.ep.SendCodec(); !ok || !media.CanTerminate(cd.Name) {
			return fmt.Errorf("this call uses a codec that cannot be monitored (only G.711 can)")
		}
	}
	key := callKey(a, b)
	c.spy.mu.Lock()
	if c.spy.byCall == nil {
		c.spy.byCall = map[string]*spySession{}
	}
	if c.spy.byCall[key] != nil {
		c.spy.mu.Unlock()
		return fmt.Errorf("the call is already monitored")
	}
	stop := make(chan struct{})
	sess := &spySession{mode: mode, call: key, super: sup, stop: stop}
	c.spy.byCall[key] = sess
	c.spy.mu.Unlock()
	defer func() {
		c.spy.mu.Lock()
		delete(c.spy.byCall, key)
		c.spy.mu.Unlock()
	}()
	c.log.Info("listening to a call", "call", a.UUID, "by", sup.Var("caller_id_number"), "mode", mode)
	c.recordEvent("call", fmt.Sprintf("%s started %s on the call %s <-> %s", sup.Var("caller_id_number"), mode, a.Var("caller_id_number"), b.Var("caller_id_number")))
	done := make(chan struct{})
	go func() {
		c.spyLoop(sess, a, b, sup)
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// the script was cancelled (the supervisor was transferred): stop mixing and give the call back
		close(stop)
		<-done
	}
	return nil
}
