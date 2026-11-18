package core

import (
	"strconv"
	"strings"
	"time"
)

// FusionPBX's stock Lua scripts that the RingQ dialplans call, in Go:
// streamfile.lua, ivr_ai2.lua, call_forward.lua and pin_number.lua.

func init() {
	RegisterScript("streamfile", scriptStreamFile)
	RegisterScript("ivr_ai2", scriptIVRAI2)
	RegisterScript("call_forward", scriptCallForward)
	RegisterScript("pin_number", scriptPINNumber)
}

// streamfile.lua <file>: plays a recording or prompt to the caller (the
// break-time and non-office-hours messages).
func scriptStreamFile(s *Script) error {
	if !s.Ready() {
		return nil
	}
	file := strings.TrimSpace(strings.Join(s.Args, " "))
	if file == "" {
		return nil
	}
	if err := s.Answer(); err != nil {
		return err
	}
	return s.Execute("playback", file)
}

// ivr_ai2.lua <caller uuid> <domain>: tells the AI service (node) a caller is on the
// AI IVR; it starts the audio stream.
func scriptIVRAI2(s *Script) error {
	if !s.Ready() || s.Arg(1) == "" {
		return nil
	}
	e := NewEvent(EvCustom, "ai_ivr_recording2")
	e.Add("Recording-Filename", "")
	e.Add("Caller-uuid", s.Arg(1))
	e.Add("Caller-subdomain", s.Arg(2))
	s.Core.events.Publish(e)
	s.Log("info", "ai ivr call", "uuid", s.Arg(1))
	return nil
}

// call_forward.lua [destination]: the *72 / *73 / *74 feature codes. The dialplan sets
// enabled=true|false|toggle; the destination is the argument, the one the extension has,
// or asked for on the phone.
func scriptCallForward(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c, ch := s.Core, s.Ch
	if err := s.Answer(); err != nil {
		return err
	}
	user, domain := ch.Var("user_name"), ch.Var("domain_name")
	if user == "" || c.config().Directory.LookupIn(domain, user) == nil {
		_, _ = c.play(s.Ctx, ch, toneErr, false)
		return nil
	}
	s.Sleep(1000)
	enabled := strings.ToLower(strings.TrimSpace(s.Var("enabled")))
	curOn := truthy(c.UserVar(user, domain, "forward_all_enabled"))
	curDest := c.UserVar(user, domain, "forward_all_destination")
	dest := strings.TrimSpace(s.Arg(1))
	if enabled == "toggle" {
		enabled = strconv.FormatBool(!curOn)
	}
	if enabled == "true" && dest == "" {
		dest = firstNonEmpty(s.Var("forward_all_destination"), curDest)
	}
	if enabled == "true" && dest == "" {
		// ask for the number to forward to (three tries)
		for try := 0; try < 3 && dest == "" && s.Ready(); try++ {
			ch.FlushDTMF()
			first, err := c.play(s.Ctx, ch, "ivr/ivr-enter_destination_telephone_number.wav", true)
			if err != nil {
				return nil
			}
			dest, _ = collectDigits(s.Ctx, ch, first, 17, "#", 5*time.Second, 5*time.Second)
		}
		if dest == "" {
			return nil
		}
	}
	vars := map[string]string{"forward_all_enabled": "false", "forward_all_destination": ""}
	prompt := "ivr/ivr-call_forwarding_has_been_cancelled.wav"
	if enabled == "true" && dest != "" {
		vars = map[string]string{"forward_all_enabled": "true", "forward_all_destination": dest, "do_not_disturb": "false"}
		prompt = "ivr/ivr-call_forwarding_has_been_set.wav"
	}
	if err := c.SetUserVars(user, domain, vars, "phone"); err != nil {
		_, _ = c.play(s.Ctx, ch, toneErr, false)
		return err
	}
	ch.flow("route", "Call forward "+vars["forward_all_enabled"])
	if _, err := c.play(s.Ctx, ch, prompt, false); err != nil {
		return nil
	}
	s.Sleep(100)
	s.Hangup("NORMAL_CLEARING")
	return nil
}

// pin_number.lua: asks for a PIN (the domain's PINs, v_pin_numbers) before the call goes
// on; three tries, then the call is refused. The PIN's account code is set on the call.
func scriptPINNumber(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c, ch := s.Core, s.Ch
	if truthy(ch.Var("pin_verified")) {
		return nil
	}
	domainUUID := ch.Var("domain_uuid")
	pins := map[string]string{}
	if rows, err := c.sqlRows(s.Ctx, `SELECT pin_number, coalesce(accountcode, '') AS accountcode FROM v_pin_numbers
		WHERE domain_uuid = $1::uuid AND (enabled IS NULL OR enabled::text IN ('true', 't', '1'))`, domainUUID); err == nil {
		for _, r := range rows {
			pins[r["pin_number"]] = r["accountcode"]
		}
	} else {
		s.Log("warn", "PINs not read", "err", err)
	}
	pin, acct, ok := c.askDialPIN(s.Ctx, ch, pins)
	if !ok {
		if s.Ready() {
			ch.log.Warn("call refused: no valid PIN")
			ch.Hangup(CauseCallRejected)
		}
		return nil
	}
	ch.SetVar("pin_verified", "true")
	ch.SetVar("pin_number", pin)
	if acct != "" {
		ch.SetVar("accountcode", acct)
	}
	return nil
}
