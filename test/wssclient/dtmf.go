package main

import (
	"flag"
	"strings"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

var dtmfDigits = flag.String("dtmf", "", "digits to press as RFC 4733 telephone-event, 1.5 s into the call (use with -dest 5000)")

// rawTrack is a local audio track that writes RTP packets as given, so one
// stream can carry both G.711 voice and telephone-event (DTMF) packets, as a
// browser's RTCDTMFSender does. pion's static tracks overwrite the payload type.
type rawTrack struct {
	mu   sync.Mutex
	w    webrtc.TrackLocalWriter
	ssrc uint32
	pt   uint8 // negotiated voice payload type
	te   uint8 // telephone-event payload type
	seq  uint16
	ts   uint32
}

func (t *rawTrack) Bind(c webrtc.TrackLocalContext) (webrtc.RTPCodecParameters, error) {
	for _, cp := range c.CodecParameters() {
		if strings.EqualFold(cp.MimeType, webrtc.MimeTypePCMU) {
			t.w, t.ssrc, t.pt = c.WriteStream(), uint32(c.SSRC()), uint8(cp.PayloadType)
			t.te = 126 // as offered by this client
			return cp, nil
		}
	}
	return webrtc.RTPCodecParameters{}, webrtc.ErrUnsupportedCodec
}
func (t *rawTrack) Unbind(webrtc.TrackLocalContext) error { return nil }
func (t *rawTrack) ID() string                            { return "audio" }
func (t *rawTrack) RID() string                           { return "" }
func (t *rawTrack) StreamID() string                      { return "wssclient" }
func (t *rawTrack) Kind() webrtc.RTPCodecType             { return webrtc.RTPCodecTypeAudio }

func (t *rawTrack) write(pt uint8, marker bool, ts uint32, payload []byte) {
	if t.w == nil {
		return
	}
	t.seq++
	_, _ = t.w.WriteRTP(&rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: t.seq, Timestamp: ts, SSRC: t.ssrc, Marker: marker}, payload)
}

// voice sends one 20 ms G.711 frame.
func (t *rawTrack) voice(frame []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.write(t.pt, false, t.ts, frame)
	t.ts += 160
}

// digit sends a key press: 5 event packets (100 ms) with growing duration, then
// 3 end packets, all with the timestamp of the first one (RFC 4733), paced by
// the caller through next (called between packets).
func (t *rawTrack) digit(d byte, next func()) {
	code := byte(0)
	switch {
	case d >= '0' && d <= '9':
		code = d - '0'
	case d == '*':
		code = 10
	case d == '#':
		code = 11
	}
	t.mu.Lock()
	start := t.ts
	t.mu.Unlock()
	for i := 1; i <= 5; i++ {
		dur := uint16(160 * i)
		t.mu.Lock()
		t.write(t.te, i == 1, start, []byte{code, 10, byte(dur >> 8), byte(dur)})
		t.ts += 160
		t.mu.Unlock()
		next()
	}
	for i := 0; i < 3; i++ {
		dur := uint16(160 * 6)
		t.mu.Lock()
		t.write(t.te, false, start, []byte{code, 0x80 | 10, byte(dur >> 8), byte(dur)})
		t.mu.Unlock()
	}
	t.mu.Lock()
	t.ts += 160
	t.mu.Unlock()
}
