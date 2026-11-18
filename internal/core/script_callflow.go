package core

import (
	"context"
	"strings"
	"time"
)

// Call flows (the *7x toggles of FusionPBX): call_flow.lua, call_flow_monitor.lua,
// call_flow_subscribe.lua (blf_subscribe.lua flow).
//
// A call flow sends a call to one destination (app + data) while its status is
// "true" and to another (the alternate) while it is "false". Dialling its feature
// code toggles the status (after the PIN, when it has one), plays the sound of the
// new state and lights or clears the BLF lamp "flow+<code>" and "<code>" (lit while
// the status is "false", the alternate destination). The dialplan runs
// call_flow.lua with call_flow_uuid set; with feature_code=true it toggles, without
// it executes the destination.

func init() {
	RegisterScript("call_flow", scriptCallFlow)
	RegisterScript("call_flow_monitor", scriptCallFlowMonitor)
	RegisterScript("call_flow_subscribe", scriptCallFlowSubscribe)
	RegisterScript("blf_subscribe", scriptBLFSubscribe)
}

func scriptCallFlow(s *Script) error {
	if !s.Ready() {
		return nil
	}
	domain, flowUUID, featureCode := s.Var("domain_name"), s.Var("call_flow_uuid"), s.Var("feature_code")
	if flowUUID == "" {
		s.Log("warn", "can not get call flow uuid")
		return nil
	}
	row := s.Row("SELECT * FROM v_call_flows WHERE call_flow_uuid = $1::uuid", flowUUID)
	if row == nil {
		return nil
	}
	status := row["call_flow_status"]
	if status == "" {
		status = "true"
	}
	app, data := row["call_flow_app"], row["call_flow_data"]
	if status != "true" {
		app, data = row["call_flow_alternate_app"], row["call_flow_alternate_data"]
	}

	if featureCode != "true" {
		// not the feature code: go to the active destination
		s.Log("info", "call flow", "execute", app+" "+data)
		if s.Ready() && app != "" {
			return s.Execute(app, data)
		}
		return nil
	}

	// the PIN
	if pin := row["call_flow_pin_number"]; pin != "" && s.Ready() {
		_ = s.Answer()
		digits := s.GetDigits(len(pin), len(pin)+1, 3, 5000, "phrase:voicemail_enter_pass:#", `\d+`)
		if digits != pin {
			s.PlaySound("phrase:voicemail_fail_auth:#")
			s.Hangup("NORMAL_CLEARING")
			return nil
		}
	}

	// the feature code toggles the status
	toggle := "true"
	if status == "true" {
		toggle = "false"
	}
	code := row["call_flow_feature_code"]
	s.Core.TurnLamp(code, domain, toggle == "false", flowUUID)
	if !strings.HasPrefix(code, "flow+") {
		s.Core.TurnLamp("flow+"+code, domain, toggle == "false", flowUUID)
	}
	label, audio := row["call_flow_alternate_label"], row["call_flow_alternate_sound"]
	if toggle == "true" {
		label, audio = row["call_flow_label"], row["call_flow_sound"]
	}
	s.Log("info", "call flow toggled", "label", label, "status", toggle, "uuid", flowUUID, "audio", audio)
	s.Exec("UPDATE v_call_flows SET call_flow_status = $1::text WHERE call_flow_uuid = $2::uuid", toggle, flowUUID)

	if s.Ready() {
		_ = s.Answer()
	}
	if s.Ready() && label != "" {
		s.Sleep(1000)
		s.display(label)
	}
	if s.Ready() {
		if audio != "" {
			s.Sleep(1000)
			s.PlaySound(audio)
			s.Sleep(1000)
		} else {
			s.Sleep(2000)
		}
	}
	if s.Ready() {
		s.Hangup("")
	}
	return nil
}

// callFlowLamps sets the lamp of every call flow (call_flow_monitor's pass).
func callFlowLamps(s *Script) {
	for _, r := range s.Rows("SELECT d.domain_name, f.call_flow_uuid, f.call_flow_extension, f.call_flow_feature_code, f.call_flow_status, " +
		"f.call_flow_label, f.call_flow_alternate_label FROM v_call_flows AS f, v_domains AS d WHERE f.domain_uuid = d.domain_uuid") {
		s.Core.TurnLamp("flow+"+r["call_flow_feature_code"], r["domain_name"], r["call_flow_status"] == "false", r["call_flow_uuid"])
	}
}

// call_flow_monitor: every minute, set the lamps of all call flows from the database (a call flow that was
// changed on the web page, not from a phone). "call_flow_monitor stop" ends it; it also ends with the switch.
func scriptCallFlowMonitor(s *Script) error {
	c := s.Core
	if s.Arg(1) == "stop" {
		c.StopService("call_flow_monitor")
		return nil
	}
	interval := time.Duration(optNum(s.Arg(1), 60)) * time.Second
	if interval < time.Second {
		interval = time.Minute
	}
	ctx, done, ok := c.registerService("call_flow_monitor", s.Ctx)
	if !ok {
		return nil // already running
	}
	defer done()
	s.Log("info", "call flow monitor started")
	s.Ctx = ctx
	for {
		callFlowLamps(s)
		select {
		case <-ctx.Done():
			s.Log("info", "call flow monitor stopped")
			return nil
		case <-time.After(interval):
		}
	}
}

// call_flow_subscribe <from> <to> <expires>: a phone started watching a call flow key.
func scriptCallFlowSubscribe(s *Script) error { return blfSubscribeFlow(s, s.Arg(1), s.Arg(2), s.Arg(3)) }

// blf_subscribe flow <from> <to> <expires>
func scriptBLFSubscribe(s *Script) error {
	if s.Arg(1) == "flow" {
		return blfSubscribeFlow(s, s.Arg(2), s.Arg(3), s.Arg(4))
	}
	// voicemail, dnd, forward and agent status lamps are kept by the switch itself (presence.go)
	s.Log("debug", "blf_subscribe: the switch keeps this lamp itself", "protocol", s.Arg(1))
	return nil
}

func blfSubscribeFlow(s *Script, from, to, expires string) error {
	if n := optNum(expires, 0); n <= 0 {
		s.Log("info", "unsubscribe", "from", from, "to", to)
		return nil
	}
	user, domain, ok := strings.Cut(to, "@")
	if !ok || domain == "" {
		return nil
	}
	short := ""
	if _, after, has := strings.Cut(user, "+"); has {
		short = after
	}
	row := s.Row("SELECT t1.call_flow_uuid, t1.call_flow_status FROM v_call_flows t1 INNER JOIN v_domains t2 ON t1.domain_uuid = t2.domain_uuid "+
		"WHERE t2.domain_name = $1 AND (t1.call_flow_feature_code = $2 OR t1.call_flow_feature_code = $3)", domain, user, short)
	if row == nil {
		s.Log("warn", "can not find call flow", "to", to)
		return nil
	}
	s.Log("info", "found call flow", "to", to, "status", row["call_flow_status"])
	s.Core.TurnLamp(user, domain, row["call_flow_status"] == "false", row["call_flow_uuid"])
	return nil
}

// ---------------------------------------------------------------------------
// services: scripts that run in the background until they are stopped

// registerService marks a service as running; ok is false when it already is.
func (c *Core) registerService(name string, parent context.Context) (context.Context, func(), bool) {
	c.callfeat.mu.Lock()
	if c.callfeat.svc == nil {
		c.callfeat.svc = map[string]context.CancelFunc{}
	}
	if _, running := c.callfeat.svc[name]; running {
		c.callfeat.mu.Unlock()
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	c.callfeat.svc[name] = cancel
	c.callfeat.mu.Unlock()
	go func() { // the switch stopping ends it too
		select {
		case <-c.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		cancel()
		c.callfeat.mu.Lock()
		delete(c.callfeat.svc, name)
		c.callfeat.mu.Unlock()
	}, true
}

// StopService stops a running service script (call_flow_monitor).
func (c *Core) StopService(name string) {
	c.callfeat.mu.Lock()
	cancel := c.callfeat.svc[name]
	c.callfeat.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// StartService starts a service script (call_flow_monitor) in the background, as FreeSWITCH's startup-script did.
func (c *Core) StartService(name string, args ...string) {
	if !hasScript(name) {
		return
	}
	go func() {
		if _, _, err := c.runScript(context.Background(), nil, name, args); err != nil {
			c.log.Warn("service failed", "script", name, "err", err)
		}
	}()
}
