package media

import (
	"bytes"
	"context"
	"math"
	"net"
	"net/netip"
	"testing"
	"time"
)

func g722Pair(t *testing.T, codecA, codecB Codec) (a, b *Endpoint, phoneA, phoneB *net.UDPConn, ra, rb func() []byte, epA, epB netip.AddrPort) {
	t.Helper()
	alloc, err := NewAllocator("127.0.0.1", 30400, 30500, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ = alloc.Allocate()
	b, _ = alloc.Allocate()
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	pa, pb := udpPeer(t), udpPeer(t)
	a.SetRemote(apOf(pa), netip.AddrPort{})
	b.SetRemote(apOf(pb), netip.AddrPort{})
	a.SetCodecs([]Codec{codecA}, codecA, -1)
	b.SetCodecs([]Codec{codecB}, codecB, -1)
	Bridge(a, b)
	lo := netip.MustParseAddr("127.0.0.1")
	return a, b, pa, pb, func() []byte { return recv(t, pa) }, func() []byte { return recv(t, pb) },
		netip.AddrPortFrom(lo, uint16(a.Port)), netip.AddrPortFrom(lo, uint16(b.Port))
}

// correlate returns the best normalised correlation of got against a 1 kHz sine over small delays.
func correlate(got []int16, hz float64) float64 {
	best := 0.0
	for delay := 0; delay < 80; delay++ {
		var num, eg, er float64
		for i := 400; i < len(got); i++ {
			r := math.Sin(2 * math.Pi * hz * float64(i-delay) / 8000)
			num += float64(got[i]) * r
			eg += float64(got[i]) * float64(got[i])
			er += r * r
		}
		if c := num / math.Sqrt(eg*er); c > best {
			best = c
		}
	}
	return best
}

var (
	cPCMU = Codec{PT: 0, Name: "PCMU", Rate: 8000}
	cG722 = Codec{PT: 9, Name: "G722", Rate: 8000}
)

// A G.711 caller and a G.722 callee: each direction is converted with filter state kept across packets.
func TestRelayBetweenG711AndG722(t *testing.T) {
	_, _, phoneA, phoneB, ra, rb, epA, epB := g722Pair(t, cPCMU, cG722)
	_ = ra

	// A (PCMU) -> B (G.722)
	dec := NewG722Decoder()
	var heard []int16
	for i := 0; i < 40; i++ {
		pcm := tone8k(1000, 8000, 160, i*160)
		pay := make([]byte, 160)
		for k, v := range pcm {
			pay[k] = LinearToUlaw(v)
		}
		if _, err := phoneA.WriteToUDPAddrPort(rtpPacketTS(0, uint16(i), uint32(i)*160, pay), epA); err != nil {
			t.Fatal(err)
		}
		got := rb()
		h, ok := parseRTP(got)
		if !ok || h.pt != 9 {
			t.Fatalf("packet %d: want payload type 9 (G722), got %+v", i, h)
		}
		if n := h.payloadEnd - h.payloadOff; n != 160 {
			t.Fatalf("G.722 payload of %d bytes, want 160", n)
		}
		if h.ts != uint32(i)*160 {
			t.Fatalf("timestamp %d, want %d (G.722 clocks at 8 kHz)", h.ts, i*160)
		}
		heard = dec.Decode8k(heard, got[h.payloadOff:h.payloadEnd])
	}
	if c := correlate(heard, 1000); c < 0.95 {
		t.Fatalf("PCMU to G.722: tone correlation %.3f", c)
	}

	// B (G.722) -> A (PCMU)
	enc := NewG722Encoder()
	var back []int16
	for i := 0; i < 40; i++ {
		pay := enc.Encode8k(nil, tone8k(1000, 8000, 160, i*160))
		if _, err := phoneB.WriteToUDPAddrPort(rtpPacketTS(9, uint16(i), uint32(i)*160, pay), epB); err != nil {
			t.Fatal(err)
		}
		got := ra()
		h, ok := parseRTP(got)
		if !ok || h.pt != 0 {
			t.Fatalf("packet %d: want payload type 0 (PCMU), got %+v", i, h)
		}
		out, _ := Decode("PCMU", got[h.payloadOff:h.payloadEnd], nil)
		back = append(back, out...)
	}
	if c := correlate(back, 1000); c < 0.95 {
		t.Fatalf("G.722 to PCMU: tone correlation %.3f", c)
	}
}

// Two G.722 phones: the payload is passed through untouched.
func TestRelayG722ToG722IsAPassThrough(t *testing.T) {
	_, _, phoneA, _, _, rb, epA, _ := g722Pair(t, cG722, cG722)
	pay := NewG722Encoder().Encode8k(nil, tone8k(700, 6000, 160, 0))
	if _, err := phoneA.WriteToUDPAddrPort(rtpPacket(9, 1, pay), epA); err != nil {
		t.Fatal(err)
	}
	got := rb()
	h, _ := parseRTP(got)
	if h.pt != 9 || !bytes.Equal(got[h.payloadOff:h.payloadEnd], pay) {
		t.Fatal("G.722 to G.722 should not be re-encoded")
	}
}

// Local mode (IVR, voicemail, tones): frames written are sent as G.722, G.722 received is read as 8 kHz frames.
func TestLocalModeWithG722(t *testing.T) {
	alloc, _ := NewAllocator("127.0.0.1", 30600, 30700, nil)
	e, err := alloc.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	phone := udpPeer(t)
	e.SetRemote(apOf(phone), netip.AddrPort{})
	e.SetCodecs([]Codec{cG722}, cG722, -1)
	e.SetMode(ModeLocal)
	if !CanTerminate("G722") {
		t.Fatal("G722 should be terminable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		for i := 0; i < 40; i++ {
			if e.WriteFrame(ctx, tone8k(1000, 8000, 160, i*160)) != nil {
				return
			}
		}
	}()
	dec := NewG722Decoder()
	var heard []int16
	// read packets until enough audio arrived
	phone.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2000)
	for len(heard) < 30*160 {
		n, err := phone.Read(buf)
		if err != nil {
			t.Fatalf("after %d samples: %v", len(heard), err)
		}
		h, ok := parseRTP(buf[:n])
		if !ok || h.pt != 9 {
			t.Fatalf("local audio should go out as G722 (PT 9), got %+v", h)
		}
		heard = dec.Decode8k(heard, buf[h.payloadOff:h.payloadEnd])
	}
	if c := correlate(heard[:30*160], 1000); c < 0.9 {
		t.Fatalf("local G.722 audio: correlation %.3f", c)
	}

	// and the other way: the phone talks G.722, ReadFrame gives 8 kHz audio
	enc := NewG722Encoder()
	lo := netip.MustParseAddr("127.0.0.1")
	ep := netip.AddrPortFrom(lo, uint16(e.Port))
	var got []int16
	for i := 0; i < 30; i++ {
		_, _ = phone.WriteToUDPAddrPort(rtpPacketTS(9, uint16(i), uint32(i)*160, enc.Encode8k(nil, tone8k(1000, 8000, 160, i*160))), ep)
		rctx, rc := context.WithTimeout(ctx, time.Second)
		f, err := e.ReadFrame(rctx)
		rc()
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if len(f) != 160 {
			t.Fatalf("frame of %d samples, want 160", len(f))
		}
		got = append(got, f...)
	}
	if c := correlate(got, 1000); c < 0.9 {
		t.Fatalf("received G.722 read as frames: correlation %.3f", c)
	}
}
