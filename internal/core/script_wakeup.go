package core

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// wakeup.lua: wake-up calls from a phone (the FusionPBX "wakeup" feature code).
//
// Dialled from a phone (no argument) it asks for the extension to call (when wakeup_destination
// is "prompt"), the hour and the minutes, schedules the call and offers to cancel it. The call is
// a scheduled `originate ... &lua('wakeup.lua <domain> <extension> <sound>')` (sched_api task
// "wakeup-call-<extension>"), and on that call, answered, the script plays the wake-up sound:
// pressing 2 schedules it again after snooze_time seconds. sched_del wakeup-call-<ext> cancels.
//
// The call variables are those of the FusionPBX dialplan: time_zone_offset (hours from the
// server's clock), sip_number_alias, wakeup_destination, sip_auto_answer, snooze_time (600),
// wakeup_caller_id_name ("wakeup call"), wakeup_caller_id_number, and the sounds
// wakeup_destination_sound, wakeup_greeting_sound, wakeup_call_sound, wakeup_scheduled_sound,
// wakeup_accept_sound and wakeup_cancelled_sound (phrase macros by default: silent unless a
// recording of that name exists).
//
// The RingQ hotel wake-up calls (q_hotel_wakeup_calls) are scheduled and placed by the RingQ
// backend itself and do not use this script.

func init() { RegisterScript("wakeup", scriptWakeup) }

func wakeupIsNumeric(v string) bool {
	_, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return err == nil && strings.TrimSpace(v) != ""
}

// wakeupOriginateCommand is the scheduled command that calls the extension.
func wakeupOriginateCommand(delay int, domain, dest, sound, autoAnswer, snooze, cidName, cidNum string) string {
	return fmt.Sprintf("sched_api +%d wakeup-call-%s originate {hangup_after_bridge=false,%s,snooze_time=%s,"+
		"origination_caller_id_name='%s',origination_caller_id_number=%s,wakeup_caller_id_name='%s',wakeup_caller_id_number=%s}"+
		"user/%s@%s &lua('wakeup.lua %s %s %s')",
		delay, dest, autoAnswer, snooze, cidName, cidNum, cidName, cidNum, dest, domain, domain, dest, sound)
}

func wakeupAutoAnswer(v string) string {
	if v == "true" {
		return "sip_auto_answer=true,sip_h_Alert-Info='Ring Answer'"
	}
	return "sip_auto_answer=false"
}

func scriptWakeup(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	if s.Arg(2) != "" { // wakeup.lua <domain> <destination> <sound>: the call itself
		return wakeupCall(s)
	}
	if err := s.Answer(); err != nil {
		return err
	}
	domain := s.Var("domain_name")
	tzOffset := s.Var("time_zone_offset")
	alias := s.Var("sip_number_alias")
	destination := s.Var("wakeup_destination")
	from := s.Var("sip_from_user")
	autoAnswer := s.Var("sip_auto_answer")
	snooze := disaDefault(s.Var("snooze_time"), "600")
	cidName := disaDefault(s.Var("wakeup_caller_id_name"), "wakeup call")
	cidNum := disaDefault(s.Var("wakeup_caller_id_number"), s.Var("destination_number"))
	destSound := disaDefault(s.Var("wakeup_destination_sound"), "phrase:wakeup-destination")
	greetSound := disaDefault(s.Var("wakeup_greeting_sound"), "phrase:wakeup-greeting")
	callSound := disaDefault(s.Var("wakeup_call_sound"), "phrase:wakeup-call")
	schedSound := disaDefault(s.Var("wakeup_scheduled_sound"), "phrase:wakeup-scheduled")
	acceptSound := disaDefault(s.Var("wakeup_accept_sound"), "phrase:wakeup-accept")
	cancelSound := disaDefault(s.Var("wakeup_cancelled_sound"), "phrase:wakeup-cancelled")
	s.Sleep(500)
	if !s.Ready() {
		return nil
	}
	if destination == "prompt" {
		destination = s.PlayAndGetDigits(1, 11, 3, 3000*time.Millisecond, "#", destSound, "", `\d+`)
	} else if !wakeupIsNumeric(destination) {
		if wakeupIsNumeric(alias) {
			destination = alias
		} else {
			destination = from
		}
	}
	s.Sleep(500)
	if !s.Ready() {
		return nil
	}
	if destination == "" {
		s.Log("warn", "no extension to wake up")
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	s.Stream(greetSound)
	if !s.Ready() {
		return nil
	}
	hours := s.PlayAndGetDigits(2, 2, 3, 3000*time.Millisecond, "#", disaDefault(s.Var("wakeup_hours_sound"), "phrase:wakeup-hours"), "", `\d+`)
	if !s.Ready() {
		return nil
	}
	minutes := s.PlayAndGetDigits(2, 2, 3, 3000*time.Millisecond, "#", disaDefault(s.Var("wakeup_minutes_sound"), "phrase:wakeup-minutes"), "", `\d+`)
	if !s.Ready() {
		return nil
	}
	h, errH := strconv.Atoi(hours)
	m, errM := strconv.Atoi(minutes)
	if errH != nil || errM != nil || h > 99 || m > 99 {
		s.Log("warn", "no wake-up time entered", "hours", hours, "minutes", minutes)
		s.Hangup("NORMAL_CLEARING")
		return nil
	}
	now := time.Now()
	curH, curM := now.Hour(), now.Minute()
	if off, err := strconv.Atoi(strings.TrimSpace(tzOffset)); err == nil {
		curH += off
		if curH < 0 {
			curH += 24
		}
		if curH > 23 {
			curH -= 24
		}
	}
	// as the Lua: the seconds are ignored, and a time that has passed is for tomorrow
	curTime := curH*100 + curM
	wakeTime, _ := strconv.Atoi(strconv.Itoa(h) + strconv.Itoa(m)) // tonumber(tostring(h)..tostring(m))
	curSecs := curH*3600 + curM*60
	wakeSecs := h*3600 + m*60
	delay := wakeSecs - curSecs
	if curTime > wakeTime {
		delay = wakeSecs + (24*3600 - curSecs)
	}
	s.Log("info", "wake-up call", "time", fmt.Sprintf("%02d%02d", h, m), "in_seconds", delay, "extension", destination)
	s.Stream(schedSound)
	s.SayDigits(strconv.Itoa(wakeTime))
	cmd := wakeupOriginateCommand(delay, domain, destination, callSound, wakeupAutoAnswer(autoAnswer), snooze, cidName, cidNum)
	c.API(cmd)
	accept := s.PlayAndGetDigits(1, 1, 3, 3000*time.Millisecond, "#", acceptSound, "", `\d+`)
	switch accept {
	case "1":
		s.Hangup("")
	case "2":
		c.API("sched_del wakeup-call-" + destination)
		s.Stream(cancelSound)
		s.Hangup("")
	}
	return nil
}

// wakeup.lua <domain> <extension> <sound>: the wake-up call is answered: play the sound; 2 snoozes.
func wakeupCall(s *Script) error {
	c := s.Core
	dest, soundArg := s.Arg(2), s.Arg(3)
	if err := s.Answer(); err != nil {
		return err
	}
	s.Sleep(500)
	digits := s.PlayAndGetDigits(1, 1, 3, 20000*time.Millisecond, "#", soundArg, "", `\d+`)
	domain := s.Var("domain_name")
	if domain == "" {
		domain = s.Arg(1)
	}
	snooze := s.Var("snooze_time")
	cidName := s.Var("wakeup_caller_id_name")
	cidNum := s.Var("wakeup_caller_id_number")
	if digits == "2" {
		secs, err := strconv.Atoi(snooze)
		if err != nil {
			secs = 600
		}
		s.Log("info", "wake-up call snoozed", "extension", dest, "seconds", secs)
		c.API(wakeupOriginateCommand(secs, domain, dest, soundArg, wakeupAutoAnswer(s.Var("sip_auto_answer")), strconv.Itoa(secs), cidName, cidNum))
	}
	return nil
}
