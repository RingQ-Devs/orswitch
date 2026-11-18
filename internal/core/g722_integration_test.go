package core_test

import (
	"strings"
	"testing"
	"time"
)

func g722Call(t *testing.T, prefs string) (sdpBody string, rtpPT int) {
	t.Helper()
	dp := aiDialplan(`
  <extension name="g722"><condition field="destination_number" expression="^9500$">
    <action application="answer"/>
    <action application="playback" data="tone_stream://%(2000,0,440);loops=-1"/>
  </condition></extension>`)
	extra := ""
	if prefs != "" {
		extra = `<param name="codec-prefs" value="` + prefs + `"/>`
	}
	h := newHarnessWith(t, extra, twoUsers(), dp, nil)
	alice := h.phone("1000")
	call, err := alice.Dial("9500", []int{9, 0}, 5*time.Second, false) // a G.722 phone: it prefers G.722
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", err)
	}
	defer func() { _, _ = call.Bye() }()
	_ = call.RTP.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2000)
	n, err := call.RTP.Read(buf)
	if err != nil || n < 13 {
		t.Fatalf("no media: %v", err)
	}
	return string(call.Final.Body), int(buf[1] & 0x7F)
}

// G.722 is opt-in: with the default codec-prefs a phone that prefers G.722 still gets G.711, and with
// codec-prefs = G722,... the switch answers G.722 and its own audio (tones, prompts) goes out as G.722.
func TestG722NegotiationIsOptIn(t *testing.T) {
	body, pt := g722Call(t, "")
	if strings.Contains(body, "G722") || pt != 0 {
		t.Fatalf("default prefs: answer %q, media PT %d, want PCMU only", body, pt)
	}
	body, pt = g722Call(t, "G722,PCMU,PCMA")
	if !strings.Contains(body, "a=rtpmap:9 G722/8000") || pt != 9 {
		t.Fatalf("codec-prefs with G722: answer %q, media PT %d, want G722 (PT 9)", body, pt)
	}
}
