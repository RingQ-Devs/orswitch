package core

import (
	"context"
	"testing"
)

func testCtx() context.Context { return context.Background() }

func TestTollTypeTemplates(t *testing.T) {
	for _, c := range []struct{ country, prefix, number, want string }{
		{"IT", "", "3401234567", "mobile"},
		{"IT", "", "0612345678", "landline"},
		{"IT", "", "0044123456", "international"},
		{"IT", "", "800123456", "tollfree"},
		{"IT", "", "899123456", "premium"},
		{"IT", "", "1", ""},
		{"FR", "", "0612345678", "mobile"},
		{"FR", "", "0142424242", "landline"},
		{"FR", "9", "90612345678", "mobile"}, // the outbound prefix is part of the pattern
		{"US", "", "2125551234", "unknown"},
		{"ZA", "", "0821234567", "mobile"},
		{"RU", "", "88001234567", "tollfree"},
	} {
		if got := tollType(c.prefix, tollTemplates[c.country], c.number); got != c.want {
			t.Errorf("%s %q%q: %q, want %q", c.country, c.prefix, c.number, got, c.want)
		}
	}
}

func TestMailHelpers(t *testing.T) {
	if got := compactBody("a  b\n\tc \r\n d"); got != "a  bc  d" {
		t.Errorf("compactBody %q", got)
	}
	if got := fillTemplate("${a} and ${b}${a}", map[string]string{"a": "1", "b": "2"}); got != "1 and 21" {
		t.Errorf("fillTemplate %q", got)
	}
	if got := htmlQuote(`it's "x"`); got != "it&#39;s &#34;x&#34;" {
		t.Errorf("htmlQuote %q", got)
	}
	if got := mimeSubject(" hi "); got != "=?utf-8?B?aGk=?=" {
		t.Errorf("mimeSubject %q", got)
	}
	if got := vmFormatSeconds(75); got != "1:15" {
		t.Errorf("vmFormatSeconds %q", got)
	}
	if got := vmFormatSeconds(3725); got != "1:02:05" {
		t.Errorf("vmFormatSeconds %q", got)
	}
	if got := haURLEncode("delete dialplan:x/1"); got != "delete%20dialplan%3Ax%2F1" {
		t.Errorf("haURLEncode %q", got)
	}
	if got := haURLEncode("delete a"); got != "delete%20a" {
		t.Errorf("haURLEncode %q", got)
	}
}

func TestScriptRegistry(t *testing.T) {
	for _, n := range []string{"disa", "disa.lua", "disa.plus.lua", "disa.callback", "disa.callback.loopback.lua", "wakeup.lua", "recordings.lua",
		"ha_monitor.lua", "callcenter-announce-messages.lua", "callcenter-announce-position.lua", "email.lua", "roku.lua", "handler.lua",
		"app_agent_status", "app_avmd", "app_call_block", "app_call_control", "app_caller_id", "app_dialplan", "app_emergency",
		"app_emergency_notify", "app_event_notify", "app_hangup", "app_is_local", "app_speed_dial", "app_toll_allow", "app_valet_park",
		"app_conference_center", "app_provision", "conference.lua", "mute.lua", "unmute.lua", "raise_hand.lua", "start_recording.lua",
		"transfer.lua", "hangup_rx.lua", "hangup_tx.lua", "clear_cache.lua", "emergency_calls.lua"} {
		if !hasScript(n) {
			t.Errorf("script %s is not registered", n)
		}
	}
}
