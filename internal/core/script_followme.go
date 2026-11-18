package core

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Follow me, the Go versions of follow_me.lua (the *21 toggle) and
// app/follow_me/index.lua (the dialplan "app.lua follow_me <follow_me_uuid>").
//
//	follow_me.lua            toggles the extension's follow me (v_follow_me, v_extensions),
//	                         plays "call forwarding has been set / cancelled" and hangs up
//	app.lua follow_me <uuid> rings the extension's follow me destinations: one bridge with a leg
//	                         per destination (simultaneous, or "enterprise": each leg with its own
//	                         delay and timeout), the "prompt to accept" confirmation, then the
//	                         no answer forwarding (or the voicemail) when nobody answered

func init() {
	RegisterScript("follow_me", scriptFollowMeToggle)
	RegisterScript("app.follow_me", scriptFollowMeApp)
}

func scriptFollowMeToggle(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	_ = s.Answer()
	domainUUID, domain, extUUID := s.Var("domain_uuid"), s.Var("domain_name"), s.Var("extension_uuid")
	s.Sleep(1000)
	if !s.Ready() {
		return nil
	}
	row := s.Row("SELECT extension, number_alias, accountcode, follow_me_uuid, follow_me_enabled FROM v_extensions "+
		"WHERE domain_uuid = $1::uuid AND extension_uuid = $2::uuid", domainUUID, extUUID)
	if row == nil {
		return nil
	}
	ext, enabled, fmUUID := row["extension"], row["follow_me_enabled"], row["follow_me_uuid"]
	switch enabled {
	case "false":
		s.display("Activated")
		s.Sleep(2000)
		s.PlaySound("ivr/ivr-call_forwarding_has_been_set.wav")
	case "true":
		s.display("Cancelled")
		s.Sleep(2000)
		s.PlaySound("ivr/ivr-call_forwarding_has_been_cancelled.wav")
	}
	next := "true"
	if enabled == "true" {
		next = "false"
	}
	if fmUUID != "" {
		s.Exec("UPDATE v_follow_me SET follow_me_enabled = $1::text WHERE domain_uuid = $2::uuid AND follow_me_uuid = $3::uuid",
			next, domainUUID, fmUUID)
	}
	s.Exec("UPDATE v_extensions SET do_not_disturb = 'false', follow_me_enabled = $1::text, forward_all_enabled = 'false' "+
		"WHERE domain_uuid = $2::uuid AND extension_uuid = $3::uuid", next, domainUUID, extUUID)
	// the directory cache: the change counts at once, and the DND / forwarding lamps follow
	if ext != "" {
		_ = c.SetUserVars(ext, domain, map[string]string{"follow_me_enabled": next, "do_not_disturb": "false", "forward_all_enabled": "false"}, "follow_me")
	}
	s.Sleep(1000)
	s.Hangup("")
	return nil
}

// display is channel_display / uuid_display: the switch cannot update a phone's display; it is logged.
func (s *Script) display(text string) {
	if s.Ch != nil {
		s.SetVar("rq_display", text)
		s.Log("debug", "display", "text", text)
	}
}

// fmForwardAll follows call forwarding (forward all) of local extensions, up to 5 hops:
// the destination, and the toll allow and account code of the last extension that forwarded.
func fmForwardAll(c *Core, dest, domain, toll, acct string) (string, string, string) {
	for count := 0; count < 5; count++ {
		if !c.userExists(dest, domain) || !truthy(c.extData(dest, domain, "forward_all_enabled")) {
			break
		}
		toll = c.extData(dest, domain, "toll_allow")
		acct = c.extData(dest, domain, "accountcode")
		dest = c.extData(dest, domain, "forward_all_destination")
	}
	return dest, toll, acct
}

type fmDest struct {
	number, domain, domainUUID string
	delayMs, timeout           int
	prompt                     string
	exists                     bool
	toll, acct                 string
}

func scriptFollowMeApp(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	ch := s.Ch
	domain, domainUUID := s.Var("domain_name"), s.Var("domain_uuid")
	origDest := s.Var("destination_number")
	callerName, callerNum := s.Var("caller_id_name"), s.Var("caller_id_number")
	outName, outNum := s.Var("outbound_caller_id_name"), s.Var("outbound_caller_id_number")
	direction := firstNonEmpty(s.Var("call_direction"), "local")

	// the strategy: simultaneous or enterprise (the default)
	strategy := "enterprise"
	if domainUUID != "" {
		if r := s.Row("SELECT domain_setting_value AS v FROM v_domain_settings WHERE domain_uuid = $1::uuid AND domain_setting_category = 'follow_me' "+
			"AND domain_setting_subcategory = 'strategy' AND domain_setting_enabled = 'true' LIMIT 1", domainUUID); r != nil && r["v"] != "" {
			strategy = r["v"]
		} else if r := s.Row("SELECT default_setting_value AS v FROM v_default_settings WHERE default_setting_category = 'follow_me' " +
			"AND default_setting_subcategory = 'strategy' AND default_setting_enabled = 'true' LIMIT 1"); r != nil && r["v"] != "" {
			strategy = r["v"]
		}
	}

	if domainUUID == "" && domain != "" {
		if r := s.Row("SELECT domain_uuid FROM v_domains WHERE domain_name = $1", domain); r != nil {
			domainUUID = r["domain_uuid"]
		}
	}

	// the extension and its follow me
	ext := s.Row("SELECT follow_me_uuid, toll_allow, accountcode FROM v_extensions WHERE domain_uuid = $1::uuid "+
		"AND (extension = $2 OR number_alias = $2)", domainUUID, origDest)
	var fmUUID, extToll, acct string
	if ext != nil {
		fmUUID, extToll, acct = ext["follow_me_uuid"], ext["toll_allow"], ext["accountcode"]
	}
	var dests []fmDest
	if fmUUID != "" {
		rows := s.Rows("SELECT d.domain_uuid, d.domain_name, f.follow_me_destination AS destination_number, "+
			"f.follow_me_delay * 1000 AS destination_delay, f.follow_me_timeout AS destination_timeout, f.follow_me_prompt AS destination_prompt "+
			"FROM v_follow_me_destinations AS f, v_domains AS d WHERE f.follow_me_uuid = $1::uuid AND f.domain_uuid = d.domain_uuid "+
			"ORDER BY f.follow_me_order", fmUUID)
		for _, r := range rows {
			d := fmDest{number: strings.TrimSpace(r["destination_number"]), domain: r["domain_name"], domainUUID: r["domain_uuid"],
				delayMs: optNum(r["destination_delay"], 0), timeout: optNum(r["destination_timeout"], 30), prompt: r["destination_prompt"]}
			// follow the forwards
			d.number, d.toll, d.acct = fmForwardAll(c, d.number, d.domain, "", acct)
			if c.userExists(d.number, d.domain) {
				d.exists = true
				if truthy(c.extData(d.number, d.domain, "do_not_disturb")) {
					dests = append(dests, fmDest{number: "", domain: "skip"}) // keeps the count of destinations: DND ones are not rung
					continue
				}
			} else {
				d.acct = firstNonEmpty(d.acct, d.domain)
			}
			dests = append(dests, d)
		}
	}
	count := len(dests)

	// a local destination that is not registered goes to its "user not registered" forwarding
	for i := range dests {
		d := &dests[i]
		if d.domain == "skip" || !d.exists {
			continue
		}
		if !c.registered(d.number, d.domain) && truthy(c.extData(d.number, d.domain, "forward_user_not_registered_enabled")) {
			if n := c.extData(d.number, d.domain, "forward_user_not_registered_destination"); n != "" {
				d.number = n
			}
		}
	}

	timeoutName, delayName := "leg_timeout", "leg_delay_start"
	// enterprise legs have their own timeout and delay: here every leg is a parallel leg of the
	// bridge with its own leg_timeout and leg_delay_start, which rings the same way
	var legs []string
	for _, d := range dests {
		if d.domain == "skip" {
			continue
		}
		dest, _, _ := fmForwardAll(c, d.number, d.domain, "", "")
		d.number = dest
		d.exists = c.userExists(d.number, d.domain)
		if count == 1 && s.Ready() {
			_ = s.Execute("set", "call_timeout="+strconv.Itoa(d.timeout))
		}
		delay := strconv.Itoa((d.delayMs + 999) / 1000)
		// (FusionPBX gave leg_delay_start in milliseconds, so a delay of 5 s became 5000 s: here it is the seconds it is meant to be)
		timing := timeoutName + "=" + strconv.Itoa(d.timeout) + "," + delayName + "=" + delay
		confirm := "confirm=false"
		if d.prompt == "1" || d.prompt == "2" {
			confirm = "group_confirm_key=exec,group_confirm_file=lua confirm.lua,confirm=true"
		}
		if strategy == "simultaneous" {
			s.SetVar("group_confirm_key", "exec")
			s.SetVar("group_confirm_file", "lua confirm.lua")
		}
		var leg string
		switch {
		case d.exists:
			extUUID := strings.TrimSpace(c.extData(d.number, d.domain, "extension_uuid"))
			rec := ""
			if s.Var("record_session") != "true" {
				if ur := c.extData(d.number, d.domain, "user_record"); ur == "all" || ur == direction {
					recDir := s.Var("recordings_dir")
					name := s.UUID() + "." + firstNonEmpty(s.Var("record_ext"), "wav")
					path := filepath.ToSlash(filepath.Join(recDir, d.domain, "archive", time.Now().Format("2006"), time.Now().Format("Jan"), time.Now().Format("02")))
					rec = ",api_on_answer=uuid_record " + s.UUID() + " start " + path + "/" + name + ",record_path=" + path + ",record_name=" + name
				}
			}
			leg = fmt.Sprintf("[sip_invite_domain=%s,call_direction=%s,%s,%s,dialed_extension=%s,extension_uuid=%s%s]user/%s@%s",
				d.domain, direction, confirm, timing, d.number, extUUID, rec, d.number, d.domain)
		case !isNumber(d.number):
			// a sip uri
			leg = fmt.Sprintf("[sip_invite_domain=%s,call_direction=%s,%s,%s]%s", d.domain, direction, confirm, timing, d.number)
		default:
			// an outside number: through the dialplan
			toll := ""
			if extToll != "" {
				toll = strings.ReplaceAll(extToll, ",", ":")
			}
			cn, cnum := callerName, callerNum
			if isNumber(callerNum) && c.userExists(callerNum, domain) && s.Var("follow_me_ignore_outbound_caller_id") != "true" {
				cn, cnum = firstNonEmpty(outName, cn), firstNonEmpty(outNum, cnum)
			}
			cid := ""
			if cn != "" {
				cid = "origination_caller_id_name='" + chanSafe(cn) + "'"
			}
			if cnum != "" {
				cid += ",origination_caller_id_number=" + chanSafe(cnum)
			}
			leg = fmt.Sprintf("[toll_allow=%s,accountcode=%s,%s,sip_invite_domain=%s,domain_uuid=%s,call_direction=%s,%s,%s]loopback/%s",
				toll, d.acct, cid, d.domain, d.domainUUID, direction, confirm, timing, d.number)
		}
		legs = append(legs, leg)
	}

	if s.Ready() {
		_ = s.Execute("ring_ready", "")
	}
	if !s.Ready() {
		return nil
	}
	_ = s.Execute("set", "hangup_after_bridge=true")
	_ = s.Execute("set", "continue_on_fail=true")
	if len(legs) == 0 {
		// no destination to ring (all do-not-disturb, or none set up): like FreeSWITCH, nothing is bridged
		// and the dialplan goes on
		return nil
	}
	data := strings.Join(legs, ",")
	if strategy == "simultaneous" {
		data = "{ignore_early_media=true}" + data
	}
	s.Log("info", "follow me", "ext", origDest+"@"+domain, "strategy", strategy, "legs", len(legs))
	_ = s.Execute("bridge", data)

	switch ch.Var("originate_disposition") {
	case "ALLOTTED_TIMEOUT", "NO_ANSWER", "NO_USER_RESPONSE", "USER_NOT_REGISTERED", "NORMAL_TEMPORARY_FAILURE",
		"NO_ROUTE_DESTINATION", "USER_BUSY", "RECOVERY_ON_TIMER_EXPIRE", "failure":
		if s.Ready() {
			fwd := truthy(c.extData(origDest, domain, "forward_no_answer_enabled"))
			userCtx := c.extData(origDest, domain, "user_context")
			if fwd {
				c.transfer(ch, c.extData(origDest, domain, "forward_no_answer_destination"), firstNonEmpty(userCtx, domain))
			} else {
				c.transfer(ch, "*99"+origDest, firstNonEmpty(userCtx, domain))
			}
		}
		fmMissed(s)
	}
	return nil
}

func isNumber(v string) bool {
	if v == "" {
		return false
	}
	_, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return err == nil
}

// fmMissed is missed(): the call was not answered; the "missed call" e-mail the Lua version
// sent through FusionPBX's mailer is left to the RingQ server, which reads the CDR
// (missed_call_app / missed_call_data are kept in the call's variables).
func fmMissed(s *Script) {
	if s.Var("missed_call_app") == "email" && s.Var("missed_call_data") != "" {
		s.Log("info", "missed call", "to", s.Var("missed_call_data"), "from", s.Var("caller_id_number"))
		e := NewEvent(EvCustom, "ringq::missed_call")
		e.Add("Caller-ID-Number", s.Var("caller_id_number"))
		e.Add("Caller-ID-Name", s.Var("caller_id_name"))
		e.Add("Dialed-User", s.Var("dialed_user"))
		e.Add("Missed-Call-Email", s.Var("missed_call_data"))
		e.Add("Domain-Name", s.Var("domain_name"))
		e.Add("Call-UUID", s.UUID())
		s.Core.events.Publish(e)
	}
}
