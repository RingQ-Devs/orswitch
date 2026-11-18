package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// The small call-feature scripts of FusionPBX / RingQ: confirm.lua, call_divert.lua,
// call_back_request.lua, dtmf_handler.lua, transfer2.lua, mkdir.lua, destination_caller_id.lua,
// cidlookup.lua, dial_string.lua, bind_digit_bleg.lua, monitor.lua, app.lua event_notify.

func init() {
	RegisterScript("confirm", scriptConfirm)
	RegisterScript("call_divert", scriptCallDivert)
	RegisterScript("call_back_request", scriptCallBackRequest)
	RegisterScript("dtmf_handler", scriptDTMFHandler)
	RegisterScript("transfer2", scriptTransfer2)
	RegisterScript("mkdir", scriptMkdir)
	RegisterScript("destination_caller_id", scriptDestinationCallerID)
	RegisterScript("cidlookup", scriptCIDLookup)
	RegisterScript("dial_string", scriptDialString)
	RegisterScript("bind_digit_bleg", scriptBindDigitBleg)
	RegisterScript("monitor", scriptMonitor)
	// app.event_notify: the vendor specific version is in script_apps.go (RegisterAppScript)
}

// ---------------------------------------------------------------------------
// confirm.lua: run on the leg that answered a bridge with group_confirm_key=exec

// groupConfirmExec runs group_confirm_file ("lua confirm.lua", or any application) on the answered leg b.
// The call is connected unless the application ended b.
func (c *Core) groupConfirmExec(ctx context.Context, b *Channel, spec string) bool {
	f := strings.Fields(spec)
	if len(f) == 0 || b == nil {
		return true
	}
	if f[0] == "lua" && len(f) > 1 {
		if _, found, err := c.runScript(ctx, b, f[1], f[2:]); !found {
			b.log.Warn("group_confirm: the script named in group_confirm_file is not installed")
			return true
		} else if err != nil {
			b.log.Warn("group_confirm script failed", "script", knownScript(f[1]), "err", err)
		}
	} else if err := c.execApp(ctx, b, f[0], strings.Join(f[1:], " "), ""); err != nil {
		b.log.Warn("group_confirm application failed", "app", c.knownApp(f[0]), "err", err)
	}
	return !b.IsHungUp() && ctx.Err() == nil
}

// confirm: when the leg's "confirm" variable is true (or the caller left a call screening recording
// /tmp/<domain>-<caller id>.<ext>), the person who answered presses 1 to accept, 2 to reject (the
// caller goes on to the next destination) or 3 for voicemail; anything else is a no answer.
func scriptConfirm(s *Script) error {
	if !s.Ready() {
		return nil
	}
	_ = s.Answer()
	confirm := s.Var("confirm")
	screen := ""
	if domain := s.Var("domain_name"); domain != "" {
		f := "/tmp/" + domain + "-" + s.Var("caller_id_number") + "." + firstNonEmpty(s.Var("record_ext"), "wav")
		if st, err := os.Stat(f); err == nil && !st.IsDir() {
			screen, confirm = f, "true"
		}
	}
	if confirm != "true" {
		return nil // accepted without asking
	}
	digit := ""
	if screen != "" {
		digit = s.GetDigits(1, 1, 1, 500, screen, `\d+`)
	}
	if digit == "" {
		digit = s.GetDigits(1, 1, 3, 5000, "ivr/ivr-accept_reject_voicemail.wav", `\d+`)
	}
	switch digit {
	case "1":
	case "2":
		s.Hangup("CALL_REJECTED")
	default: // 3 (voicemail) or no answer
		s.Hangup("NO_ANSWER")
	}
	return nil
}

// ---------------------------------------------------------------------------
// call_divert.lua, call_back_request.lua: say the caller's number to a call in progress

// playSeq plays sounds one after the other to the channels (in the background), as the queued
// uuid_broadcast commands of FreeSWITCH played them.
func (c *Core) playSeq(targets []*Channel, names []string) {
	for _, t := range targets {
		t := t
		go func() {
			s := &Script{Core: c, Ch: t, Ctx: context.Background()}
			for _, n := range names {
				if t.IsHungUp() {
					return
				}
				c.broadcast(t, s.Sound(n))
			}
		}()
	}
}

func digitSounds(number string) []string {
	var out []string
	for _, r := range number {
		if r >= '0' && r <= '9' {
			out = append(out, "digits/"+string(r)+".wav")
		}
	}
	return out
}

// call_divert <uuid>: both parties hear "you have a call" and the caller's number.
func scriptCallDivert(s *Script) error {
	uuid := s.Arg(1)
	if uuid == "" {
		return nil
	}
	ch := s.Core.Channel(uuid)
	if ch == nil {
		return nil
	}
	caller := ch.Var("caller_id_number")
	if caller == "" {
		// "you have a call" is still announced (silence first, as the Lua script queued it)
		s.Core.playSeq(bothLegs(ch), []string{"silence_stream://50", "misc/you_have_a_call.wav"})
		return nil
	}
	names := append([]string{"silence_stream://50", "misc/you_have_a_call.wav", "silence_stream://500"}, digitSounds(caller)...)
	s.Core.playSeq(bothLegs(ch), names)
	return nil
}

func bothLegs(ch *Channel) []*Channel {
	out := []*Channel{ch}
	if p := ch.Partner(); p != nil {
		out = append(out, p)
	}
	return out
}

// call_back_request <uuid> <alternative number>: the A leg hears the number to call back, then the caller id.
func scriptCallBackRequest(s *Script) error {
	uuid, alt := s.Arg(1), s.Arg(2)
	if uuid == "" {
		return nil
	}
	ch := s.Core.Channel(uuid)
	if ch == nil {
		return nil
	}
	caller := ch.Var("caller_id_number")
	if caller == "" {
		return nil
	}
	names := append(digitSounds(alt), "silence_stream://3000")
	names = append(names, digitSounds(caller)...)
	s.Core.playSeq([]*Channel{ch}, names)
	return nil
}

// ---------------------------------------------------------------------------
// dtmf_handler.lua: keeps the digits a call pressed in its dtmf_history variable
//
//	dtmf_handler <uuid> <digit>   one digit
//	dtmf_handler                  (luarun) hook mode, as lua.conf's <hook event="DTMF">: every DTMF event of
//	                              every call, until "dtmf_handler stop" or the switch stops

func dtmfRemember(c *Core, uuid, digit string) {
	ch := c.Channel(uuid)
	if ch == nil || digit == "" {
		return
	}
	_ = c.execApp(context.Background(), ch, "push", "dtmf_history="+strconv.FormatInt(time.Now().Unix(), 10)+":"+digit+"\n", "")
}

func scriptDTMFHandler(s *Script) error {
	c := s.Core
	if s.Arg(1) == "stop" {
		c.StopService("dtmf_handler")
		return nil
	}
	if s.Arg(1) != "" {
		dtmfRemember(c, s.Arg(1), s.Arg(2))
		return nil
	}
	ctx, done, ok := c.registerService("dtmf_handler", s.Ctx)
	if !ok {
		return nil
	}
	defer done()
	sub := c.events.Subscribe(1024, func(e *Event) bool { return e.Name == EvDTMF })
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-sub.C:
			dtmfRemember(c, firstNonEmpty(e.Get("Caller-Unique-ID"), e.Get("Unique-ID")), e.Get("DTMF-Digit"))
		}
	}
}

// ---------------------------------------------------------------------------
// transfer2.lua <uuid> <t1> <t2>: transfer both parties of a call
//
// t1 is where uuid goes and t2 where its bridged partner (signal_bond) goes: "::NONE::" leaves it,
// "::KILL::" hangs it up, anything else is "destination::dialplan::context".

func scriptTransfer2(s *Script) error {
	uuid, t1, t2 := s.Arg(1), s.Arg(2), s.Arg(3)
	if uuid == "" || t1 == "" || t2 == "" {
		return fmt.Errorf("transfer2: usage: transfer2 <uuid> <transfer 1> <transfer 2>")
	}
	c := s.Core
	ch := c.Channel(uuid)
	var other *Channel
	if ch != nil {
		if b := ch.Var("signal_bond"); b != "" {
			other = c.Channel(b)
		}
		if other == nil {
			other = ch.Partner()
		}
	}
	s.Log("info", "transfer2", "session", uuid, "other leg", func() string {
		if other == nil {
			return "<NONE>"
		}
		return other.UUID
	}())
	apply := func(target *Channel, t string) {
		if target == nil || t == "::NONE::" {
			return
		}
		if t == "::KILL::" {
			target.Hangup(CauseManagerRequest)
			return
		}
		parts := strings.Split(t, "::")
		ctxName := ""
		if len(parts) > 2 {
			ctxName = parts[2]
		}
		c.transfer(target, parts[0], ctxName)
	}
	if other != nil {
		apply(other, t2)
	}
	apply(ch, t1)
	return nil
}

// ---------------------------------------------------------------------------
// mkdir.lua <path>

func scriptMkdir(s *Script) error {
	p := strings.TrimSpace(s.Arg(1))
	if p == "" || strings.ContainsRune(p, 0) {
		return nil
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		s.Log("warn", "mkdir failed", "path", p, "err", err)
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// destination_caller_id.lua: the caller id of an outbound call is one of the tenant's own numbers in the
// dialled number's area code (1 + NPA)

func scriptDestinationCallerID(s *Script) error {
	number := s.Var("destination_number")
	s.Log("info", "outbound number", "number", number)
	if len(number) < 5 {
		return nil
	}
	area := number[2:5]
	s.Log("info", "area code", "area", area)
	rows := s.Rows("SELECT destination_number FROM v_destinations WHERE destination_number LIKE $1", "1"+area+"%")
	found := ""
	for _, r := range rows {
		found = r["destination_number"] // the last row wins, as in the Lua callback
	}
	if s.Ready() && found != "" {
		s.Log("info", "effective_caller_id_number", "number", found)
		return s.Execute("set", "effective_caller_id_number="+found)
	}
	return nil
}

// ---------------------------------------------------------------------------
// cidlookup.lua <uuid> [domain_uuid]: the caller's name from the tenant's contacts
//
// The name comes from v_contacts (by the number in v_contact_phones; the caller number without the local
// country prefix). Else, when the global or channel variable cidlookup_url is set (mod_cidlookup's "url"
// source: an address with ${caller_id_number}), that web service gives it as text or JSON {"name": ...}.

func scriptCIDLookup(s *Script) error {
	c := s.Core
	uuid := s.Arg(1)
	if uuid == "" {
		return nil
	}
	ch := c.Channel(uuid)
	if ch == nil {
		return nil
	}
	caller, callee := ch.Var("caller_id_number"), ch.Var("destination_number")
	domainUUID := s.Arg(2)
	cc, exit := ch.Var("default_countrycode"), ch.Var("default_exitcode")
	if cc != "" {
		if p := "+" + cc; strings.HasPrefix(caller, p) {
			caller = strings.TrimPrefix(caller, p)
		} else if exit != "" && strings.HasPrefix(caller, exit+cc) {
			caller = strings.TrimPrefix(caller, exit+cc)
		}
	}
	q := "SELECT CASE WHEN contact_name_given = '' THEN v_contacts.contact_organization ELSE v_contacts.contact_name_given || ' ' || v_contacts.contact_name_family END AS name " +
		"FROM v_contacts INNER JOIN v_contact_phones ON v_contact_phones.contact_uuid = v_contacts.contact_uuid " +
		"INNER JOIN v_destinations ON v_destinations.domain_uuid = v_contacts.domain_uuid AND v_destinations.destination_number = $1 "
	args := []any{callee}
	if domainUUID == "" {
		q += "WHERE v_contact_phones.phone_number = $2"
		args = append(args, caller)
	} else {
		q += "WHERE v_contacts.domain_uuid = $2::uuid AND v_contact_phones.phone_number = $3"
		args = append(args, domainUUID, caller)
	}
	name := ""
	for _, r := range s.Rows(q, args...) {
		name = r["name"]
	}
	if name == "" {
		s.Log("info", "caller name from contacts db is nil")
		name = httpCIDLookup(s, ch, caller)
	} else {
		s.Log("info", "caller name from contacts db", "name", name)
	}
	if name != "" {
		ch.SetVar("ignore_display_updates", "false")
		ch.SetVar("caller_id_name", name)
		ch.SetVar("effective_caller_id_name", name)
	}
	return nil
}

func httpCIDLookup(s *Script, ch *Channel, number string) string {
	gv, _ := s.Core.globals.get("cidlookup_url")
	u := firstNonEmpty(ch.Var("cidlookup_url"), gv)
	if u == "" || number == "" {
		return ""
	}
	u = strings.ReplaceAll(u, "${caller_id_number}", url.QueryEscape(number))
	ctx, cancel := context.WithTimeout(s.Ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.Log("warn", "cidlookup url failed", "err", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if n, ok := m["name"].(string); ok {
				return strings.TrimSpace(n)
			}
			return ""
		}
	}
	if len(text) > 100 || strings.ContainsAny(text, "\r\n<") {
		return ""
	}
	return text
}

// ---------------------------------------------------------------------------
// dial_string.lua: set, clear or toggle the "dial string" of an extension from a phone
//
// The caller enters the extension's id (or number), then its PIN (the call's pin_number, else the
// extension's voicemail password); direction=in saves {sip_invite_domain=...}${sofia_contact(...)} of the
// calling phone as the extension's dial string, direction=out clears it, direction=both toggles it.

func scriptDialString(s *Script) error {
	if !s.Ready() {
		return nil
	}
	_ = s.Answer()
	domainUUID, pin := s.Var("domain_uuid"), s.Var("pin_number")
	fromUser, fromHost := s.Var("sip_from_user"), s.Var("sip_from_host")
	direction, isExt := s.Var("direction"), s.Var("extension")
	dialString := s.Var("dial_string")

	uniqueID := s.GetDigits(1, 15, 3, 5000, "phrase:voicemail_enter_id:#", `\d+`)
	if dialString != "" {
		dialString = strings.ReplaceAll(dialString, "{v_unique_id}", uniqueID)
	}
	minD, maxD := 1, 12
	if pin != "" {
		minD, maxD = len(pin), len(pin)+1
	}
	callerPIN := s.GetDigits(minD, maxD, 3, 5000, "phrase:voicemail_enter_pass:#", `\d+`)

	extUUID, curDial := "", ""
	var rowDomain, rowExt, rowAlias string
	if uniqueID != "" {
		var row map[string]string
		if isExt == "true" {
			row = s.Row("SELECT e.* FROM v_extensions AS e, v_domains AS d WHERE e.domain_uuid = d.domain_uuid AND e.extension = $1 AND e.domain_uuid = $2::uuid", uniqueID, domainUUID)
		} else {
			row = s.Row("SELECT e.* FROM v_extensions AS e, v_domains AS d WHERE e.domain_uuid = d.domain_uuid AND e.unique_id = $1", uniqueID)
		}
		if row != nil {
			extUUID, curDial = row["extension_uuid"], row["dial_string"]
			rowDomain, rowExt, rowAlias = row["domain_uuid"], row["extension"], row["number_alias"]
		}
	}
	if pin != "" {
		if pin != callerPIN {
			extUUID = ""
		}
	} else if rowDomain != "" {
		id := rowExt
		if _, err := strconv.Atoi(rowExt); err != nil {
			id = rowAlias
		}
		vm := s.Row("SELECT voicemail_password FROM v_voicemails WHERE domain_uuid = $1::uuid AND voicemail_id = $2", rowDomain, id)
		if vm == nil || vm["voicemail_password"] != callerPIN {
			extUUID = ""
		}
	}
	if extUUID == "" {
		s.PlaySound("phrase:voicemail_fail_auth:#")
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	set := func() {
		ds := dialString
		if ds == "" {
			ds = "{sip_invite_domain=" + fromHost + ",presence_id=" + fromUser + "@" + fromHost + "}${sofia_contact(" + fromUser + "@" + fromHost + ")}"
		}
		s.Exec("UPDATE v_extensions SET dial_string = $1::text, dial_user = $2::text, dial_domain = $3::text WHERE extension_uuid = $4::uuid", ds, fromUser, fromHost, extUUID)
		s.PlaySound("voicemail/vm-saved.wav")
	}
	clear := func() {
		s.Exec("UPDATE v_extensions SET dial_string = null, dial_user = null, dial_domain = null WHERE extension_uuid = $1::uuid", extUUID)
		s.PlaySound("voicemail/vm-deleted.wav")
	}
	switch direction {
	case "in":
		set()
	case "out":
		clear()
	case "both":
		if len(curDial) > 1 {
			clear()
		} else {
			set()
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// bind_digit_bleg.lua <context>: the transfer keys of the called party (*1 dx, *3 cf, *4 att_xfer)

func scriptBindDigitBleg(s *Script) error {
	if !s.Ready() {
		return nil
	}
	ctxName := s.Arg(1)
	for _, d := range [][2]string{{"*1", "dx"}, {"*3", "cf"}, {"*4", "att_xfer"}} {
		_ = s.Execute("bind_digit_action", "local,"+d[0]+",exec:execute_extension,"+d[1]+" XML "+ctxName+",self,self")
	}
	_ = s.Execute("digit_action_set_realm", "local")
	return nil
}

// ---------------------------------------------------------------------------
// monitor.lua: restarts a SIP profile that is not running (every 3 s), until "monitor stop"

func scriptMonitor(s *Script) error {
	c := s.Core
	if s.Arg(1) == "stop" {
		c.StopService("monitor")
		return nil
	}
	ctx, done, ok := c.registerService("monitor", s.Ctx)
	if !ok {
		return nil
	}
	defer done()
	for {
		for _, p := range []string{"internal", "external"} {
			out := strings.TrimSpace(c.API("sofia status profile " + p))
			if strings.Contains(out, "Invalid Profile!") {
				s.Log("warn", "sip profile is not running; starting it", "profile", p)
				_ = c.API("sofia profile " + p + " start")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// luarun app.lua event_notify <profile> reboot|check_sync <user@domain> [vendor]

func scriptEventNotify(s *Script) error {
	user, domain, _ := strings.Cut(s.Arg(3), "@")
	if user == "" {
		return fmt.Errorf("event_notify: usage: <profile> reboot|check_sync <user@domain> [vendor]")
	}
	n := s.Core.CheckSync(user, domain, s.Arg(2) == "reboot")
	s.Write(fmt.Sprintf("+OK sent to %d phone(s)\n", n))
	return nil
}

func init() {
	// the mkdir application (it was accepted and ignored)
	builtinAppsExtra["mkdir"] = func(_ context.Context, _ *Core, ch *Channel, data string) error {
		if p := strings.TrimSpace(data); p != "" && !strings.ContainsRune(p, 0) {
			return os.MkdirAll(p, 0o755)
		}
		return nil
	}
}
