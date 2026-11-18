package core

import "testing"

// The log names a script, application or API command only when the switch knows it, and never the text around it.
func TestLogNames(t *testing.T) {
	if got := knownScript("ringq_desk_aux.lua"); got != "ringq_desk_aux" {
		t.Errorf("a registered script: %q", got)
	}
	if got := knownScript("secret-pass-1234"); got != "(unknown)" {
		t.Errorf("an unknown script must not be echoed: %q", got)
	}
	if got := knownAPI("hostname"); got != "hostname" {
		t.Errorf("a known API command: %q", got)
	}
	if got := knownAPI("hunter2"); got != "(unknown)" {
		t.Errorf("an unknown command must not be echoed: %q", got)
	}
	c := &Core{apps: map[string]App{"hangup": nil}}
	if got := c.knownApp("hangup"); got != "hangup" || c.knownApp("pw=abc") != "(unknown)" {
		t.Errorf("applications: %q %q", got, c.knownApp("pw=abc"))
	}
	for tmpl, want := range map[string]string{
		"lua ringq_desk_aux 1 ${sip_auth_password}":   "lua ringq_desk_aux",
		"luarun nosuch.lua ${password}":               "lua (unknown)",
		"hostname ${password}":                        "api hostname",
		"curl http://x/?pw=${password}":               "api (unknown)",
		"":                                            "",
	} {
		if got := hookLabel(tmpl); got != want {
			t.Errorf("hookLabel(%q) = %q, want %q", tmpl, got, want)
		}
	}
}
