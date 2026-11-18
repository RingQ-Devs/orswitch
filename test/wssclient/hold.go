package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

var holdTest = flag.Bool("holdtest", false, "put the call on hold and resume it with re-INVITEs, as sip.js does (use -talk 9s or more)")

// reinvite sends a re-INVITE whose SDP has the given direction (sip.js makes
// hold by rewriting the direction of its own offer) and applies the answer.
func reinvite(c *client, pc *webrtc.PeerConnection, callID, uri, to, direction string) (*msg, error) {
	// The wire SDP is the call's own offer with its direction changed. (pion
	// refuses such edits to its own description, so the client's peer
	// connection is left alone: only the switch's handling is under test.)
	sdpText := pc.LocalDescription().SDP
	for _, d := range []string{"a=sendrecv", "a=sendonly", "a=recvonly", "a=inactive"} {
		sdpText = strings.ReplaceAll(sdpText, d, "a="+direction)
	}
	h := c.common("INVITE", callID)
	cseq := strconv.Itoa(c.cseq)
	h = append(h, "To: "+to, "Content-Type: application/sdp")
	c.send("INVITE "+uri+" SIP/2.0", h, sdpText)
	r := c.wait(8*time.Second, func(m *msg) bool {
		return m.status() >= 200 && strings.HasPrefix(m.get("CSeq"), cseq+" INVITE")
	})
	if r == nil || r.status() != 200 {
		return r, fmt.Errorf("re-INVITE (%s) got %v", direction, r)
	}
	if !strings.Contains(r.body, "UDP/TLS/RTP/SAVPF") {
		return r, fmt.Errorf("the answer to the re-INVITE is not WebRTC SDP:\n%s", r.body)
	}
	c.send("ACK "+uri+" SIP/2.0", []string{
		"Via: SIP/2.0/WSS " + c.via + ";branch=z9hG4bK" + token(6), "Max-Forwards: 70",
		"From: <sip:" + *ext + "@" + c.domain + ">;tag=" + c.tag, "To: " + to,
		"CSeq: " + cseq + " ACK", "Call-ID: " + callID,
	}, "")
	return r, nil
}

// holdSequence: hold, wait, resume; reports the media in each phase.
func holdSequence(c *client, pc *webrtc.PeerConnection, callID, uri, to string, rx *atomic.Int64) {
	time.Sleep(1500 * time.Millisecond)
	before := rx.Load()
	r, err := reinvite(c, pc, callID, uri, to, "sendonly")
	if err != nil {
		fail("hold: %v", err)
	}
	dirs := []string{}
	for _, l := range strings.Split(r.body, "\n") {
		l = strings.TrimSpace(l)
		if l == "a=recvonly" || l == "a=sendonly" || l == "a=inactive" || l == "a=sendrecv" {
			dirs = append(dirs, l)
		}
	}
	fmt.Println("OK   hold re-INVITE answered:", r.first, dirs)
	time.Sleep(2 * time.Second)
	onHold := rx.Load() - before
	r, err = reinvite(c, pc, callID, uri, to, "sendrecv")
	if err != nil {
		fail("resume: %v", err)
	}
	fmt.Println("OK   resume re-INVITE answered:", r.first)
	atResume := rx.Load()
	time.Sleep(2 * time.Second)
	after := rx.Load() - atResume
	fmt.Printf("     RTP packets received: while on hold %d, after resume %d\n", onHold, after)
	if after < 20 {
		fail("no audio after resume")
	}
	fmt.Println("OK   audio flows again after resume")
}
