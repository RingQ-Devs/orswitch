package core_test

import (
	"bytes"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pion/srtp/v3"

	"github.com/ringq-devs/orswitch/internal/media"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// secureLeg is the phone side of one SRTP leg: our key (what the phone encrypts with), and the switch's key once
// its SDP has been read (what the phone decrypts with).
type secureLeg struct {
	t        *testing.T
	mine     *media.SRTPKey
	theirs   *media.SRTPKey
	enc, dec *srtp.Context
}

func newSecureLeg(t *testing.T) *secureLeg {
	k, err := media.NewSRTPKey(media.SRTPSuite80)
	if err != nil {
		t.Fatal(err)
	}
	return &secureLeg{t: t, mine: k}
}

// sdp is a PCMU offer or answer as RTP/SAVP with the leg's key.
func (l *secureLeg) sdp(port int) []byte {
	b := string(testua.SDP(port, 0))
	b = strings.Replace(b, "RTP/AVP", "RTP/SAVP", 1)
	return []byte(b + "a=crypto:1 " + l.mine.Suite + " inline:" + l.mine.Inline() + "\r\n")
}

// learn reads the switch's key line from its SDP (200 OK or INVITE) and builds the two contexts.
func (l *secureLeg) learn(sdpBody []byte) {
	l.t.Helper()
	var crypto string
	for _, line := range strings.Split(string(sdpBody), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "a=crypto:") {
			crypto = strings.TrimSpace(line)
		}
	}
	if !strings.Contains(string(sdpBody), "RTP/SAVP") || crypto == "" {
		l.t.Fatalf("the SDP is not SRTP:\n%s", sdpBody)
	}
	f := strings.Fields(strings.TrimPrefix(crypto, "a=crypto:"))
	if len(f) < 3 || !strings.HasPrefix(f[2], "inline:") {
		l.t.Fatalf("bad key line %q", crypto)
	}
	key, err := media.ParseSRTPKey(f[1], strings.TrimPrefix(f[2], "inline:"))
	if err != nil {
		l.t.Fatal(err)
	}
	l.theirs = key
	if l.enc, err = srtp.CreateContext(l.mine.Master, l.mine.Salt, srtp.ProtectionProfileAes128CmHmacSha1_80); err != nil {
		l.t.Fatal(err)
	}
	if l.dec, err = srtp.CreateContext(key.Master, key.Salt, srtp.ProtectionProfileAes128CmHmacSha1_80); err != nil {
		l.t.Fatal(err)
	}
}

func rtpPkt(pt byte, seq uint16, payload []byte) []byte {
	b := make([]byte, 12+len(payload))
	b[0], b[1] = 0x80, pt
	b[2], b[3] = byte(seq>>8), byte(seq)
	ts := uint32(seq) * 160
	b[4], b[5], b[6], b[7] = byte(ts>>24), byte(ts>>16), byte(ts>>8), byte(ts)
	b[8], b[9], b[10], b[11] = 0xde, 0xad, 0xbe, 0xef
	copy(b[12:], payload)
	return b
}

func (l *secureLeg) send(conn *net.UDPConn, dst netip.AddrPort, pkt []byte) {
	l.t.Helper()
	enc, err := l.enc.EncryptRTP(nil, pkt, nil)
	if err != nil {
		l.t.Fatal(err)
	}
	if _, err := conn.WriteToUDPAddrPort(enc, dst); err != nil {
		l.t.Fatal(err)
	}
}

// read returns the next decrypted RTP packet (header included).
func (l *secureLeg) read(conn *net.UDPConn) []byte {
	l.t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2000)
	n, err := conn.Read(buf)
	if err != nil {
		l.t.Fatalf("no media: %v", err)
	}
	plain, err := l.dec.DecryptRTP(nil, buf[:n], nil)
	if err != nil {
		l.t.Fatalf("the packet is not valid SRTP with the switch's key: %v", err)
	}
	return plain
}

func payloadOf(b []byte) []byte { return b[12:] }

func srtpDialplan() string {
	return aiDialplan(`
  <extension name="srtpecho"><condition field="destination_number" expression="^9600$">
    <action application="answer"/>
    <action application="echo"/>
  </condition></extension>
  <extension name="srtptone"><condition field="destination_number" expression="^9601$">
    <action application="answer"/>
    <action application="playback" data="tone_stream://%(2000,0,440);loops=-1"/>
  </condition></extension>
  <extension name="to1001"><condition field="destination_number" expression="^8001$">
    <action application="set" data="rtp_secure_media=true"/>
    <action application="bridge" data="user/1001"/>
  </condition></extension>
  <extension name="plain1001"><condition field="destination_number" expression="^8002$">
    <action application="bridge" data="user/1001"/>
  </condition></extension>`)
}

// A phone that offers SRTP is answered with SRTP: RTP/SAVP, the switch's own key line, media encrypted both ways.
func TestSRTPOfferIsAnsweredWithSRTP(t *testing.T) {
	h := newHarnessWith(t, "", twoUsers(), srtpDialplan(), nil)
	alice := h.phone("1000")
	leg := newSecureLeg(t)
	alice.OfferSDP = func(port int, _ []int) []byte { return leg.sdp(port) }
	call, err := alice.Dial("9600", nil, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("an SRTP offer should be answered: %v %v", call, err)
	}
	defer func() { _, _ = call.Bye() }()
	leg.learn(call.Final.Body)
	if leg.theirs.Suite != leg.mine.Suite {
		t.Fatalf("suite %s answered for %s", leg.theirs.Suite, leg.mine.Suite)
	}
	if bytes.Equal(leg.theirs.Master, leg.mine.Master) {
		t.Fatal("the switch must use its own key, not echo ours")
	}

	// echo: what the phone sends encrypted comes back encrypted, and decrypts to the same payload
	want := bytes.Repeat([]byte{0x55}, 160)
	for i := 0; i < 5; i++ {
		leg.send(call.RTP, call.RemoteRTP, rtpPkt(0, uint16(i+1), want))
	}
	got := leg.read(call.RTP)
	if !bytes.Equal(payloadOf(got), want) {
		t.Fatalf("echoed payload differs: % x", payloadOf(got)[:8])
	}
}

// The switch's own audio on an SRTP leg is SRTP too.
func TestSRTPLocalAudioIsEncrypted(t *testing.T) {
	h := newHarnessWith(t, "", twoUsers(), srtpDialplan(), nil)
	alice := h.phone("1000")
	leg := newSecureLeg(t)
	alice.OfferSDP = func(port int, _ []int) []byte { return leg.sdp(port) }
	call, err := alice.Dial("9601", nil, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", err)
	}
	defer func() { _, _ = call.Bye() }()
	leg.learn(call.Final.Body)
	pkt := leg.read(call.RTP) // decrypts only if the switch used the key it offered
	if len(payloadOf(pkt)) != 160 {
		t.Fatalf("payload of %d bytes", len(payloadOf(pkt)))
	}
}

// srtp=off keeps the old behaviour: an SRTP offer is refused.
func TestSRTPOffIsRefused(t *testing.T) {
	h := newHarnessWith(t, `<param name="srtp" value="off"/>`, twoUsers(), srtpDialplan(), nil)
	alice := h.phone("1000")
	leg := newSecureLeg(t)
	alice.OfferSDP = func(port int, _ []int) []byte { return leg.sdp(port) }
	call, err := alice.Dial("9600", nil, 5*time.Second, false)
	if err == nil && call != nil && call.Final != nil && call.Final.StatusCode == 200 {
		_, _ = call.Bye()
		t.Fatal("srtp=off should refuse an SRTP offer")
	}
	if call != nil && call.Final != nil && call.Final.StatusCode != 488 {
		t.Fatalf("status %d, want 488", call.Final.StatusCode)
	}
}

// An SRTP leg bridged to a plain RTP leg: each side keeps its own protection, the switch converts.
func TestSRTPLegBridgedToPlainLeg(t *testing.T) {
	h := newHarnessWith(t, "", twoUsers(), srtpDialplan(), nil)
	alice, bob := h.phone("1000"), h.phone("1001")
	leg := newSecureLeg(t)
	alice.OfferSDP = func(port int, _ []int) []byte { return leg.sdp(port) }
	done := make(chan *testua.Call, 1)
	go func() {
		c, _ := alice.Dial("8002", nil, 15*time.Second, false)
		done <- c
	}()
	in, err := bob.WaitInvite(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(in.Invite.Body), "SAVP") || strings.Contains(string(in.Invite.Body), "a=crypto") {
		t.Fatalf("the plain leg must be offered plain RTP:\n%s", in.Invite.Body)
	}
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	call := <-done
	if call == nil || call.Final == nil || call.Final.StatusCode != 200 {
		t.Fatalf("not answered: %+v", call)
	}
	defer func() { _, _ = call.Bye() }()
	leg.learn(call.Final.Body)
	h.waitFor("bridged", func() bool { return strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA") == 2 })

	// alice (SRTP) -> bob (plain)
	want := bytes.Repeat([]byte{0x33}, 160)
	for i := 0; i < 5; i++ {
		leg.send(call.RTP, call.RemoteRTP, rtpPkt(0, uint16(i+1), want))
		time.Sleep(20 * time.Millisecond)
	}
	_ = in.RTP.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2000)
	n, err := in.RTP.Read(buf)
	if err != nil {
		t.Fatalf("bob got no media: %v", err)
	}
	if n != 12+160 || !bytes.Equal(buf[12:n], want) {
		t.Fatalf("bob should get the plain payload (len %d)", n)
	}
	// bob (plain) -> alice (SRTP)
	back := bytes.Repeat([]byte{0x77}, 160)
	for i := 0; i < 5; i++ {
		_ = testua.SendRTP(in.RTP, in.Remote, 0, uint16(i+1), uint32(i+1)*160, back)
		time.Sleep(20 * time.Millisecond)
	}
	got := leg.read(call.RTP)
	if !bytes.Equal(payloadOf(got), back) {
		t.Fatal("alice should get bob's audio as SRTP")
	}
}

// rtp_secure_media=true: the leg we start is offered SRTP, and its SRTP answer keys the media.
func TestSRTPOfferedOnTheOutboundLeg(t *testing.T) {
	h := newHarnessWith(t, "", twoUsers(), srtpDialplan(), nil)
	alice, bob := h.phone("1000"), h.phone("1001")
	leg := newSecureLeg(t)
	bob.AnswerSDP = func(port int, offer []byte) []byte {
		leg.learn(offer) // the INVITE must carry the switch's key line
		return leg.sdp(port)
	}
	done := make(chan *testua.Call, 1)
	go func() {
		c, _ := alice.Dial("8001", nil, 15*time.Second, false)
		done <- c
	}()
	in, err := bob.WaitInvite(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(in.Invite.Body), "RTP/SAVP") {
		t.Fatalf("rtp_secure_media=true: the INVITE should offer RTP/SAVP:\n%s", in.Invite.Body)
	}
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	call := <-done
	if call == nil || call.Final == nil || call.Final.StatusCode != 200 {
		t.Fatalf("not answered: %+v", call)
	}
	defer func() { _, _ = call.Bye() }()
	h.waitFor("bridged", func() bool { return strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA") == 2 })

	// alice (plain) -> bob (SRTP)
	want := bytes.Repeat([]byte{0x21}, 160)
	for i := 0; i < 5; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i+1), uint32(i+1)*160, want)
		time.Sleep(20 * time.Millisecond)
	}
	got := leg.read(in.RTP)
	if !bytes.Equal(payloadOf(got), want) {
		t.Fatal("bob should get alice's audio as SRTP")
	}
}

// A callee that answers a mandatory SRTP offer with plain RTP does not get the call.
func TestSRTPMandatoryRefusesAPlainAnswer(t *testing.T) {
	h := newHarnessWith(t, "", twoUsers(), srtpDialplan(), nil)
	alice, bob := h.phone("1000"), h.phone("1001")
	done := make(chan *testua.Call, 1)
	go func() {
		c, _ := alice.Dial("8001", nil, 15*time.Second, false)
		done <- c
	}()
	in, err := bob.WaitInvite(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = in.Answer(0) // plain answer to an SRTP offer
	call := <-done
	if call != nil && call.Final != nil && call.Final.StatusCode == 200 {
		_, _ = call.Bye()
		t.Fatal("a plain answer to a mandatory SRTP offer must not connect the call")
	}
}
