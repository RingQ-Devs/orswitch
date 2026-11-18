// Command wssclient is a small WebRTC softphone for testing SIP over
// WebSocket: it registers an extension over wss:// (digest authentication),
// calls a number (default 9196, the echo test) with a WebRTC offer, sends one
// second of a tone as G.711 over ICE and DTLS-SRTP, checks that audio comes
// back, and hangs up.
//
//	go run ./test/wssclient -host switch.example.com -addr 127.0.0.1:7443 -ext 1000 -pw <sip password>
package main

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

var (
	host = flag.String("host", "switch.example.com", "SIP domain and TLS server name")
	addr = flag.String("addr", "", "host:port to connect to (default <host>:7443)")
	ext  = flag.String("ext", "1000", "extension")
	pw   = flag.String("pw", "", "SIP password of the extension")
	dest = flag.String("dest", "9196", "number to call")
	hold = flag.Duration("talk", 4*time.Second, "how long to talk")
	// linger keeps the registration up for a while (no call), to watch it.
	linger = flag.Duration("linger", 0, "register, stay connected this long, then exit (no call)")
)

func fail(f string, a ...any) {
	fmt.Printf("FAIL: "+f+"\n", a...)
	os.Exit(1)
}

func token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

type msg struct {
	first string
	hdr   map[string][]string
	body  string
}

func (m *msg) get(n string) string {
	if v := m.hdr[strings.ToLower(n)]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (m *msg) status() int {
	f := strings.Fields(m.first)
	if len(f) >= 2 && f[0] == "SIP/2.0" {
		n, _ := strconv.Atoi(f[1])
		return n
	}
	return 0
}

func parse(raw string) *msg {
	head, body, _ := strings.Cut(raw, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	m := &msg{first: lines[0], hdr: map[string][]string{}, body: body}
	for _, l := range lines[1:] {
		if k, v, ok := strings.Cut(l, ":"); ok {
			k = strings.ToLower(strings.TrimSpace(k))
			m.hdr[k] = append(m.hdr[k], strings.TrimSpace(v))
		}
	}
	return m
}

type client struct {
	ws     *websocket.Conn
	in     chan *msg
	domain string
	via    string // random .invalid sent-by, as browsers do
	tag    string
	cseq   int
}

func (c *client) send(first string, headers []string, body string) {
	var b strings.Builder
	b.WriteString(first + "\r\n")
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
	if err := c.ws.WriteMessage(websocket.TextMessage, []byte(b.String())); err != nil {
		fail("websocket write: %v", err)
	}
}

func (c *client) wait(d time.Duration, pred func(*msg) bool) *msg {
	t := time.After(d)
	for {
		select {
		case m := <-c.in:
			if pred(m) {
				return m
			}
		case <-t:
			return nil
		}
	}
}

func digest(method, uri, user, pass string, challenge string) string {
	get := func(k string) string {
		i := strings.Index(challenge, k+`="`)
		if i < 0 {
			return ""
		}
		r := challenge[i+len(k)+2:]
		return r[:strings.Index(r, `"`)]
	}
	realm, nonce := get("realm"), get("nonce")
	cn, nc := token(4), "00000001"
	resp := md5hex(strings.Join([]string{md5hex(user + ":" + realm + ":" + pass), nonce, nc, cn, "auth", md5hex(method + ":" + uri)}, ":"))
	return fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s", algorithm=MD5, cnonce="%s", qop=auth, nc=%s`, user, realm, nonce, uri, resp, cn, nc)
}

func (c *client) common(method, callID string) []string {
	c.cseq++
	return []string{
		"Via: SIP/2.0/WSS " + c.via + ";branch=z9hG4bK" + token(6),
		"Max-Forwards: 70",
		"From: <sip:" + *ext + "@" + c.domain + ">;tag=" + c.tag,
		"CSeq: " + strconv.Itoa(c.cseq) + " " + method,
		"Call-ID: " + callID,
		"Contact: <sip:" + *ext + "@" + c.via + ";transport=ws>",
		"User-Agent: wssclient",
	}
}

func main() {
	flag.Parse()
	if *pw == "" {
		fail("-pw is required")
	}
	target := *addr
	if target == "" {
		target = *host + ":7443"
	}
	d := websocket.Dialer{
		Subprotocols:    []string{"sip"},
		TLSClientConfig: &tls.Config{ServerName: *host},
		NetDial:         func(n, _ string) (net.Conn, error) { return net.DialTimeout(n, target, 5*time.Second) },
	}
	ws, resp, err := d.Dial("wss://"+*host+"/", http.Header{"Origin": {"https://" + *host}})
	if err != nil {
		fail("websocket connect: %v", err)
	}
	fmt.Println("OK   websocket connected, subprotocol:", resp.Header.Get("Sec-WebSocket-Protocol"))
	c := &client{ws: ws, in: make(chan *msg, 64), domain: *host, via: token(5) + ".invalid", tag: token(4)}
	go func() {
		for {
			_, b, err := ws.ReadMessage()
			if err != nil {
				close(c.in)
				return
			}
			if s := strings.TrimSpace(string(b)); s != "" {
				c.in <- parse(string(b))
			}
		}
	}()

	// --- REGISTER
	regCall := token(8)
	uri := "sip:" + *host
	doReg := func(auth string) *msg {
		h := c.common("REGISTER", regCall)
		h = append(h, "To: <sip:"+*ext+"@"+c.domain+">", "Expires: 300")
		if auth != "" {
			h = append(h, "Authorization: "+auth)
		}
		c.send("REGISTER "+uri+" SIP/2.0", h, "")
		return c.wait(5*time.Second, func(m *msg) bool { return m.status() >= 200 || m.status() == 401 })
	}
	r := doReg("")
	if r == nil || r.status() != 401 {
		fail("REGISTER: expected 401, got %v", r)
	}
	r = doReg(digest("REGISTER", uri, *ext, *pw, r.get("WWW-Authenticate")))
	if r == nil || r.status() != 200 {
		fail("REGISTER with digest: %v", r)
	}
	fmt.Println("OK   registered over WSS:", r.first)
	if *linger > 0 {
		time.Sleep(*linger)
		fmt.Println("PASS (lingered)")
		return
	}
	if *answerMode {
		answerCall(c)
		return
	}

	// --- WebRTC offer
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
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fail("peer connection: %v", err)
	}
	track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "wssclient")
	var raw *rawTrack
	if *dtmfDigits != "" {
		raw = &rawTrack{} // voice and telephone-event in one stream
		if _, err := pc.AddTrack(raw); err != nil {
			fail("add track: %v", err)
		}
	} else if _, err := pc.AddTrack(track); err != nil {
		fail("add track: %v", err)
	}
	var rxPackets, rxVoiced atomic.Int64
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			rxPackets.Add(1)
			for _, b := range pkt.Payload {
				if b != 0xff && b != 0x7f { // not μ-law silence
					rxVoiced.Add(1)
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
	offer, _ := pc.CreateOffer(nil)
	if err := pc.SetLocalDescription(offer); err != nil {
		fail("local description: %v", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	offerSDP := pc.LocalDescription().SDP

	// --- INVITE (with the digest challenge a registered phone gets)
	callID := token(8)
	inviteURI := "sip:" + *dest + "@" + *host
	invite := func(auth, hdrName string) *msg {
		h := c.common("INVITE", callID)
		h = append(h, "To: <"+inviteURI+">", "Content-Type: application/sdp", "Allow: INVITE, ACK, CANCEL, BYE, OPTIONS, INFO, NOTIFY")
		if auth != "" {
			h = append(h, hdrName+": "+auth)
		}
		c.send("INVITE "+inviteURI+" SIP/2.0", h, offerSDP)
		return c.wait(10*time.Second, func(m *msg) bool { return m.status() >= 200 })
	}
	ack := func(ok *msg, to string) {
		c.send("ACK "+inviteURI+" SIP/2.0", []string{
			"Via: SIP/2.0/WSS " + c.via + ";branch=z9hG4bK" + token(6), "Max-Forwards: 70",
			"From: <sip:" + *ext + "@" + c.domain + ">;tag=" + c.tag, "To: " + to,
			"CSeq: " + strings.Fields(ok.get("CSeq"))[0] + " ACK", "Call-ID: " + callID,
		}, "")
	}
	r = invite("", "")
	if r != nil && (r.status() == 401 || r.status() == 407) {
		ack(r, r.get("To"))
		hdr, chal := "Authorization", r.get("WWW-Authenticate")
		if r.status() == 407 {
			hdr, chal = "Proxy-Authorization", r.get("Proxy-Authenticate")
		}
		r = invite(digest("INVITE", inviteURI, *ext, *pw, chal), hdr)
	}
	if r == nil || r.status() != 200 {
		fail("INVITE: %v", r)
	}
	fmt.Println("OK   call answered:", r.first)
	if !strings.Contains(r.body, "UDP/TLS/RTP/SAVPF") || !strings.Contains(r.body, "a=fingerprint") {
		fail("the 200 OK answer is not a WebRTC description:\n%s", r.body)
	}
	fmt.Println("OK   answer is WebRTC SDP (DTLS-SRTP, ICE)")
	for _, l := range strings.Split(r.body, "\n") {
		if strings.HasPrefix(l, "a=candidate") || strings.HasPrefix(l, "m=audio") || strings.HasPrefix(l, "a=rtpmap") {
			fmt.Println("     sdp:", strings.TrimSpace(l))
		}
	}
	toHdr := r.get("To")
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: r.body}); err != nil {
		fail("remote description: %v", err)
	}
	ack(r, toHdr)

	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		fail("ICE/DTLS did not connect within 10 s")
	}
	fmt.Println("OK   ICE and DTLS-SRTP connected")

	if *holdTest {
		go holdSequence(c, pc, callID, inviteURI, toHdr, &rxVoiced)
	}

	// --- talk: a 1 kHz-ish tone as μ-law, 20 ms frames
	var linear2ulaw = func(s int16) byte {
		const bias, clip = 0x84, 32635
		sign := byte(0)
		v := int(s)
		if v < 0 {
			v, sign = -v, 0x80
		}
		if v > clip {
			v = clip
		}
		v += bias
		exp := 7
		for m := 0x4000; exp > 0 && v&m == 0; m >>= 1 {
			exp--
		}
		return ^(sign | byte(exp<<4) | byte((v>>(exp+3))&0x0f))
	}
	phase := 0.0
	end := time.Now().Add(*hold)
	tick := time.NewTicker(20 * time.Millisecond)
	began := time.Now()
	pressed := false
	for time.Now().Before(end) {
		<-tick.C
		frame := make([]byte, 160)
		for i := range frame {
			frame[i] = linear2ulaw(int16(8000 * math.Sin(phase)))
			phase += 2 * math.Pi * 1000 / 8000
		}
		if raw == nil {
			_ = track.WriteSample(media.Sample{Data: frame, Duration: 20 * time.Millisecond})
			continue
		}
		raw.voice(frame)
		if !pressed && time.Since(began) > 1500*time.Millisecond {
			pressed = true
			for _, d := range []byte(*dtmfDigits) {
				raw.digit(d, func() { <-tick.C })
				for i := 0; i < 10; i++ { // a short gap between digits
					<-tick.C
					raw.voice(frame)
				}
			}
			fmt.Printf("OK   sent keypad digits %q as RFC 4733 telephone-event\n", *dtmfDigits)
		}
	}
	tick.Stop()
	time.Sleep(300 * time.Millisecond)
	fmt.Printf("     received %d RTP packets, %d with audio\n", rxPackets.Load(), rxVoiced.Load())
	if rxVoiced.Load() < 20 {
		fail("no echoed audio came back")
	}
	fmt.Println("OK   audio echoed back through the switch")

	// --- BYE
	h := c.common("BYE", callID)
	h = append(h, "To: "+toHdr)
	c.send("BYE "+inviteURI+" SIP/2.0", h, "")
	if b := c.wait(5*time.Second, func(m *msg) bool { return m.status() >= 200 && strings.Contains(m.get("CSeq"), "BYE") }); b == nil || b.status() != 200 {
		fail("BYE: %v", b)
	}
	fmt.Println("OK   hung up")
	_ = pc.Close()
	_ = ws.Close()
	fmt.Println("PASS")
}
