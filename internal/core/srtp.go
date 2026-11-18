package core

import (
	"strings"

	"github.com/ringq-devs/orswitch/internal/media"
	"github.com/ringq-devs/orswitch/internal/sdp"
)

// SRTP with SDES key exchange (RFC 4568). Each leg of a call has its own media endpoint and so its own keys: a
// phone's SRTP leg can be bridged to a plain RTP trunk and the other way round.
//
//   - A caller that offers RTP/SAVP with an a=crypto line it can use gets SRTP in the answer (setting srtp=optional,
//     the default; srtp=off refuses such offers with 488, as before).
//   - The legs we start are plain RTP unless the dialplan sets rtp_secure_media to true or mandatory (or names a suite,
//     AES_CM_128_HMAC_SHA1_80 / _32): the INVITE then offers RTP/SAVP, and an answer that is not SRTP ends the leg.
//
// Not done: SRTCP, other key exchanges (DTLS-SRTP is what the WebRTC gateway uses), more than one suite per offer.

// srtpState is the SRTP side of one leg.
type srtpState struct {
	suite     string
	tag       int
	local     *media.SRTPKey // what we encrypt with
	mandatory bool           // we offered SRTP: a plain answer fails the leg
}

// srtpSetting is the srtp setting: "optional" (default) or "off".
func (c *Core) srtpEnabled() bool {
	return !strings.EqualFold(c.config().Settings.SRTP, "off")
}

// hasUsableCrypto reports whether m offers a key line with a suite and key we can run.
func hasUsableCrypto(m *sdp.Media) bool {
	for _, c := range m.Cryptos() {
		if !media.SupportedSRTPSuite(c.Suite) {
			continue
		}
		if _, err := media.ParseSRTPKey(c.Suite, c.Key); err == nil {
			return true
		}
	}
	return false
}

// acceptSecure takes the first key line of an SRTP offer that we can run. It reports false when there is none.
func (ch *Channel) acceptSecure(m *sdp.Media) bool {
	for _, c := range m.Cryptos() {
		if !media.SupportedSRTPSuite(c.Suite) {
			continue
		}
		if _, err := media.ParseSRTPKey(c.Suite, c.Key); err != nil {
			continue
		}
		local, err := media.NewSRTPKey(c.Suite)
		if err != nil {
			return false
		}
		ch.mu.Lock()
		ch.srtp = &srtpState{suite: strings.ToUpper(c.Suite), tag: c.Tag, local: local}
		ch.mu.Unlock()
		return true
	}
	return false
}

// offerSecure makes this leg an SRTP leg that we offer (a leg we start). value is the rtp_secure_media variable:
// true or mandatory use AES_CM_128_HMAC_SHA1_80; a suite name chooses that one. It reports whether SRTP is on.
func (ch *Channel) offerSecure(value string) bool {
	suite := media.SRTPSuite80
	switch v := strings.ToLower(strings.TrimSpace(value)); {
	case v == "" || v == "false" || v == "no" || v == "0" || v == "off" || v == "optional":
		// "optional" would offer RTP/AVP with key lines, which not every phone survives: plain RTP
		return false
	case strings.Contains(v, "sha1_32"):
		suite = media.SRTPSuite32
	}
	local, err := media.NewSRTPKey(suite)
	if err != nil {
		return false
	}
	ch.mu.Lock()
	ch.srtp = &srtpState{suite: suite, tag: 1, local: local, mandatory: true}
	ch.mu.Unlock()
	return true
}

// secureLocal turns the audio stream of an SDP we send into SRTP with this leg's key, when the leg is secured.
// It is safe to call more than once.
func (ch *Channel) secureLocal(s *sdp.Session) {
	if s == nil {
		return
	}
	ch.mu.Lock()
	st := ch.srtp
	ch.mu.Unlock()
	if st == nil {
		return
	}
	if m, _ := s.Audio(); m != nil {
		m.SetSecure(&sdp.Crypto{Tag: st.tag, Suite: st.suite, Key: st.local.Inline()})
	}
}

// applySRTP keys the endpoint from the far end's SDP. It is called whenever a remote SDP is applied; the same key
// again changes nothing.
func (ch *Channel) applySRTP(m *sdp.Media) {
	ch.mu.Lock()
	st := ch.srtp
	ch.mu.Unlock()
	if st == nil {
		return
	}
	var remote *media.SRTPKey
	if m.IsSecure() {
		for _, c := range m.Cryptos() {
			if c.Tag != st.tag || !strings.EqualFold(c.Suite, st.suite) {
				continue
			}
			if k, err := media.ParseSRTPKey(c.Suite, c.Key); err == nil {
				remote = k
				break
			}
		}
	}
	if remote == nil {
		if st.mandatory {
			// srtpAnswerUsable ends the leg when this is the final answer; early media without a key just stays silent
			return
		}
		ch.log.Warn("srtp: the far end's key line is unusable; media stays plain")
		_ = ch.ep.SetSRTP(nil, nil)
		ch.mu.Lock()
		ch.srtp = nil
		ch.mu.Unlock()
		return
	}
	if err := ch.ep.SetSRTP(st.local, remote); err != nil {
		ch.log.Warn("srtp: keying the media failed", "err", err)
	}
}

// srtpReoffer checks a re-INVITE's audio stream m against the leg: an SRTP leg must get an SRTP offer with a key
// line (the one in use, or a new one, which re-keys), a plain leg a plain offer.
func (ch *Channel) srtpReoffer(m *sdp.Media) bool {
	ch.mu.Lock()
	st := ch.srtp
	ch.mu.Unlock()
	if st == nil {
		return !m.IsSecure()
	}
	if !m.IsSecure() {
		return false
	}
	for _, c := range m.Cryptos() {
		if c.Tag == st.tag && strings.EqualFold(c.Suite, st.suite) {
			return true // the same line: keep our key, applyRemote takes the far end's
		}
	}
	return ch.acceptSecure(m) // a different line: new suite and key
}

// srtpAnswerUsable reports whether the final answer m to an SRTP offer of ours is SRTP with the key line we
// offered; a leg that asked for SRTP and did not get it must not be bridged as plain RTP.
func (ch *Channel) srtpAnswerUsable(m *sdp.Media) bool {
	ch.mu.Lock()
	st := ch.srtp
	ch.mu.Unlock()
	if st == nil || !st.mandatory {
		return true
	}
	if !m.IsSecure() {
		return false
	}
	for _, c := range m.Cryptos() {
		if c.Tag == st.tag && strings.EqualFold(c.Suite, st.suite) {
			_, err := media.ParseSRTPKey(c.Suite, c.Key)
			return err == nil
		}
	}
	return false
}
