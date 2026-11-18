package main

import (
	"flag"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

var (
	answerMode = flag.Bool("answer", false, "wait for a call from the switch instead of placing one")
	waitFor    = flag.Duration("wait", 30*time.Second, "with -answer: how long to wait for the call")
)

// respond sends a response to a received request.
func (c *client) respond(req *msg, code int, text string, extra []string, body string) {
	h := []string{}
	for _, v := range req.hdr["via"] {
		h = append(h, "Via: "+v)
	}
	to := req.get("To")
	if !strings.Contains(to, "tag=") {
		to += ";tag=" + c.tag
	}
	h = append(h, "From: "+req.get("From"), "To: "+to, "Call-ID: "+req.get("Call-ID"), "CSeq: "+req.get("CSeq"),
		"Contact: <sip:"+*ext+"@"+c.via+";transport=ws>")
	h = append(h, extra...)
	c.send(fmt.Sprintf("SIP/2.0 %d %s", code, text), h, body)
}

// answerCall waits for an INVITE with a WebRTC offer, answers it, talks, and
// waits for the switch to hang up.
func answerCall(c *client) {
	fmt.Println("     waiting for a call to", *ext, "...")
	inv := c.wait(*waitFor, func(m *msg) bool { return strings.HasPrefix(m.first, "INVITE ") })
	if inv == nil {
		fail("no call arrived within %s", *waitFor)
	}
	fmt.Println("OK   INVITE received from the switch")
	if !strings.Contains(inv.body, "UDP/TLS/RTP/SAVPF") || !strings.Contains(inv.body, "a=fingerprint") {
		fail("the switch's offer is not WebRTC SDP:\n%s", inv.body)
	}
	fmt.Println("OK   offer is WebRTC SDP (DTLS-SRTP, ICE)")
	c.respond(inv, 180, "Ringing", nil, "")

	me := &webrtc.MediaEngine{}
	for _, cp := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, PayloadType: 0},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMA, ClockRate: 8000, Channels: 1}, PayloadType: 8},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "audio/telephone-event", ClockRate: 8000, Channels: 1}, PayloadType: 126},
	} {
		if err := me.RegisterCodec(cp, webrtc.RTPCodecTypeAudio); err != nil {
			fail("codec: %v", err)
		}
	}
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(0)
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fail("peer connection: %v", err)
	}
	track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "wssclient")
	var rx, voiced atomic.Int64
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			rx.Add(1)
			for _, b := range pkt.Payload {
				if b != 0xff && b != 0x7f {
					voiced.Add(1)
					break
				}
			}
		}
	})
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Println("     peer connection:", s)
		if s == webrtc.PeerConnectionStateConnected {
			close(connected)
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: inv.body}); err != nil {
		fail("remote offer: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		fail("add track: %v", err)
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		fail("create answer: %v", err)
	}
	if err := pc.SetLocalDescription(ans); err != nil {
		fail("local answer: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	c.respond(inv, 200, "OK", []string{"Content-Type: application/sdp"}, pc.LocalDescription().SDP)
	fmt.Println("OK   call answered with a WebRTC answer")

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		fail("ICE/DTLS did not connect within 10 s")
	}
	fmt.Println("OK   ICE and DTLS-SRTP connected")

	// Send tone (the call is a bridge to &echo, which returns it).
	phase := 0
	stop := time.After(*hold)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for talking := true; talking; {
		select {
		case <-stop:
			talking = false
		case <-tick.C:
			frame := make([]byte, 160)
			for i := range frame {
				frame[i] = 0x2a + byte((phase/4)%2)*0x30 // a square-ish pattern, not silence
				phase++
			}
			_ = track.WriteSample(media.Sample{Data: frame, Duration: 20 * time.Millisecond})
		}
	}
	fmt.Printf("     received %d RTP packets, %d with audio\n", rx.Load(), voiced.Load())
	if voiced.Load() < 20 {
		fail("no audio from the switch")
	}
	fmt.Println("OK   audio arrives from the switch")

	// Until the switch hangs up: answer re-INVITEs (hold and resume from the
	// other party) and the final BYE.
	deadline := time.After(25 * time.Second)
	for {
		var m *msg
		var ok bool
		select {
		case m, ok = <-c.in:
			if !ok {
				fail("the websocket closed")
			}
		case <-deadline:
			fail("the switch did not hang up")
		}
		switch {
		case strings.HasPrefix(m.first, "INVITE "):
			dir := "?"
			for _, l := range strings.Split(m.body, "\n") {
				switch l = strings.TrimSpace(l); l {
				case "a=sendrecv", "a=sendonly", "a=recvonly", "a=inactive":
					dir = l
				}
			}
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.body}); err != nil {
				fail("re-INVITE offer: %v", err)
			}
			a, err := pc.CreateAnswer(nil)
			if err != nil {
				fail("re-INVITE answer: %v", err)
			}
			if err := pc.SetLocalDescription(a); err != nil {
				fail("re-INVITE local answer: %v", err)
			}
			<-webrtc.GatheringCompletePromise(pc)
			c.respond(m, 200, "OK", []string{"Content-Type: application/sdp"}, pc.LocalDescription().SDP)
			fmt.Println("OK   re-INVITE from the switch answered; its offer direction:", dir)
		case strings.HasPrefix(m.first, "BYE "):
			c.respond(m, 200, "OK", nil, "")
			fmt.Println("OK   BYE from the switch answered")
			_ = pc.Close()
			fmt.Println("PASS")
			return
		}
	}
}
