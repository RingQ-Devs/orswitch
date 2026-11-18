package core

import (
	"regexp"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/logbus"
)

func TestSessionPeak(t *testing.T) {
	var p sessionPeak
	t0 := time.Unix(1_700_000_000, 0)
	p.note(3, t0)
	p.note(7, t0.Add(30*time.Second))
	p.note(2, t0.Add(2*time.Minute))
	all, last := p.get(t0.Add(2 * time.Minute))
	if all != 7 || last != 7 {
		t.Fatalf("peak = %d / %d, want 7 / 7", all, last)
	}
	all, last = p.get(t0.Add(10 * time.Minute))
	if all != 7 || last != 0 {
		t.Fatalf("after ten minutes peak = %d / %d, want 7 / 0", all, last)
	}
}

func TestFSChannelName(t *testing.T) {
	cases := map[string]string{
		"ringq/gateway/2b70e958-8135-4b93-b44b-ac06b50bea62/0065123": "sofia/gateway/2b70e958-8135-4b93-b44b-ac06b50bea62/0065123",
		"ringq/internal/1001@dom":                                    "ringq/internal/1001@dom",
		"sip/1000@1.2.3.4":                                           "sip/1000@1.2.3.4",
	}
	for in, want := range cases {
		if got := fsChannelName(in); got != want {
			t.Errorf("fsChannelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The lines RingQ Core's fail2ban filters and Call Debug look for.
func TestFSLogLines(t *testing.T) {
	now := time.Date(2026, 11, 14, 3, 12, 7, 123456000, time.UTC)
	line := func(text, file string, num int) logbus.Line {
		return logbus.Line{Time: now, Level: 4, File: file, Num: num, Text: text}
	}
	// regular expressions of fail2ban's filters in RingQ's jobs.sh
	authFailure := regexp.MustCompile(`\[WARNING\] sofia_reg\.c:\d+ SIP auth failure \(REGISTER\) on sofia profile '.*' for \[.*\] from ip 203\.0\.113\.9`)
	invite := regexp.MustCompile(`\[INFO\] sofia\.c:\d+ receiving invite from 203\.0\.113\.9:5060`)
	challenge := regexp.MustCompile(`\[WARNING\] sofia_reg\.c:\d+ SIP auth challenge \(INVITE\) on sofia profile 'internal' for \[900@sw\] from ip 203\.0\.113\.9`)

	got := fsLogLine(line(`authentication failed (wrong user or password) method=REGISTER user=1001 src=udp:203.0.113.9:5060 failures="1/10 per 1m0s"`, "registrar.go", 351), "sw.example")
	if !authFailure.MatchString(got) || got[len(got)-1] != '\n' {
		t.Errorf("auth failure line = %q", got)
	}
	if !regexp.MustCompile(`2026-11-14 03:12:07\.123456`).MatchString(got) {
		t.Errorf("timestamp missing in %q", got)
	}
	l := line("sip/1000@203.0.113.9 receiving invite from 203.0.113.9:5060 version: Zoiper call-id: abc", "inbound.go", 182)
	l.Level = 6
	if got := fsLogLine(l, ""); !invite.MatchString(got) {
		t.Errorf("invite line = %q", got)
	}
	if got := fsLogLine(line("SIP auth challenge (INVITE) for [900@sw] from ip 203.0.113.9", "registrar.go", 326), ""); !challenge.MatchString(got) {
		t.Errorf("challenge line = %q", got)
	}
	// any other line keeps its text, file and level
	l = line("registered user=1001", "registrar.go", 464)
	l.Level = 6
	if got := fsLogLine(l, ""); !regexp.MustCompile(`\[INFO\] registrar\.go:464 registered user=1001\n$`).MatchString(got) {
		t.Errorf("plain line = %q", got)
	}
	if got := fsLogLine(logbus.Line{Raw: true, Text: "Dialplan: x Regex (PASS)"}, ""); got != "Dialplan: x Regex (PASS)\n" {
		t.Errorf("raw line = %q", got)
	}
}
