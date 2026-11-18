package core

import (
	"fmt"
	"strconv"
	"strings"
)

// ringq_call_control: RingQ "Call Control" (per call-profile forwarding), the Go
// version of ringq_call_control.lua.
//
// It applies the extension's Call Control settings (q_call_behaviors, five slots:
// Available, Away, Busy, Custom 1, Custom 2; internal calls use the plain columns,
// external ones the columns ending in "2") for the profile the user has selected
// (Redis "<ext>+<domain>", set by the dialer's changeProfile), so a status change
// takes effect on the next call.
//
//	resolve <uuid>   inline from the dialplan (ringq-call-control, order 36): sets
//	                 forward_all_*, call_timeout and the rq_* variables of the call
//	screen           before the first bridge of local_extension: On Busy, call screening
//	failover         after the bridges: Call Relay, On Busy and No Answer Action;
//	                 it returns silently when nothing applies, so the voicemail step still runs

func init() { RegisterScript("ringq_call_control", scriptCallControl) }

func scriptCallControl(s *Script) error {
	switch s.Arg(1) {
	case "resolve":
		return ccResolve(s)
	case "screen":
		return ccScreen(s)
	case "failover":
		return ccFailover(s)
	}
	return nil
}

func ccSplit(v, sep string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, sep)
}

// ccSlot is the idx-th (0-based) comma separated value of csv, or def.
func ccSlot(csv string, idx int, def string) string {
	p := ccSplit(csv, ",")
	if idx >= len(p) {
		return def
	}
	return strings.TrimSpace(p[idx])
}

func ccBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "t", "1":
		return true
	}
	return false
}

// ccSafe keeps only the characters a destination or number can have.
func ccSafe(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', strings.ContainsRune("*#+@._-|", r):
			return r
		}
		return -1
	}, v)
}

func ccBoolStr(b bool) string { return strconv.FormatBool(b) }

// ccProfileIndex is the extension's selected call profile, 0..4 (default Available).
func ccProfileIndex(c *Core, ext, domain string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(c.redisStr("GET", ccSafe(ext)+"+"+ccSafe(domain)))); err == nil && n >= 0 && n <= 4 {
		return n
	}
	return 0
}

// ccRelayDests are the Call Relay destinations of one slot: 15 values (3 per slot) or 5 values "a|b|c".
func ccRelayDests(csv string, idx int) []string {
	all := ccSplit(csv, ",")
	var out []string
	if len(all) >= 15 {
		for i := 0; i < 3; i++ {
			out = append(out, strings.TrimSpace(all[idx*3+i]))
		}
		return out
	}
	if idx < len(all) {
		for _, d := range ccSplit(all[idx], "|") {
			out = append(out, strings.TrimSpace(d))
		}
	}
	return out
}

// ccOwnVM is true for "", "*99" and "*99<own ext>": the extension's own voicemail (the normal path).
func ccOwnVM(dest, ext string) bool { return dest == "" || dest == "*99" || dest == "*99"+ext }

func ccResolve(s *Script) error {
	c := s.Core
	ch := c.Channel(s.Arg(2))
	if ch == nil {
		return nil
	}
	ext, domain, extUUID := ch.Var("destination_number"), ch.Var("domain_name"), ch.Var("extension_uuid")
	if extUUID == "" {
		return nil
	}
	rows, err := c.sqlRows(s.Ctx, "SELECT * FROM q_call_behaviors WHERE extension_uuid = $1::uuid LIMIT 1", extUUID)
	if err != nil {
		s.Log("warn", "call behaviors not read", "ext", ext, "err", err)
		return nil
	}
	if len(rows) == 0 {
		s.Write("no-behaviors")
		return nil
	}
	row := rows[0]
	idx := ccProfileIndex(c, ext, domain)
	external := ch.Var("call_direction") != "local"
	sfx, dir := "", "internal"
	if external {
		sfx, dir = "2", "external"
	}
	set := func(k, v string) { ch.SetVar(k, ccSafe(v)) }

	naTimeout := 30
	if n, err := strconv.Atoi(ccSlot(row["forward_no_answer_timeout"+sfx], idx, "")); err == nil {
		naTimeout = n
	}
	naTimeout = max(5, min(300, naTimeout))

	// Always: call-forward-all (internal) and call-forward-all2 (external)
	for _, x := range []string{"", "2"} {
		en := ccBool(ccSlot(row["forward_all_enabled"+x], idx, "false"))
		dest := ccSlot(row["forward_all_destination"+x], idx, "")
		if ccOwnVM(dest, ext) {
			dest = "*99" + ext
		}
		set("forward_all_enabled"+x, ccBoolStr(en))
		set("forward_all_destination"+x, dest)
	}
	set("call_timeout", strconv.Itoa(naTimeout))
	set("follow_me_enabled", "false") // the relay is handled by the failover step
	set("rq_cc_profile", strconv.Itoa(idx))
	set("rq_cc_dir", dir)
	set("rq_cc_timeout", strconv.Itoa(naTimeout))
	set("rq_busy_enabled", ccBoolStr(ccBool(ccSlot(row["forward_busy_enabled"+sfx], idx, "false"))))
	set("rq_busy_dest", ccSlot(row["forward_busy_destination"+sfx], idx, ""))
	set("rq_na_enabled", ccBoolStr(ccBool(ccSlot(row["forward_no_answer_enabled"+sfx], idx, "false"))))
	set("rq_na_dest", ccSlot(row["forward_no_answer_destination"+sfx], idx, ""))
	var relays []string
	if ccBool(ccSlot(row["follow_me_enabled"+sfx], idx, "false")) {
		for _, d := range ccRelayDests(row["follow_me_destinations"+sfx], idx) {
			if d != "" && !ccOwnVM(d, ext) && d != ext {
				relays = append(relays, d)
			}
		}
	}
	set("rq_relay", strings.Join(relays, "|"))
	set("rq_screen", ccBoolStr(ccBool(ccSlot(row["call_screening"+sfx], idx, "false"))))
	// Parallel Ringing: ring the extension's mobile number at the same time
	mobile := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == '+' {
			return r
		}
		return -1
	}, row["mobile_number"])
	if ccBool(ccSlot(row["simultaneous_destination_enabled"+sfx], idx, "false")) && len(mobile) >= 5 {
		ch.SetVar("rq_parallel", fmt.Sprintf(",[leg_timeout=%d,rq_cc_leg=parallel]loopback/%s/%s", naTimeout, mobile, ccSafe(domain)))
	} else {
		set("rq_parallel", "")
	}
	s.Log("info", "call control resolved", "ext", ext+"@"+domain, "profile", idx, "dir", dir)
	s.Write("ok")
	return nil
}

// ccExtOnCall: is the extension already talking on another call? Its channels carry
// its extension as the user of the leg to its phone, or as the user of a call it made.
func ccExtOnCall(c *Core, ext, domain, own string) bool {
	for _, b := range c.Channels() {
		if b.UUID == own || b.IsHungUp() || !b.IsAnswered() {
			continue
		}
		v := b.Vars()
		switch {
		case b.Direction == DirectionOutbound && v["dialed_user"] == ext && strings.EqualFold(v["dialed_domain"], domain):
			return true
		case b.Direction == DirectionInbound && v["user_name"] == ext && strings.EqualFold(v["domain_name"], domain) && v["sip_authorized"] == "true":
			return true
		}
	}
	return false
}

func ccExt(s *Script) string {
	return firstNonEmpty(s.Var("dialed_extension"), s.Var("destination_number"))
}

// ccTransfer sends the call to dest in the call's domain.
func ccTransfer(s *Script, dest, domain string) {
	ctxName := ""
	if domain != "" && s.Core.config().Dialplan.Contexts[domain] != nil {
		ctxName = domain
	}
	s.Core.transfer(s.Ch, dest, ctxName)
}

func ccScreen(s *Script) error {
	if !s.Ready() {
		return nil
	}
	ext, domain := ccExt(s), s.Var("domain_name")

	// On Busy is switched on and the extension is already on a call: don't ring it
	// (apps and phones ring a second call as call waiting, so "busy" was never reported).
	if s.Var("rq_busy_enabled") == "true" && s.Var("rq_busy_checked") != ext {
		s.SetVar("rq_busy_checked", ext)
		if ccExtOnCall(s.Core, ext, domain, s.UUID()) {
			dest := strings.TrimSpace(s.Var("rq_busy_dest"))
			if dest == "hangup" {
				s.Log("info", "on a call: on-busy hangup", "ext", ext)
				s.Hangup("USER_BUSY")
				return nil
			}
			if ccOwnVM(dest, ext) {
				dest = "*99" + ext
			}
			s.Log("info", "on a call: on-busy", "ext", ext, "to", dest)
			s.SetVar("rq_failover_done", ext) // the failover step must not act again
			if s.Var("rq_cc_dir") == "external" {
				s.SetVar("outbound_caller_id_name", s.Var("caller_id_name"))
				s.SetVar("outbound_caller_id_number", s.Var("caller_id_number"))
			}
			ccTransfer(s, dest, domain)
			return nil
		}
	}
	if s.Var("rq_screen") != "true" || s.Var("rq_screened") == ext {
		return nil
	}
	s.SetVar("rq_screened", ext)
	file := fmt.Sprintf("/tmp/%s-screen-%s.wav", ccSafe(domain), s.UUID())
	if err := s.Answer(); err != nil {
		return err
	}
	s.Sleep(500)
	_ = s.Execute("playback", "phrase:voicemail_record_name")
	_ = s.Execute("playback", "tone_stream://%(500,0,640)")
	s.SetVar("playback_terminators", "#*0123456789")
	_ = s.Execute("record", file+" 5 200 2")
	// the callee hears the recording and presses 1 to take the call (the bridge asks)
	s.SetVar("group_confirm_key", "1")
	s.SetVar("fail_on_single_reject", "true")
	s.SetVar("group_confirm_file", "phrase:screen_confirm:"+file)
	s.SetVar("group_confirm_cancel_timeout", "1")
	return nil
}

func ccFailover(s *Script) error {
	disp := s.Var("originate_disposition")
	ext, domain := ccExt(s), s.Var("domain_name")
	if disp == "SUCCESS" || disp == "ANSWER" || s.Var("rq_failover_done") == ext {
		return nil
	}
	s.SetVar("rq_failover_done", ext)
	external := s.Var("rq_cc_dir") == "external"
	busy := disp == "USER_BUSY" || disp == "CALL_REJECTED"

	forward := func(dest, why string) bool {
		if dest == "hangup" {
			s.Log("info", why+": hangup", "ext", ext)
			if busy {
				s.Hangup("USER_BUSY")
			} else {
				s.Hangup("NO_ANSWER")
			}
			return true
		}
		if ccOwnVM(dest, ext) {
			return false // the own voicemail is the normal path
		}
		s.Log("info", why+": forward", "ext", ext, "disposition", disp, "to", dest)
		if external {
			s.SetVar("outbound_caller_id_name", s.Var("caller_id_name"))
			s.SetVar("outbound_caller_id_number", s.Var("caller_id_number"))
		}
		ccTransfer(s, dest, domain)
		return true
	}

	if busy {
		if s.Var("rq_busy_enabled") == "true" {
			forward(s.Var("rq_busy_dest"), "on-busy")
		}
		return nil
	}

	// not answered / not reachable: Call Relay first, then the No Answer Action
	if relay := s.Var("rq_relay"); relay != "" {
		t := firstNonEmpty(s.Var("rq_cc_timeout"), "30")
		for _, d := range ccSplit(relay, "|") {
			if !s.Ready() {
				return nil
			}
			s.Log("info", "call relay", "ext", ext, "to", d)
			// an extension rings only its phones (not its own forwarding or voicemail); anything
			// else (a mobile or outside number, an IVR, a queue...) goes through the dialplan
			target := "loopback/" + d + "/" + domain
			if s.Core.config().Directory.LookupIn(domain, d) != nil {
				target = "user/" + d + "@" + domain
			}
			_ = s.Execute("bridge", "[leg_timeout="+t+",rq_cc_leg=relay]"+target)
			if r := s.Var("originate_disposition"); r == "SUCCESS" || r == "ANSWER" || !s.Ready() {
				return nil
			}
		}
	}
	if s.Ready() && s.Var("rq_na_enabled") == "true" {
		forward(s.Var("rq_na_dest"), "no-answer")
	}
	return nil
}
