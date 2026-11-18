package core

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DISA (Direct Inward System Access): the Go versions of the FusionPBX scripts disa.lua,
// disa.plus.lua, disa.callback.lua and disa.callback.loopback.lua. A caller dials in, proves
// who they are (a PIN, or a reference number and PIN of a calling-card contact), and dials out as
// the PBX.
//
//	lua disa.lua                  PIN (pin_number) and destination, then transfer into the dialplan
//	lua disa.plus.lua             calling-card DISA: reference number + PIN against v_contact_settings,
//	                              pinless callers by number, optional callback, gateways
//	lua disa.callback.lua a b ctx acct          call a, then b, and bridge them (started by disa.plus
//	                                            with sched_api +2 for the callback option)
//	lua disa.callback.loopback.lua a b ctx acct the same through loopback/<number>/<context>

func init() {
	RegisterScript("disa", scriptDISA)
	RegisterScript("disa.plus", scriptDISAPlus)
	RegisterScript("disa.callback", scriptDISACallback)
	RegisterScript("disa.callback.loopback", scriptDISACallbackLoopback)
}

// scriptName strips ".lua" only: "disa.plus.lua" is the name "disa.plus", which filepath-based code
// would read as the extension ".plus" of "disa". scriptName already handles that (TrimSuffix).

// disaDefault is v or def.
func disaDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func disaInt(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return def
}

func disaMillis(v string, def int) time.Duration {
	return time.Duration(disaInt(v, def)) * time.Millisecond
}

// disaDenied is the goodbye of a refused caller: invalid pin, sorry, goodbye, hangup.
func disaDenied(s *Script) {
	s.Stream("ivr/ivr-pin_or_extension_is-invalid.wav")
	s.Stream("ivr/ivr-im_sorry.wav")
	s.Stream("voicemail/vm-goodbye.wav")
	s.Hangup("NORMAL_CLEARING")
}

// disa.lua
func scriptDISA(s *Script) error {
	if !s.Ready() {
		return nil
	}
	if err := s.Answer(); err != nil {
		return err
	}
	if !s.Ready() {
		return nil
	}
	soundGreeting := s.Var("sound_greeting")
	soundPIN := disaDefault(s.Var("sound_pin"), "ivr/ivr-please_enter_pin_followed_by_pound.wav")
	soundExt := disaDefault(s.Var("sound_extension"), "ivr/ivr-enter_destination_telephone_number.wav")
	pin := s.Var("pin_number")
	predefined := s.Var("predefined_destination")
	fallback := s.Var("fallback_destination")
	minLen := disaInt(s.Var("digit_min_length"), 7)
	maxLen := disaInt(s.Var("digit_max_length"), 11)
	timeout := disaMillis(s.Var("digit_timeout"), 5000)
	ctxName := s.Var("context")
	maxTries := disaInt(s.Var("max_tries"), 3)
	pinTries := disaInt(s.Var("pin_tries"), maxTries)
	extTries := disaInt(s.Var("extension_tries"), maxTries)

	if soundGreeting != "" {
		s.Stream(soundGreeting)
		s.Sleep(200)
	}
	if s.Ready() && pin != "" {
		digits := s.PlayAndGetDigits(len(pin), len(pin)+1, pinTries, timeout, "#", soundPIN, "", `\d+`)
		if digits != pin {
			disaDenied(s)
			return nil
		}
	}
	dest := ""
	if s.Ready() {
		if predefined != "" {
			dest = predefined
		} else {
			s.Sleep(1000)
			dest = s.PlayAndGetDigits(minLen, maxLen, extTries, timeout, "#", soundExt, "", `\d+`)
			if dest == "" && fallback != "" {
				dest = fallback
			}
		}
	}
	if s.Ready() && s.Var("privacy") == "true" {
		_ = s.Execute("privacy", "full")
		_ = s.Execute("set", "sip_h_Privacy=id")
		_ = s.Execute("set", "privacy=yes")
	}
	if !s.Ready() {
		return nil
	}
	if dest == "" {
		s.Log("info", "no destination dialled")
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	// calls to outside numbers go out with the outbound caller id (else the caller's own)
	if s.API("user_exists id "+dest+" "+ctxName) == "false" {
		name := firstNonEmpty(s.Var("outbound_caller_id_name"), s.Var("caller_id_name"))
		num := firstNonEmpty(s.Var("outbound_caller_id_number"), s.Var("caller_id_number"))
		if name != "" {
			_ = s.Execute("set", "outbound_caller_id_name="+name)
			_ = s.Execute("set", "effective_caller_id_name="+name)
		}
		if num != "" {
			_ = s.Execute("set", "outbound_caller_id_number="+num)
			_ = s.Execute("set", "effective_caller_id_number="+num)
		}
	}
	_ = s.Execute("set", "disa_outbound=true")
	return s.Transfer(dest, ctxName)
}

// disa.plus.lua
func scriptDISAPlus(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if err := s.Answer(); err != nil {
		return err
	}
	if !s.Ready() {
		return nil
	}
	reference := s.Var("reference_number")
	pin := s.Var("pin_number")
	predefined := s.Var("predefined_destination")
	gateway := s.Var("gateway")
	ctxName := s.Var("context")
	soundRef := disaDefault(s.Var("sound_reference"), "misc/provide_reference_number.wav")
	soundPIN := disaDefault(s.Var("sound_pin"), "ivr/ivr-please_enter_pin_followed_by_pound.wav")
	soundCallback := disaDefault(s.Var("sound_callback"), "ivr/ivr-we_will_return_your_call_at_this_number.wav")
	pinMin := disaInt(s.Var("pin_min_length"), 1)
	pinMax := disaInt(s.Var("pin_max_length"), 16)
	digMin := disaInt(s.Var("digit_min_length"), 2)
	digMax := 12
	if v := s.Var("digit_max_length"); v != "" {
		digMax = disaInt(v, 11) + 1
	}
	const maxTries = 3
	const timeout = 5000 * time.Millisecond
	pinless := s.Var("pinless") != ""
	callback := s.Var("callback") != ""
	cidName, cidNum := s.Var("caller_id_name"), s.Var("caller_id_number")

	if reference != "" && pin != "" {
		s.Log("info", "you can not set reference_number and pin_number at same time")
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	userExists := s.API("user_exists id "+cidNum+" "+ctxName) == "true"
	s.Log("info", "caller", "number", cidNum, "user_exists", userExists)

	domainUUID, contactUUID := "", ""
	findContact := func(q string, args ...any) {
		rows, err := c.sqlRows(s.Ctx, q, args...)
		if err != nil {
			s.Log("warn", "contact lookup failed", "err", err)
			return
		}
		if len(rows) > 0 {
			domainUUID, contactUUID = rows[0]["domain_uuid"], rows[0]["contact_uuid"]
		}
	}
	if pinless {
		// the caller's number is a calling card's pinless number
		findContact(`SELECT c.domain_uuid::text AS domain_uuid, c.contact_uuid::text AS contact_uuid FROM v_contacts c
			INNER JOIN v_contact_settings s1 USING (contact_uuid)
			WHERE s1.contact_setting_category = 'calling card' AND s1.contact_setting_subcategory = 'pinless'
			AND s1.contact_setting_name = 'phonenumber' AND s1.contact_setting_value = $1`, cidNum)
	} else {
		if reference == "" {
			reference = s.PlayAndGetDigits(pinMin, pinMax, maxTries, timeout, "#", soundRef, "", `\d+`)
		}
		s.Log("info", "reference number", "reference", reference)
		pinDigits := s.PlayAndGetDigits(pinMin, pinMax, maxTries, timeout, "#", soundPIN, "", `\d+`)
		if pin != "" {
			// the PIN is fixed for all callers; the reference number picks the contact
			if pinDigits != pin {
				disaDenied(s)
				return nil
			}
			findContact(`SELECT c.domain_uuid::text AS domain_uuid, c.contact_uuid::text AS contact_uuid FROM v_contacts c
				INNER JOIN v_contact_settings s1 USING (contact_uuid)
				WHERE s1.contact_setting_category = 'calling card' AND s1.contact_setting_subcategory = 'authentication'
				AND s1.contact_setting_name = 'username' AND s1.contact_setting_value = $1`, reference)
		} else {
			findContact(`SELECT c.domain_uuid::text AS domain_uuid, c.contact_uuid::text AS contact_uuid FROM v_contacts c
				INNER JOIN v_contact_settings s1 USING (contact_uuid) INNER JOIN v_contact_settings s2 USING (contact_uuid)
				WHERE s1.contact_setting_category = 'calling card' AND s1.contact_setting_subcategory = 'authentication'
				AND s1.contact_setting_name = 'username' AND s1.contact_setting_value = $1
				AND s2.contact_setting_category = 'calling card' AND s2.contact_setting_subcategory = 'authentication'
				AND s2.contact_setting_name = 'password' AND s2.contact_setting_value = $2`, reference, pinDigits)
		}
		if contactUUID == "" {
			disaDenied(s)
			return nil
		}
	}
	if pinless && contactUUID == "" {
		s.Log("warn", "pinless caller is not a calling card contact", "number", cidNum)
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	if !s.Ready() {
		return nil
	}
	dest := predefined
	if dest == "" {
		dest = s.PlayAndGetDigits(digMin, digMax, maxTries, timeout, "#", "ivr/ivr-enter_destination_telephone_number.wav", "", `\d+`)
	}
	s.Log("info", "destination", "number", dest)
	// the caller id of the outgoing call
	if userExists {
		cidName = firstNonEmpty(cidName, s.Var("effective_caller_id_name"))
		cidNum = firstNonEmpty(cidNum, s.Var("effective_caller_id_number"))
	} else {
		cidName = firstNonEmpty(cidName, s.Var("outbound_caller_id_name"))
		cidNum = firstNonEmpty(cidNum, s.Var("outbound_caller_id_number"))
	}
	if callback {
		s.Stream(soundCallback)
		s.Log("info", "scheduling to call back", "number", cidNum, "channel", s.Var("channel_name"))
		c.API(fmt.Sprintf("sched_api +2 none lua disa.callback.lua %s %s %s %s", cidNum, dest, ctxName, ctxName))
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	if !s.Ready() {
		return nil
	}
	if domainUUID == "" {
		s.Log("warn", "the contact has no domain")
		return nil
	}
	if userExists {
		return s.Execute("bridge", "{domain_uuid="+domainUUID+"}user/"+dest+"@"+ctxName)
	}
	if gateway != "" {
		for _, gw := range strings.Split(gateway, ",") {
			if !s.Ready() {
				break
			}
			_ = s.Execute("bridge", "{domain_uuid="+domainUUID+",continue_on_fail=true,hangup_after_bridge=true,origination_caller_id_name="+
				cidName+",origination_caller_id_number="+cidNum+"}sofia/gateway/"+strings.TrimSpace(gw)+"/"+dest)
		}
		return nil
	}
	_ = s.Execute("set", "domain_uuid="+domainUUID)
	_ = s.Execute("set", "effective_caller_id_name="+cidName)
	_ = s.Execute("set", "effective_caller_id_number="+cidNum)
	return s.Transfer(dest, ctxName)
}

// ---------------------------------------------------------------------------
// the callback legs

// disaLegDial gives the dial string disa.callback.lua builds for a number: a user of the domain
// is dialled as user/<n>@<context>; any other number goes where the domain's outbound routes
// (v_dialplans of the outbound-route app) send it, one bridge per route that matches.
func disaLegDial(s *Script, number, other, ctxName, accountcode, domainUUID string) string {
	common := "context=" + ctxName + ",sip_invite_domain=" + ctxName + ",domain_name=" + ctxName + ",domain=" + ctxName +
		",accountcode=" + accountcode + ",domain_uuid=" + domainUUID
	if s.API("user_exists id "+number+" "+ctxName) == "true" {
		return "[origination_caller_id_number=*3472,outbound_caller_id_number=*3472,call_timeout=30," + common + "]user/" + number + "@" + ctxName
	}
	rows, err := s.Core.sqlRows(s.Ctx, `SELECT d.dialplan_uuid::text AS dialplan_uuid, s.dialplan_detail_tag AS tag, s.dialplan_detail_type AS type,
		s.dialplan_detail_data AS data FROM v_dialplans d, v_dialplan_details s
		WHERE (d.domain_uuid = $1::uuid OR d.domain_uuid IS NULL) AND d.app_uuid = '8c914ec3-9fc0-8ab5-4cda-6c9288bdc9a3'
		AND d.dialplan_enabled::text IN ('true', 't') AND d.dialplan_uuid = s.dialplan_uuid
		ORDER BY d.dialplan_order ASC, d.dialplan_name ASC, d.dialplan_uuid ASC, s.dialplan_detail_group ASC,
		CASE s.dialplan_detail_tag WHEN 'condition' THEN 1 WHEN 'action' THEN 2 WHEN 'anti-action' THEN 3 ELSE 100 END,
		s.dialplan_detail_order ASC`, domainUUID)
	if err != nil {
		s.Log("warn", "outbound routes not read", "err", err)
		return ""
	}
	var dial []string
	var square strings.Builder
	matched, started := false, false
	capture := ""
	prev := ""
	flush := func() {
		if started {
			started = false
		}
	}
	for _, r := range rows {
		if prev != "" && prev != r["dialplan_uuid"] {
			matched = false
			flush()
		}
		prev = r["dialplan_uuid"]
		switch {
		case r["tag"] == "condition" && r["type"] == "destination_number":
			if re, err := regexp.Compile(r["data"]); err == nil {
				if m := re.FindStringSubmatch(number); m != nil {
					matched = true
					capture = ""
					if len(m) > 1 {
						capture = m[1]
					}
				}
			}
		case r["tag"] == "action" && matched:
			data := strings.ReplaceAll(r["data"], "$1", capture)
			if !started {
				started = true
				square.Reset()
				square.WriteString("direction=outbound,origination_caller_id_number=" + other + ",outbound_caller_id_number=" + other +
					",call_timeout=30," + common + ",")
			}
			switch r["type"] {
			case "set":
				switch data {
				case "sip_h_X-accountcode=${accountcode}":
					square.WriteString("sip_h_X-accountcode=" + accountcode + ",")
				case "effective_caller_id_name=${outbound_caller_id_name}", "effective_caller_id_number=${outbound_caller_id_number}":
				default:
					square.WriteString(data + ",")
				}
			case "bridge":
				dial = append(dial, "["+strings.TrimSuffix(square.String(), ",")+"]"+data)
			}
		}
	}
	return strings.Join(dial, ",")
}

// disaCall places one leg and waits (up to 30 s) for it to answer; nil if it did not.
func disaCall(s *Script, dial, number string) *Channel {
	if dial == "" {
		s.Log("warn", "no way to call", "number", number)
		return nil
	}
	s.Log("info", "calling", "number", number, "dial", dial)
	ctx, cancel := context.WithTimeout(s.Ctx, 35*time.Second)
	defer cancel()
	ch, cause := s.Core.originate(ctx, nil, dial, map[string]string{"call_timeout": "30"})
	if ch == nil {
		s.Log("info", "no answer", "number", number, "cause", cause.String())
		return nil
	}
	return ch
}

// disaBridgeLegs bridges two answered legs and waits until the call ends.
func disaBridgeLegs(s *Script, a, b *Channel) {
	s.Core.adopt(a)
	s.Core.adopt(b)
	done := make(chan struct{})
	go func() {
		s.Core.bridgeChannels(context.Background(), a, b)
		close(done)
	}()
	<-done
}

func disaCallbackArgs(s *Script) (aleg, bleg, ctxName, accountcode string, ok bool) {
	aleg, bleg, ctxName, accountcode = s.Arg(1), s.Arg(2), s.Arg(3), s.Arg(4)
	if aleg == "" || bleg == "" || ctxName == "" {
		s.Log("warn", "usage: disa.callback.lua <a number> <b number> <context> <accountcode>")
		return "", "", "", "", false
	}
	return aleg, bleg, ctxName, accountcode, true
}

// disa.callback.lua: call a (a user, or through the outbound routes), then b, and bridge them.
func scriptDISACallback(s *Script) error {
	aleg, bleg, ctxName, acct, ok := disaCallbackArgs(s)
	if !ok {
		return nil
	}
	domainUUID := s.Core.domainUUID(s.Ctx, ctxName)
	if domainUUID == "" {
		s.Log("warn", "unknown domain", "context", ctxName)
		return nil
	}
	s.Sleep(2000)
	a := disaCall(s, disaLegDial(s, aleg, bleg, ctxName, acct, domainUUID), aleg)
	if a == nil {
		return nil
	}
	a.SetVar("domain_uuid", domainUUID)
	b := disaCall(s, disaLegDial(s, bleg, aleg, ctxName, acct, domainUUID), bleg)
	if b == nil {
		a.Hangup(CauseNoAnswer)
		return nil
	}
	disaBridgeLegs(s, a, b)
	return nil
}

// disa.callback.loopback.lua: the same through loopback/<number>/<context>.
func scriptDISACallbackLoopback(s *Script) error {
	aleg, bleg, ctxName, acct, ok := disaCallbackArgs(s)
	if !ok {
		return nil
	}
	domainUUID := s.Core.domainUUID(s.Ctx, ctxName)
	if domainUUID == "" {
		s.Log("warn", "unknown domain", "context", ctxName)
		return nil
	}
	common := "context=" + ctxName + ",domain_name=" + ctxName + ",domain=" + ctxName + ",accountcode=" + acct + ",domain_uuid=" + domainUUID
	s.Sleep(2000)
	a := disaCall(s, "{direction=outbound,origination_caller_id_number=*3472,outbound_caller_id_number=*3472,call_timeout=30,"+common+"}loopback/"+aleg+"/"+ctxName, aleg)
	if a == nil {
		return nil
	}
	a.SetVar("domain_uuid", domainUUID)
	b := disaCall(s, "{"+common+"}loopback/"+bleg+"/"+ctxName, bleg)
	if b == nil {
		a.Hangup(CauseNoAnswer)
		return nil
	}
	disaBridgeLegs(s, a, b)
	return nil
}
