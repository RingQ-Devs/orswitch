package media

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/srtp/v3"
)

func TestSRTPKeys(t *testing.T) {
	k, err := NewSRTPKey(SRTPSuite80)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Master) != 16 || len(k.Salt) != 14 {
		t.Fatalf("key %d bytes, salt %d", len(k.Master), len(k.Salt))
	}
	back, err := ParseSRTPKey(SRTPSuite80, k.Inline())
	if err != nil || !k.same(back) {
		t.Fatalf("inline round trip: %v", err)
	}
	if _, err := ParseSRTPKey(SRTPSuite80, "AAAA"); err == nil {
		t.Fatal("a short key must be refused")
	}
	if _, err := ParseSRTPKey("AES_256_CM_HMAC_SHA1_80", k.Inline()); err == nil {
		t.Fatal("an unsupported suite must be refused")
	}
	k2, _ := NewSRTPKey(SRTPSuite80)
	if k.same(k2) {
		t.Fatal("two random keys are equal")
	}
}

// One secured endpoint against a phone that speaks SRTP with the two keys: what the phone encrypts is decrypted
// and echoed back encrypted with the endpoint's own key; a packet that is not valid SRTP is dropped; keying with the
// same key again keeps the stream going.
func TestEndpointSRTPEcho(t *testing.T) {
	alloc, _ := NewAllocator("127.0.0.1", 30800, 30900, nil)
	e, err := alloc.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	phone := udpPeer(t)
	e.SetRemote(apOf(phone), netip.AddrPort{})
	e.SetCodecs([]Codec{cPCMU}, cPCMU, -1)
	e.SetMode(ModeEcho)

	phoneKey, _ := NewSRTPKey(SRTPSuite80)
	ourKey, _ := NewSRTPKey(SRTPSuite80)
	if err := e.SetSRTP(ourKey, phoneKey); err != nil {
		t.Fatal(err)
	}
	if !e.Secure() {
		t.Fatal("endpoint should be secure")
	}
	enc, _ := srtp.CreateContext(phoneKey.Master, phoneKey.Salt, srtp.ProtectionProfileAes128CmHmacSha1_80)
	dec, _ := srtp.CreateContext(ourKey.Master, ourKey.Salt, srtp.ProtectionProfileAes128CmHmacSha1_80)
	to := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(e.Port))

	send := func(seq uint16, payload []byte) {
		pkt, err := enc.EncryptRTP(nil, rtpPacket(0, seq, payload), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := phone.WriteToUDPAddrPort(pkt, to); err != nil {
			t.Fatal(err)
		}
	}
	want := bytes.Repeat([]byte{0x42}, 160)
	send(1, want)
	got, err := dec.DecryptRTP(nil, recv(t, phone), nil)
	if err != nil {
		t.Fatalf("the echo is not SRTP with the endpoint's key: %v", err)
	}
	if !bytes.Equal(got[12:], want) {
		t.Fatal("echoed payload differs")
	}

	// plain RTP into a secured endpoint is dropped (nothing comes back)
	_, _ = phone.WriteToUDPAddrPort(rtpPacket(0, 2, want), to)
	_ = phone.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := phone.Read(make([]byte, 2000)); err == nil {
		t.Fatalf("plain RTP was answered (%d bytes)", n)
	}
	if e.srtpDrops.Load() == 0 {
		t.Fatal("the dropped packet should be counted")
	}

	// the same keys again (a re-INVITE repeating the SDP): the stream's state is kept
	if err := e.SetSRTP(ourKey, phoneKey); err != nil {
		t.Fatal(err)
	}
	send(3, want)
	if _, err := dec.DecryptRTP(nil, recv(t, phone), nil); err != nil {
		t.Fatalf("after re-keying with the same key: %v", err)
	}
}
