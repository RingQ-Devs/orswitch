package sdp

import (
	"strconv"
	"strings"
)

// Crypto is an SDES key line, a=crypto:<tag> <suite> inline:<base64 key||salt>[|lifetime|MKI] (RFC 4568).
type Crypto struct {
	Tag   int
	Suite string
	Key   string // the base64 key and salt, without "inline:" and the lifetime
}

// IsSecure reports whether the stream is secured RTP (RTP/SAVP, RTP/SAVPF).
func (m *Media) IsSecure() bool { return strings.Contains(strings.ToUpper(m.Proto), "SAVP") }

// Cryptos returns the key lines of m in offered order. Lines in a form we do not read (key methods other than
// inline) are skipped.
func (m *Media) Cryptos() []Crypto {
	var out []Crypto
	for _, a := range m.Attributes {
		name, val, ok := strings.Cut(a, ":")
		if !ok || name != "crypto" {
			continue
		}
		f := strings.Fields(val)
		if len(f) < 3 {
			continue
		}
		tag, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		method, params, ok := strings.Cut(f[2], ":")
		if !ok || method != "inline" {
			continue
		}
		key, _, _ := strings.Cut(params, "|")
		out = append(out, Crypto{Tag: tag, Suite: strings.ToUpper(f[1]), Key: key})
	}
	return out
}

// SetSecure turns m into secured RTP carrying c as its only key line, or back into plain RTP/AVP when c is nil.
func (m *Media) SetSecure(c *Crypto) {
	var keep []string
	for _, a := range m.Attributes {
		if !strings.HasPrefix(a, "crypto:") {
			keep = append(keep, a)
		}
	}
	m.Attributes = keep
	if c == nil {
		m.Proto = "RTP/AVP"
		return
	}
	m.Proto = "RTP/SAVP"
	m.Attributes = append(m.Attributes, "crypto:"+strconv.Itoa(c.Tag)+" "+c.Suite+" inline:"+c.Key)
}
