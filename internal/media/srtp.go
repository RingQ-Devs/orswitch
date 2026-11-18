package media

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/pion/srtp/v3"
)

// SRTP (RFC 3711) with keys exchanged in SDP (SDES, RFC 4568). An endpoint decrypts what the far end sends with the
// far end's key and encrypts what it sends with its own, so each leg of a bridged call is secured on its own: one
// leg can be SRTP and the other plain RTP. RTCP of a secured endpoint is neither generated nor relayed.

// The two suites of RFC 4568 that every SDES phone supports.
const (
	SRTPSuite80 = "AES_CM_128_HMAC_SHA1_80"
	SRTPSuite32 = "AES_CM_128_HMAC_SHA1_32"
)

// SRTPKey is the keying of one direction: a 16 byte master key and a 14 byte master salt.
type SRTPKey struct {
	Suite  string
	Master []byte
	Salt   []byte
}

func srtpProfile(suite string) (srtp.ProtectionProfile, bool) {
	switch strings.ToUpper(suite) {
	case SRTPSuite80:
		return srtp.ProtectionProfileAes128CmHmacSha1_80, true
	case SRTPSuite32:
		return srtp.ProtectionProfileAes128CmHmacSha1_32, true
	}
	return 0, false
}

// SupportedSRTPSuite reports whether we can run suite.
func SupportedSRTPSuite(suite string) bool {
	_, ok := srtpProfile(suite)
	return ok
}

// NewSRTPKey makes a random key for suite.
func NewSRTPKey(suite string) (*SRTPKey, error) {
	if !SupportedSRTPSuite(suite) {
		return nil, fmt.Errorf("srtp: unsupported suite %q", suite)
	}
	k := &SRTPKey{Suite: strings.ToUpper(suite), Master: make([]byte, 16), Salt: make([]byte, 14)}
	if _, err := rand.Read(k.Master); err != nil {
		return nil, err
	}
	if _, err := rand.Read(k.Salt); err != nil {
		return nil, err
	}
	return k, nil
}

// ParseSRTPKey reads the base64 key||salt of an SDES "inline:" parameter.
func ParseSRTPKey(suite, inline string) (*SRTPKey, error) {
	if !SupportedSRTPSuite(suite) {
		return nil, fmt.Errorf("srtp: unsupported suite %q", suite)
	}
	inline = strings.TrimSpace(inline)
	b, err := base64.StdEncoding.DecodeString(inline)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(inline); err != nil {
			return nil, errors.New("srtp: key is not base64")
		}
	}
	if len(b) != 30 {
		return nil, fmt.Errorf("srtp: key and salt are %d bytes, want 30", len(b))
	}
	return &SRTPKey{Suite: strings.ToUpper(suite), Master: b[:16], Salt: b[16:]}, nil
}

// Inline is the base64 key||salt for an SDP "inline:" parameter.
func (k *SRTPKey) Inline() string {
	return base64.StdEncoding.EncodeToString(append(append([]byte(nil), k.Master...), k.Salt...))
}

func (k *SRTPKey) same(o *SRTPKey) bool {
	return k != nil && o != nil && k.Suite == o.Suite && string(k.Master) == string(o.Master) && string(k.Salt) == string(o.Salt)
}

func (k *SRTPKey) context() (*srtp.Context, error) {
	p, ok := srtpProfile(k.Suite)
	if !ok {
		return nil, fmt.Errorf("srtp: unsupported suite %q", k.Suite)
	}
	return srtp.CreateContext(k.Master, k.Salt, p)
}

// SetSRTP secures the endpoint: remote is the key the far end encrypts with, local the key we encrypt with; nil
// switches that direction back to plain RTP. A key that did not change keeps its state (replay window, rollover
// counter), so a re-INVITE that repeats the same SDP does not disturb the stream.
func (e *Endpoint) SetSRTP(local, remote *SRTPKey) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !remote.same(e.sInKey) {
		e.sIn, e.sInKey = nil, nil
		if remote != nil {
			ctx, err := remote.context()
			if err != nil {
				return err
			}
			e.sIn, e.sInKey = ctx, remote
		}
	}
	if !local.same(e.sOutKey) {
		e.sOut, e.sOutKey = nil, nil
		if local != nil {
			ctx, err := local.context()
			if err != nil {
				return err
			}
			e.sOut, e.sOutKey = ctx, local
		}
	}
	return nil
}

// Secure reports whether the endpoint runs SRTP in either direction.
func (e *Endpoint) Secure() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sIn != nil || e.sOut != nil
}

// protect encrypts an outgoing RTP packet when the endpoint sends SRTP.
func (e *Endpoint) protect(pkt []byte) ([]byte, bool) {
	e.mu.Lock()
	ctx := e.sOut
	e.mu.Unlock()
	if ctx == nil {
		return pkt, true
	}
	e.smu.Lock()
	defer e.smu.Unlock()
	enc, err := ctx.EncryptRTP(nil, pkt, nil)
	if err != nil {
		return nil, false
	}
	return enc, true
}
