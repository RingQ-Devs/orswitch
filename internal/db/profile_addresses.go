package db

import "github.com/ringq-devs/orswitch/internal/config"

// applyProfileAddresses makes the public address of the database's "internal" SIP
// profile the switch's own (profile-addresses = database): Global Settings >
// Network writes it there (ext-sip-ip, ext-rtp-ip), and on FreeSWITCH that is
// what phones and carriers are given.
func applyProfileAddresses(cfg *config.Config) {
	if !cfg.Settings.DBAddresses {
		return
	}
	for _, p := range cfg.Profiles {
		if p.Name != "internal" || !p.Enabled || p.ExtSIPIP == "" {
			continue
		}
		cfg.Settings.ExtSIPIP = p.ExtSIPIP
		cfg.Settings.ExtRTPIP = p.ExtSIPIP
		if p.ExtRTPIP != "" {
			cfg.Settings.ExtRTPIP = p.ExtRTPIP
		}
		return
	}
}
