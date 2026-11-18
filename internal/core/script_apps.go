package core

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// FusionPBX's app/<name>/index.lua scripts, started as "app.lua <name> [args]" (Arg(1) is the
// name, as argv[1]); the Go versions are registered with RegisterAppScript. Where RingQswitch
// already does the job natively the report (REPORT.md) says so; the scripts here are the ones with
// no native equivalent:
//
//	agent_status    *22 / *23: an agent logs in, out or on break by id and password
//	avmd            hangs up a call whose answering machine beep was detected (service)
//	call_block      blocks calls by number (v_call_block): busy, hold, reject, extension, ivr,
//	                ring group or voicemail
//	call_control    *33: enable or disable calling for a call group (v_extensions.user_context)
//	caller_id       sets the caller id name from the contacts (v_contact_phones)
//	dialplan        sets call_direction
//	emergency       911 / 922 / 933: mails the emergency contacts and logs the call
//	emergency_calls the last emergency calls, read to the caller
//	emergency_notify mails one address that an emergency call was made
//	event_notify    check-sync / reboot a user's phones (vendor specific events)
//	hangup          missed call mail (api_hangup_hook)
//	is_local        outbound number that is one of our own inbound destinations: route it inside
//	speed_dial      speed dial numbers of the contacts (v_contact_phones.phone_speed_dial)
//	toll_allow      bars calls by toll type (extension toll_allow)
//	valet_park      parks the call in the first free valet slot (*5901 - *5999)

func init() {
	RegisterAppScript("agent_status", appScriptAgentStatus)
	RegisterAppScript("avmd", appScriptAVMD)
	RegisterAppScript("call_block", appScriptCallBlock)
	RegisterAppScript("call_control", appScriptCallControl)
	RegisterAppScript("caller_id", appScriptCallerID)
	RegisterAppScript("dialplan", appScriptDialplan)
	RegisterAppScript("emergency", appScriptEmergency)
	RegisterAppScript("emergency_notify", appScriptEmergencyNotify)
	RegisterAppScript("event_notify", appScriptEventNotify)
	RegisterAppScript("hangup", appScriptHangup)
	RegisterAppScript("is_local", appScriptIsLocal)
	RegisterAppScript("speed_dial", appScriptSpeedDial)
	RegisterAppScript("toll_allow", appScriptTollAllow)
	RegisterAppScript("valet_park", appScriptValetPark)
	RegisterScript("emergency_calls", scriptEmergencyCalls)
}

// ---------------------------------------------------------------------------
// agent_status

func appScriptAgentStatus(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	s.Sleep(1000)
	if !s.Ready() {
		return nil
	}
	domainUUID := s.DomainUUID("")
	authorized := s.Var("agent_authorized") == "true"
	action := s.Var("agent_action")
	agentID := s.Var("agent_id")
	agentName := s.Var("agent_name")
	password := s.Var("agent_password")
	const timeout = 5000 * time.Millisecond
	if agentID == "" && agentName == "" {
		agentID = s.PlayAndGetDigits(2, 20, 3, timeout, "#", "phrase:voicemail_enter_id:#", "", `\d+`)
	}
	if password == "" && !authorized {
		password = s.PlayAndGetDigits(3, 20, 3, timeout, "#", "phrase:voicemail_enter_pass:#", "", `\d+`)
	}
	q := `SELECT call_center_agent_uuid::text AS u, agent_name, agent_id, coalesce(user_uuid::text, '') AS user_uuid
		FROM v_call_center_agents WHERE domain_uuid = $1::uuid`
	args := []any{domainUUID}
	if agentID != "" {
		q += " AND (agent_id = $2 OR agent_name = $2)"
		args = append(args, agentID)
	} else {
		q += " AND agent_name = $2"
		args = append(args, agentName)
	}
	if !authorized {
		q += " AND agent_password = $3"
		args = append(args, password)
	}
	var agentUUID, userUUID string
	if rows, err := c.sqlRows(s.Ctx, q, args...); err == nil && len(rows) > 0 {
		agentUUID, agentName, agentID, userUUID = rows[0]["u"], rows[0]["agent_name"], rows[0]["agent_id"], rows[0]["user_uuid"]
		authorized = true
	} else if err != nil {
		s.Log("warn", "agents not read", "err", err)
	}
	if agentID != "" {
		s.Log("info", "agent login", "id", agentID, "authorized", authorized)
	}
	if !authorized || agentUUID == "" {
		s.Stream("voicemail/vm-fail_auth.wav")
		if s.Ready() {
			s.Sleep(2000)
		}
		return nil
	}
	cur := strings.TrimSpace(c.API("callcenter_config agent get status " + agentUUID))
	var do, status string
	switch action {
	case "break":
		if cur == "On Break" {
			do, status = "login", "Available"
		} else {
			do, status = "break", "On Break"
		}
	case "login":
		do, status = "login", "Available"
	case "logout":
		do, status = "logout", "Logged Out"
	default:
		if cur == "Available" {
			do, status = "logout", "Logged Out"
		} else {
			do, status = "login", "Available"
		}
	}
	// five seconds later, so the confirmation is heard first
	c.API("sched_api +5 none callcenter_config agent set status " + agentUUID + " '" + status + "'")
	if userUUID != "" {
		if _, err := c.sqlExec(s.Ctx, `UPDATE v_users SET user_status = $1 WHERE user_uuid = $2::uuid AND domain_uuid = $3::uuid`, status, userUUID, domainUUID); err != nil {
			s.Log("warn", "user status not saved", "err", err)
		}
	}
	if s.Ready() {
		switch do {
		case "login":
			s.Stream("ivr/ivr-you_are_now_logged_in.wav")
		case "logout":
			s.Stream("ivr/ivr-you_are_now_logged_out.wav")
		case "break":
			s.Stream("ivr/ivr-thank_you.wav")
		}
		s.Sleep(2000)
	}
	return nil
}

// ---------------------------------------------------------------------------
// avmd

// appScriptAVMD (luarun app.lua avmd): hangs up the calls the answering machine detection
// (CUSTOM avmd::beep) reports.
func appScriptAVMD(s *Script) error {
	c := s.Core
	sub := c.events.Subscribe(64, func(e *Event) bool { return e.Name == EvCustom && e.Subclass() == "avmd::beep" })
	defer sub.Close()
	for {
		select {
		case e := <-sub.C:
			uuid := e.Get("Unique-ID")
			s.Log("info", "avmd beep", "uuid", uuid)
			if uuid != "" {
				c.API("uuid_kill " + uuid + " NORMAL_CLEARING")
			}
		case <-c.stop:
			return nil
		case <-s.Ctx.Done():
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// call_block

func appScriptCallBlock(s *Script) error {
	if !s.Ready() || s.Var("call_block") != "" {
		return nil
	}
	c := s.Core
	domainUUID := firstNonEmpty(s.Var("domain_uuid"), c.domainUUID(s.Ctx, s.Var("domain_name")))
	direction := s.Var("call_direction")
	name, number := s.Var("caller_id_name"), s.Var("caller_id_number")
	if direction == "outbound" {
		number = s.Var("destination_number")
	} else if direction != "inbound" {
		return nil
	}
	ctxName, ext := s.Var("context"), s.Var("extension_uuid")
	q := `SELECT call_block_uuid::text AS u, coalesce(call_block_app, '') AS app, coalesce(call_block_data, '') AS data,
		coalesce(call_block_count, 0)::text AS cnt FROM v_call_block
		WHERE (domain_uuid = $1::uuid OR domain_uuid IS NULL) AND call_block_enabled = 'true' AND call_block_direction = $2
		AND ((call_block_name = $3 AND ('+' || call_block_country_code || call_block_number = $4 OR call_block_country_code || call_block_number = $4 OR call_block_number = $4))
		  OR (call_block_name IS NULL AND ('+' || call_block_country_code || call_block_number = $4 OR call_block_country_code || call_block_number = $4 OR call_block_number = $4))
		  OR (call_block_name = $3 AND call_block_number IS NULL))`
	args := []any{domainUUID, direction, name, number}
	if ext == "" {
		q += " AND extension_uuid IS NULL"
	} else {
		q += " AND (extension_uuid IS NULL OR extension_uuid = $5::uuid)"
		args = append(args, ext)
	}
	rows, err := c.sqlRows(s.Ctx, q, args...)
	if err != nil {
		s.Log("warn", "call block not read", "err", err)
		return nil
	}
	if len(rows) == 0 || rows[0]["app"] == "" {
		return nil
	}
	r := rows[0]
	app, data, id := r["app"], r["data"], r["u"]
	_ = s.Execute("set", "call_block=true")
	set := func() {
		_ = s.Execute("set", "call_block_uuid="+id)
		_ = s.Execute("set", "call_block_app="+app)
	}
	s.Log("info", "call blocked", "number", number, "action", app)
	switch app {
	case "busy":
		_ = s.Execute("respond", "486")
		set()
	case "reject":
		_ = s.Execute("respond", "600")
		set()
	case "hold":
		set()
		_ = s.Execute("playback", s.Var("hold_music"))
	case "extension", "ivr", "ring_group":
		if data != "" {
			set()
			_ = s.Execute("set", "call_block_data="+data)
			_ = s.Transfer(data, ctxName)
		}
	case "voicemail":
		if data != "" {
			set()
			_ = s.Execute("set", "call_block_data="+data)
			_ = s.Transfer("*99"+data, ctxName)
		}
	}
	n, _ := strconv.Atoi(r["cnt"])
	if _, err := c.sqlExec(context.Background(), `UPDATE v_call_block SET call_block_count = $1 WHERE call_block_uuid = $2::uuid`, n+1, id); err != nil {
		s.Log("warn", "block count not saved", "err", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// call_control (*33)

func appScriptCallControl(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	group := s.Var("target_group")
	prompt := disaDefault(s.Var("audio_prompt"), "phrase:agent_status:#")
	pins := s.Var("pin_number")
	domain, domainUUID := s.Var("domain_name"), s.Var("domain_uuid")
	enabled := disaDefault(s.Var("context_enabled"), "domain")
	disabled := disaDefault(s.Var("context_disabled"), "limit")
	if enabled == "domain" || enabled == "domain_name" {
		enabled = domain
	}
	if pins != "" {
		s.Sleep(500)
		digits := s.PlayAndGetDigits(2, 20, 3, 3000*time.Millisecond, "#", "phrase:voicemail_enter_pass:#", "", `\d+`)
		ok := false
		for _, p := range strings.Split(pins, ",") {
			if digits != "" && digits == strings.TrimSpace(p) {
				ok = true
				s.SetVar("pin_number", strings.TrimSpace(p))
				break
			}
		}
		if !ok {
			s.Stream("phrase:voicemail_fail_auth:#")
			s.Hangup("NORMAL_CLEARING")
			return nil
		}
	}
	pressed := s.PlayAndGetDigits(1, 1, 1, 3000*time.Millisecond, "#", prompt, "", `\d+`)
	if !s.Ready() {
		return nil
	}
	var userContext string
	switch pressed {
	case "1":
		userContext = enabled
		s.SetVar("call_control", "enabled")
		s.SetVar("call_control_status", "enabled")
	case "2":
		userContext = disabled
		s.SetVar("call_control_status", "disabled")
	default:
		return nil
	}
	s.SetVar("call_control_context", userContext)
	s.SetVar("call_control_group", group)
	s.Log("info", "call control", "context", userContext, "group", group)
	if _, err := c.sqlExec(s.Ctx, `UPDATE v_extensions SET user_context = $1 WHERE domain_uuid = $2::uuid AND call_group = $3`,
		userContext, domainUUID, group); err != nil {
		s.Log("warn", "call control not saved", "err", err)
		return err
	}
	c.requestReload() // the directory is read again (the Lua removed the cached directory files)
	s.Sleep(1000)
	s.Sleep(2000)
	return nil
}

// ---------------------------------------------------------------------------
// caller_id

func appScriptCallerID(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	domainUUID, num := s.DomainUUID(""), s.Var("caller_id_number")
	rows, err := c.sqlRows(s.Ctx, `SELECT coalesce(contact_organization, '') AS org, coalesce(contact_name_family, '') AS fam,
		coalesce(contact_name_given, '') AS giv FROM v_contacts WHERE contact_uuid IN (
		SELECT contact_uuid FROM v_contact_phones WHERE domain_uuid = $1::uuid AND (phone_number = $2
		OR phone_country_code || phone_number = $2 OR '+' || phone_country_code || phone_number = $2))`, domainUUID, num)
	if err != nil {
		s.Log("warn", "contacts not read", "err", err)
		return nil
	}
	name := ""
	if len(rows) > 0 { // the last row wins, as in the Lua
		r := rows[len(rows)-1]
		name = r["org"]
		if name == "" {
			name = strings.TrimSpace(r["fam"] + " " + r["giv"])
		}
	}
	if name != "" {
		_ = s.Execute("set", "effective_caller_id_name="+name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// dialplan

// appScriptDialplan sets call_direction (inbound for the public context, outbound for
// outbound@... or long numbers, else local) when the call has none. The before/after hook scripts
// of the app (app/dialplan/resources/*, all ".noload" on the reference server) are not run.
func appScriptDialplan(s *Script) error {
	if !s.Ready() || s.Var("call_direction") != "" {
		return nil
	}
	ctxName, dest := s.Var("context"), s.Var("destination_number")
	dir := "local"
	switch {
	case ctxName == "public":
		dir = "inbound"
	case strings.HasPrefix(ctxName, "outbound@"), len(dest) > 6:
		dir = "outbound"
	}
	s.SetVar("call_direction", dir)
	return nil
}

// ---------------------------------------------------------------------------
// emergency

func scriptEmergencyEvent(dest string) string {
	switch n, _ := strconv.Atoi(dest); n {
	case 933:
		return "933 Emergency Address Validation Service"
	case 922:
		return "922 Emergency Address Validation Service"
	case 911:
		return "911 Emergency Call"
	}
	return ""
}

// compactBody is what the Lua mail scripts did to a template body: new lines and tabs go, spaces stay.
func compactBody(body string) string {
	body = strings.ReplaceAll(body, " ", "\x00")
	body = strings.Join(strings.Fields(body), "")
	return strings.TrimSpace(strings.ReplaceAll(body, "\x00", " "))
}

func appScriptEmergency(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	domainUUID := s.DomainUUID("")
	cidName, cidNum := s.Var("caller_id_name"), s.Var("caller_id_number")
	ecName := firstNonEmpty(s.Var("emergency_caller_id_name"), s.Var("outbound_caller_id_name"))
	ecNum := firstNonEmpty(s.Var("emergency_caller_id_number"), s.Var("outbound_caller_id_number"))
	event := scriptEmergencyEvent(s.Var("destination_number"))
	var to []string
	if rows, err := c.sqlRows(s.Ctx, `SELECT default_setting_value AS v FROM v_default_settings WHERE default_setting_category = 'emergency'
		AND default_setting_subcategory = 'email_address' AND default_setting_enabled::text IN ('true', 't') LIMIT 5`); err == nil {
		for _, r := range rows {
			to = append(to, r["v"])
		}
	}
	if rows, err := c.sqlRows(s.Ctx, `SELECT domain_setting_value AS v FROM v_domain_settings WHERE domain_uuid = $1::uuid
		AND domain_setting_category = 'emergency' AND domain_setting_subcategory = 'email_address'
		AND domain_setting_enabled::text IN ('true', 't') LIMIT 5`, domainUUID); err == nil {
		for _, r := range rows {
			to = append(to, r["v"])
		}
	}
	from := c.scriptSetting(s.Ctx, "", "email", "smtp_from", "text")
	if len(to) > 0 {
		_, body, _ := c.emailTemplate(s.Ctx, domainUUID, "", "plugin", "emergency")
		if body != "" {
			body = compactBody(fillTemplate(body, map[string]string{
				"caller_id_name": cidName, "caller_id_number": cidNum, "emergency_caller_id_name": ecName,
				"emergency_caller_id_number": ecNum, "sip_from_user": s.Var("sip_from_user"),
				"message_date": time.Now().Format("2006-01-02 15:04:05"), "event": event}))
		}
		for _, addr := range to {
			if err := c.QueueEmail(s.Ctx, EmailMessage{DomainUUID: domainUUID, Type: "app", CallUUID: s.UUID(), From: from, To: addr, Subject: event, Body: body}); err != nil {
				s.Log("warn", "emergency mail not queued", "to", addr, "err", err)
			}
		}
	}
	if _, err := c.sqlExec(s.Ctx, `INSERT INTO v_emergency_logs (emergency_log_uuid, domain_uuid, insert_date, extension, event)
		VALUES ($1::uuid, $2::uuid, now(), $3::numeric, $4)`, s.UUID(), domainUUID, digitsOnly(cidNum), event); err != nil {
		s.Log("warn", "emergency log not written", "err", err)
	}
	return nil
}

func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// scriptEmergencyCalls (app/emergency/resources/scripts/emergency_calls.lua): reads out the last
// ten emergency calls of the domain: the extension, "has called emergency services", and after
// each * ends the call. (The date and time are not spoken: no number-to-speech yet.)
func scriptEmergencyCalls(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	s.Sleep(1000)
	rows, err := c.sqlRows(s.Ctx, `SELECT extension::text AS ext FROM v_emergency_logs WHERE domain_uuid = $1::uuid ORDER BY insert_date DESC LIMIT 10`,
		s.DomainUUID(""))
	if err != nil {
		s.Log("warn", "emergency log not read", "err", err)
	}
	for _, r := range rows {
		if !s.Ready() {
			return nil
		}
		s.SayDigits(r["ext"])
		s.Stream("ivr/ivr-has_called_emergency_services.wav")
		d := s.PlayAndGetDigits(1, 20, 1, 100*time.Millisecond, "#", "phrase:pin_number_start:#", "", `\d+`)
		if d == "" {
			d = s.PlayAndGetDigits(1, 1, 1, 100*time.Millisecond, "#", "voicemail/vm-press.wav", "", `\d+|\*`)
		}
		if d == "" {
			d = s.PlayAndGetDigits(1, 1, 1, 10*time.Second, "#", "digits/1.wav", "", `\d+|\*`)
		}
		if d == "*" {
			break
		}
	}
	s.Stream("phrase:voicemail_goodbye")
	s.Hangup("")
	return nil
}

// appScriptEmergencyNotify: app.lua emergency_notify <to> <from>: "<name> <number> made an
// emergency call" to one address.
func appScriptEmergencyNotify(s *Script) error {
	if !s.Ready() {
		return nil
	}
	to, from := s.Arg(2), s.Arg(3)
	vals := map[string]string{
		"caller_id_name": htmlQuote(s.Var("outbound_caller_id_name")), "caller_id_number": htmlQuote(s.Var("caller_id_number")),
		"sip_to_user": s.Var("sip_to_user"), "caller_destination": s.Var("caller_destination")}
	subject := mimeSubject(fillTemplate("${caller_id_name} <${caller_id_number}> Made An Emergency Call", vals))
	body := compactBody(fillTemplate("${caller_id_name} &lt;${caller_id_number}&gt; made an emergency call to ${sip_to_user}", vals))
	return s.Core.QueueEmail(s.Ctx, EmailMessage{DomainUUID: s.Var("domain_uuid"), Type: "emergency_call", CallUUID: s.UUID(), From: from, To: to, Subject: subject, Body: body})
}

// ---------------------------------------------------------------------------
// event_notify

// notifyEvents is the Event header each phone vendor wants for a reboot and for a re-provision
// (check_sync): [reboot, check_sync].
var notifyEvents = map[string][2]string{
	"aastra":      {"check-sync;reboot=true", "check-sync;reboot=true"},
	"cisco":       {"check-sync", "check-sync"},
	"cisco-spa":   {"reboot=true", "reboot=true"},
	"digium":      {"check-sync", "check-sync"},
	"fanvil":      {"check-sync;reboot=true", "resync"},
	"grandstream": {"check-sync;reboot=true", "resync"},
	"htek":        {"check-sync;reboot=true", "resync"},
	"sangoma":     {"check-sync;reboot=true", "resync"},
	"linksys":     {"reboot=true", "reboot=true"},
	"panasonic":   {"check-sync;reboot=true", "check-sync;reboot=true"},
	"polycom":     {"check-sync", "check-sync"},
	"snom":        {"check-sync;reboot=true", "check-sync;reboot=false"},
	"yealink":     {"check-sync;reboot=true", "check-sync;reboot=false"},
}

// app.lua event_notify <profile> <reboot|check_sync> <user@domain> [vendor]: asks every phone
// registered for the user to re-provision or reboot, with the NOTIFY event the vendor uses.
// Without a (known) vendor the generic check-sync is sent (with reboot=true for a reboot).
func appScriptEventNotify(s *Script) error {
	command, account, vendor := s.Arg(3), s.Arg(4), strings.ToLower(s.Arg(5))
	user, domain, _ := strings.Cut(account, "@")
	if user == "" {
		return fmt.Errorf("event_notify: usage: app.lua event_notify <profile> <reboot|check_sync> <user@domain> [vendor]")
	}
	c := s.Core
	n := 0
	if ev, ok := notifyEvents[vendor]; ok && (command == "reboot" || command == "check_sync") {
		event := ev[0]
		if command == "check_sync" {
			event = ev[1]
		}
		for _, b := range c.reg.Lookup(user, domain) {
			go c.notify(b, event, "", nil)
			n++
		}
	} else {
		n = c.CheckSync(user, domain, command == "reboot")
	}
	s.Log("info", "event_notify", "command", command, "user", account, "vendor", vendor, "phones", n)
	s.Write(fmt.Sprintf("+OK sent to %d phone(s)\n", n))
	return nil
}

// ---------------------------------------------------------------------------
// hangup (missed call mail)

const (
	missedSubject = "Missed Call from ${caller_id_name} <${caller_id_number}>"
	missedBody    = "Missed Call from ${caller_id_name} &lt;${caller_id_number}&gt; to ${sip_to_user} ext ${dialed_user}"
)

// appScriptHangup (api_hangup_hook=lua app.lua hangup): a call the caller gave up on
// (originate_disposition ORIGINATOR_CANCEL) with missed_call_app=email is mailed to missed_call_data.
func appScriptHangup(s *Script) error {
	if s.Header("originate_disposition") != "ORIGINATOR_CANCEL" {
		return nil
	}
	app, data := s.Header("missed_call_app"), s.Header("missed_call_data")
	if app != "email" || data == "" {
		return nil
	}
	c := s.Core
	domainUUID := s.Header("domain_uuid")
	name, num := s.Header("caller_id_name"), s.Header("caller_id_number")
	if name == "" {
		name = s.Header("Caller-Caller-ID-Name")
	}
	if num == "" {
		num = s.Header("Caller-Caller-ID-Number")
	}
	if s.Header("call_direction") == "local" {
		name = s.Header("effective_caller_id_name")
	}
	lang := disaDefault(s.Header("default_language"), "en") + "-" + disaDefault(s.Header("default_dialect"), "us")
	subject, body, ok := c.emailTemplate(s.Ctx, domainUUID, lang, "missed", "")
	if !ok {
		subject, body = missedSubject, missedBody
	}
	vals := map[string]string{"caller_id_name": htmlQuote(name), "caller_id_number": htmlQuote(num),
		"sip_to_user": s.Header("sip_to_user"), "dialed_user": s.Header("dialed_user")}
	m := EmailMessage{DomainUUID: domainUUID, Type: "missed", CallUUID: s.Header("uuid"),
		From: c.scriptSetting(s.Ctx, domainUUID, "email", "smtp_from", "text"), To: data,
		Subject: mimeSubject(fillTemplate(subject, vals)), Body: compactBody(fillTemplate(body, vals))}
	return c.QueueEmail(s.Ctx, m)
}

// ---------------------------------------------------------------------------
// is_local, speed_dial

func appScriptIsLocal(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	dest := s.Var("destination_number")
	rows, err := c.sqlRows(s.Ctx, `SELECT destination_number AS n, destination_context AS ctx FROM v_destinations
		WHERE (destination_prefix || destination_area_code || destination_number = $1
		OR destination_trunk_prefix || destination_area_code || destination_number = $1
		OR destination_prefix || destination_number = $1 OR '+' || destination_prefix || destination_number = $1
		OR '+' || destination_prefix || destination_area_code || destination_number = $1
		OR destination_area_code || destination_number = $1 OR destination_number = $1)
		AND destination_type = 'inbound' AND destination_enabled = 'true'`, dest)
	if err != nil {
		s.Log("warn", "destinations not read", "err", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	r := rows[0]
	if v := s.Var("outbound_caller_id_name"); v != "" {
		_ = s.Execute("set", "caller_id_name="+v)
		_ = s.Execute("set", "effective_caller_id_name="+v)
	}
	if v := s.Var("outbound_caller_id_number"); v != "" {
		_ = s.Execute("set", "caller_id_number="+v)
		_ = s.Execute("set", "effective_caller_id_number="+v)
	}
	s.Log("info", "is_local", "destination", r["n"], "context", r["ctx"])
	return s.Transfer(r["n"], r["ctx"])
}

func appScriptSpeedDial(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	dest := s.Arg(2)
	domain, domainUUID, ctxName := s.Var("domain_name"), s.Var("domain_uuid"), s.Var("context")
	user := firstNonEmpty(s.Var("sip_auth_username"), s.Var("username"))
	var rows []map[string]string
	var err error
	if s.Var("permissions") == "true" {
		rows, err = c.sqlRows(s.Ctx, `SELECT t0.phone_number AS n FROM v_contact_phones t0
			INNER JOIN v_contacts t1 ON t0.contact_uuid = t1.contact_uuid
			LEFT OUTER JOIN v_contact_groups t2 ON t1.contact_uuid = t2.contact_uuid
			LEFT OUTER JOIN v_user_groups t3 ON t2.group_uuid = t3.group_uuid
			LEFT OUTER JOIN v_users t4 ON t3.user_uuid = t4.user_uuid
			LEFT OUTER JOIN v_extension_users t5 ON t4.user_uuid = t5.user_uuid
			LEFT OUTER JOIN v_extensions t6 ON t5.extension_uuid = t6.extension_uuid
			WHERE t0.domain_uuid = $1::uuid AND t0.phone_speed_dial = $2
			AND ((1 = 0) OR (t6.domain_uuid = $1::uuid AND (t6.extension = $3 OR t6.number_alias = $3))
			OR (t2.contact_uuid IS NULL AND NOT EXISTS (SELECT 1 FROM v_contact_users t WHERE t.contact_uuid = t0.contact_uuid)))
			UNION
			SELECT t0.phone_number AS n FROM v_contact_phones t0
			INNER JOIN v_contacts t1 ON t0.contact_uuid = t1.contact_uuid
			LEFT OUTER JOIN v_contact_users t2 ON t1.contact_uuid = t2.contact_uuid
			LEFT OUTER JOIN v_users t3 ON t2.user_uuid = t3.user_uuid
			LEFT OUTER JOIN v_extension_users t4 ON t3.user_uuid = t4.user_uuid
			LEFT OUTER JOIN v_extensions t5 ON t4.extension_uuid = t5.extension_uuid
			WHERE t0.domain_uuid = $1::uuid AND t0.phone_speed_dial = $2
			AND ((1 = 0) OR (t5.domain_uuid = $1::uuid AND (t5.extension = $3 OR t5.number_alias = $3))
			OR (t2.contact_user_uuid IS NULL AND NOT EXISTS (SELECT 1 FROM v_contact_groups t WHERE t.contact_uuid = t0.contact_uuid)))`,
			domainUUID, dest, user)
	} else if s.Var("global") == "true" {
		rows, err = c.sqlRows(s.Ctx, `SELECT phone_number AS n FROM v_contact_phones WHERE phone_speed_dial = $1 LIMIT 1`, dest)
	} else {
		rows, err = c.sqlRows(s.Ctx, `SELECT phone_number AS n FROM v_contact_phones WHERE domain_uuid = $1::uuid AND phone_speed_dial = $2 LIMIT 1`, domainUUID, dest)
	}
	if err != nil {
		s.Log("warn", "speed dials not read", "err", err)
		return nil
	}
	if len(rows) == 0 || rows[0]["n"] == "" {
		s.Log("warn", "can not find number in domain", "number", dest, "domain", domain)
		return nil
	}
	s.Log("info", "speed dial", "code", dest, "number", rows[0]["n"])
	return s.Transfer(rows[0]["n"], ctxName)
}

// ---------------------------------------------------------------------------
// toll_allow

// tollTemplates: the number plans of toll_allow.lua per country, as regular expressions (the Lua
// patterns' %d is \d). Each is [toll type, alternatives].
var tollTemplates = map[string][][2]string{
	"IT": {
		{"mobile", `[35]\d\d\d\d\d\d+`}, {"landline", `0[123456789]\d+`}, {"international", `00\d+`},
		{"tollfree", `119|1[3456789]\d|19[24]\d|192[01]\d\d|800\d\d\d\d\d+|803\d\d\d+|456\d\d\d\d\d\d+|11[2345678]|15\d\d|116\d\d\d|196\d\d`},
		{"sharedcharge", `84[0178]\d\d\d\d+|199\d\d\d\d\d+|178\d\d\d\d\d+|12\d\d|10\d\d\d+|1482|149\d+|4[012]\d+|70\d\d\d\d\d+`},
		{"premium", `89[2459]\d\d\d+|16[456]\d\d\d+|144\d\d\d+|4[346789]\d\d+`}, {"unknown", `\d\d+`}},
	"FR": {
		{"mobile", `0[67]\d\d\d\d\d\d\d\d`}, {"landline", `0[1234589]\d\d\d\d\d\d\d\d`}, {"international", `00\d+`},
		{"tollfree", `15|17|18|112|114|115|116\d\d\d|118\d\d\d|119|19[16]|1[06]\d\d|080\d+`},
		{"sharedcharge", `081\d+|082[0156]\d+|0884\d+|089[0123789]\d+`}, {"premium", `\d\d+`}, {"unknown", `\d\d+`}},
	"US": {{"unknown", `\d+`}},
	"RU": {
		{"international", `810\d+|8[89]40\d+|87\d\d\d\d\d\d\d\d\d`}, {"mobile", `89\d\d\d\d\d\d\d\d\d`},
		{"tollfree", `8800\d\d\d\d\d\d\d|10[1-9]`}, {"landline", `8[3-68]\d\d\d\d\d\d\d\d\d`}, {"unknown", ``}},
	"ZA": {
		{"international", `00\d+`}, {"mobile", `0[6-8]\d\d\d\d\d\d\d\d`}, {"landline", `0[1-5]\d\d\d\d\d\d\d\d`}, {"unknown", ``}},
}

var tollKnown = map[string]bool{"mobile": true, "landline": true, "international": true, "tollfree": true, "sharedcharge": true, "premium": true, "unknown": true}

// tollType is the first template whose pattern (prefix + alternative) matches the whole number.
func tollType(prefix string, templates [][2]string, called string) string {
	for _, t := range templates {
		for _, part := range strings.Split(t[1], "|") {
			if re, err := regexp.Compile("^" + regexp.QuoteMeta(prefix) + "(?:" + part + ")$"); err == nil && re.MatchString(called) {
				return t[0]
			}
		}
	}
	return ""
}

// appScriptTollAllow: app.lua toll_allow <uuid>: hangs up (OUTGOING_CALL_BARRED) a call whose toll
// type (mobile, landline, international, tollfree, sharedcharge, premium, unknown for the
// default_country's number plan) is not in the extension's toll_allow list.
func appScriptTollAllow(s *Script) error {
	uuid := s.Arg(2)
	if uuid == "" {
		return nil
	}
	ch := s.Ch
	if ch == nil {
		ch = s.Core.Channel(uuid)
	}
	if ch == nil {
		return nil
	}
	called, caller := ch.Var("destination_number"), ch.Var("caller_id_number")
	prefix, country, allow := ch.Var("outbound_prefix"), ch.Var("default_country"), ch.Var("toll_allow")
	if allow == "" {
		s.Log("warn", "unknown call not authorized: OUTGOING_CALL_BARRED", "from", caller, "to", called)
		ch.Hangup(ParseCause("OUTGOING_CALL_BARRED"))
		return nil
	}
	if country == "" {
		s.Log("warn", "undefined country")
		return nil
	}
	tpl, ok := tollTemplates[country]
	if !ok {
		s.Log("warn", "undefined templates")
		return nil
	}
	typ := tollType(prefix, tpl, called)
	if typ == "" {
		typ = "unknown"
	}
	s.Log("info", "toll type", "type", typ)
	for _, part := range strings.Split(allow, ",") {
		if !tollKnown[part] {
			s.Log("warn", "unknown toll_allow name in extension", "name", part)
		}
		if part == typ {
			s.Log("info", "call authorized", "type", typ, "from", caller, "to", called)
			return nil
		}
	}
	s.Log("warn", "call not authorized: OUTGOING_CALL_BARRED", "type", typ, "from", caller, "to", called)
	ch.Hangup(ParseCause("OUTGOING_CALL_BARRED"))
	return nil
}

// ---------------------------------------------------------------------------
// valet_park

// appScriptValetPark: with valet_parking_direction=in the call is parked in the first free slot of
// the lot park@<context>, *5901 to *5999 (valet_parking_display shows the slot, valet_announce_slot
// says it).
func appScriptValetPark(s *Script) error {
	if !s.Ready() {
		return nil
	}
	if err := s.Answer(); err != nil {
		return err
	}
	ctxName := s.Var("context")
	if s.Var("valet_parking_direction") != "in" {
		return nil
	}
	c := s.Core
	lot := "park@" + ctxName
	slot := 0
	c.park.mu.Lock()
	for i := 5901; i <= 5999; i++ {
		if p := c.park.slots[strings.ToLower(lot+"/*"+strconv.Itoa(i))]; p == nil || p.IsHungUp() {
			slot = i
			break
		}
	}
	c.park.mu.Unlock()
	if slot == 0 {
		s.Log("warn", "valet park: no free slot", "lot", lot)
		return nil
	}
	s.Log("info", "valet park", "caller", s.Var("caller_id_number"), "slot", "*"+strconv.Itoa(slot))
	if s.Var("valet_parking_display") == "enable" {
		s.Sleep(3000)
	}
	if s.Var("valet_announce_slot") == "enable" {
		s.SayDigits(strconv.Itoa(slot))
	}
	if !s.Ready() {
		return nil
	}
	return s.Execute("valet_park", lot+" *"+strconv.Itoa(slot))
}
