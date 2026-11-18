package db

import (
	"testing"

	"github.com/ringq-devs/orswitch/internal/config"
)

func TestApplyProfileAddresses(t *testing.T) {
	mk := func(on bool) *config.Config {
		c := &config.Config{Profiles: []config.SIPProfile{
			{Name: "external", Enabled: true, ExtSIPIP: "9.9.9.9"},
			{Name: "internal", Enabled: true, ExtSIPIP: "203.0.113.5"},
		}}
		c.Settings.DBAddresses = on
		c.Settings.ExtSIPIP, c.Settings.ExtRTPIP = "1.1.1.1", "1.1.1.1"
		return c
	}
	off := mk(false)
	applyProfileAddresses(off)
	if off.Settings.ExtSIPIP != "1.1.1.1" {
		t.Fatalf("addresses changed while the setting is off: %s", off.Settings.ExtSIPIP)
	}
	on := mk(true)
	applyProfileAddresses(on)
	if on.Settings.ExtSIPIP != "203.0.113.5" || on.Settings.ExtRTPIP != "203.0.113.5" {
		t.Fatalf("addresses = %s / %s, want the internal profile's", on.Settings.ExtSIPIP, on.Settings.ExtRTPIP)
	}
	on = mk(true)
	on.Profiles[1].ExtRTPIP = "203.0.113.6"
	applyProfileAddresses(on)
	if on.Settings.ExtRTPIP != "203.0.113.6" {
		t.Fatalf("ext-rtp-ip = %s", on.Settings.ExtRTPIP)
	}
}
