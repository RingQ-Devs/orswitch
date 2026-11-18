package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ringq-devs/orswitch/internal/dialplan"
)

// Paging / intercom group: page.lua.
//
// The dialplan sets destinations (or extension_list: "1000,1002,2000-2005"), optionally pin_number
// (comma separated), mute, auto_answer (call_info, sip_auto_answer or a header), alert_info,
// check_destination_status, moderator, and runs page.lua. Every destination is rung with an
// auto-answer call that joins the conference "page-<dialled number>@<domain>" (muted when mute=true:
// the paged phones only listen); the caller joins it as the moderator: when the caller hangs up
// the page ends. When nobody could be paged the caller hears a busy tone.

func init() { RegisterScript("page", scriptPage) }

// pageNumbers expands "1000" and "1000-1005" (leading zeros keep their length).
func pageNumbers(value string) ([]string, error) {
	begin, end, isRange := strings.Cut(strings.TrimSpace(value), "-")
	if !isRange || begin == end {
		return []string{begin}, nil
	}
	if strings.HasPrefix(begin, "0") && len(begin) != len(end) {
		return nil, fmt.Errorf("number in range with leading `0` should have same length: %s", value)
	}
	b, err1 := strconv.Atoi(begin)
	e, err2 := strconv.Atoi(end)
	if err1 != nil || err2 != nil || b > e {
		return nil, fmt.Errorf("invalid range: %s", value)
	}
	if e-b > 1000 {
		return nil, fmt.Errorf("range too large: %s", value)
	}
	var out []string
	for n := b; n <= e; n++ {
		out = append(out, fmt.Sprintf("%0*d", len(begin), n))
	}
	return out, nil
}

func scriptPage(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	_ = s.Answer()
	destNumber, pin, domain := s.Var("destination_number"), s.Var("pin_number"), s.Var("domain_name")
	destinations := firstNonEmpty(s.Var("destinations"), s.Var("extension_list"))
	rtpSecure := firstNonEmpty(s.Var("rtp_secure_media"), "false")
	callerName, callerNum := firstNonEmpty(s.Var("caller_id_name"), s.Var("effective_caller_id_name")), firstNonEmpty(s.Var("caller_id_number"), s.Var("effective_caller_id_number"))
	fromUser := s.Var("sip_from_user")
	mute := s.Var("mute")
	checkStatus := firstNonEmpty(s.Var("check_destination_status"), "false")

	autoAnswer := firstNonEmpty(s.Var("auto_answer"), "call_info")
	switch autoAnswer {
	case "call_info":
		autoAnswer = "sip_h_Call-Info=<sip:" + domain + ">;answer-after=0"
	case "sip_auto_answer":
		autoAnswer = "sip_auto_answer=true"
	}
	alertInfo := firstNonEmpty(s.Var("alert_info"), "ring_answer")
	switch alertInfo {
	case "auto_answer":
		alertInfo = "sip_h_Alert-Info='Auto Answer'"
	case "ring_answer":
		alertInfo = "sip_h_Alert-Info='Ring Answer'"
	default:
		alertInfo = "sip_h_Alert-Info='" + chanSafe(alertInfo) + "'"
	}

	room := "page-" + destNumber + "@" + domain
	bridge := room + "@page"
	flags := "flags{}"
	if mute == "true" {
		flags = "flags{mute}"
	}

	// the PIN
	if pin != "" {
		s.Sleep(500)
		digits := s.GetDigits(2, 20, 3, 3000, "phrase:voicemail_enter_pass:#", `\d+`)
		ok := false
		for _, p := range splitList(pin, ",") {
			if digits != "" && digits == p {
				ok = true
				s.SetVar("pin_number", p)
				break
			}
		}
		if !ok {
			s.PlaySound("phrase:voicemail_fail_auth:#")
			s.Hangup("NORMAL_CLEARING")
			return nil
		}
	}
	s.Log("info", "page", "destinations", destinations)

	// who is busy (check_destination_status)
	busy := func(dest string) bool {
		if checkStatus != "true" {
			return false
		}
		for _, ch := range c.Channels() {
			if ch != s.Ch && !ch.IsHungUp() && strings.EqualFold(ch.Var("presence_id"), dest+"@"+domain) {
				return true
			}
		}
		return false
	}

	count := 0
	for _, item := range splitList(destinations, ",") {
		nums, err := pageNumbers(item)
		if err != nil {
			return err
		}
		for _, n := range nums {
			dest := c.userID(n, domain) // number alias: the extension it is for
			if dest == fromUser {
				continue // the caller is not paged
			}
			if busy(dest) {
				continue
			}
			dial := fmt.Sprintf("{%s,%s,hangup_after_bridge=false,rtp_secure_media=%s,origination_caller_id_name='%s',origination_caller_id_number=%s}user/%s@%s",
				autoAnswer, alertInfo, rtpSecure, chanSafe(callerName), chanSafe(callerNum), dest, domain)
			go c.pageLeg(dial, bridge+"+"+flags)
			count++
		}
	}

	if count > 0 {
		mod := ""
		if s.Var("moderator") == "true" {
			mod = ",moderator"
		}
		return s.Execute("conference", bridge+"+flags{endconf,mintwo"+mod+"}")
	}
	return s.Execute("playback", "tone_stream://%(500,500,480,620);loops=3")
}

// pageLeg is the bgapi originate of page.lua: the call to one phone joins the page conference.
func (c *Core) pageLeg(dial, conf string) {
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	b, cause := c.originate(ctx, nil, dial, map[string]string{"call_timeout": "60"})
	if b == nil {
		c.log.Info("page: a phone did not answer", "cause", cause.String())
		return
	}
	c.runExecutorWith(b, []dialplan.Action{{App: "conference", Data: conf}})
}
