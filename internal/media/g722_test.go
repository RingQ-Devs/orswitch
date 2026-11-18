package media

import (
	"encoding/binary"
	"math"
	"os"
	"testing"
)

func readPCM(t *testing.T, name string) []int16 {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

// testdata/g722_*.{pcm,bin} come from libspandsp's g722_encode / g722_decode (64 kbit/s, 16 kHz) on a
// 400 ms signal of two tones, a 300-7500 Hz chirp and a noise burst: the codec must match them byte for byte.
func TestG722MatchesTheReferenceEncoder(t *testing.T) {
	in := readPCM(t, "g722_in.pcm")
	want, err := os.ReadFile("testdata/g722_ref.bin")
	if err != nil {
		t.Fatal(err)
	}
	got := NewG722Encoder().Encode(nil, in)
	if len(got) != len(want) {
		t.Fatalf("%d bytes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("first difference at byte %d (sample %d): got %#02x want %#02x", i, i*2, got[i], want[i])
		}
	}
}

func TestG722MatchesTheReferenceDecoder(t *testing.T) {
	data, _ := os.ReadFile("testdata/g722_ref.bin")
	want := readPCM(t, "g722_dec.pcm")
	got := NewG722Decoder().Decode(nil, data)
	if len(got) != len(want) {
		t.Fatalf("%d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("first difference at sample %d: got %d want %d", i, got[i], want[i])
		}
	}
}

// Encoding in odd-sized pieces must give the same bytes as one call: the state carries over.
func TestG722StateCarriesAcrossCalls(t *testing.T) {
	in := readPCM(t, "g722_in.pcm")
	want := NewG722Encoder().Encode(nil, in)
	e := NewG722Encoder()
	var got []byte
	for i := 0; i < len(in); i += 320 {
		got = e.Encode(got, in[i:min(i+320, len(in))])
	}
	if string(got) != string(want) {
		t.Fatal("chunked encoding differs from one-shot encoding")
	}
}

func tone8k(hz, amp float64, n, start int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*hz*float64(start+i)/8000))
	}
	return out
}

func rmsOf(s []int16) float64 {
	var e float64
	for _, v := range s {
		e += float64(v) * float64(v)
	}
	return math.Sqrt(e / float64(len(s)))
}

// An 8 kHz tone through the encoder and decoder (what a G.711 caller to a G.722 phone and back hears) keeps
// its frequency and level: the correlation with the original at the right delay is high.
func TestG722EightKHzRoundTrip(t *testing.T) {
	enc, dec := NewG722Encoder(), NewG722Decoder()
	var out []int16
	for f := 0; f < 50; f++ {
		pkt := enc.Encode8k(nil, tone8k(1000, 8000, 160, f*160))
		if len(pkt) != 160 {
			t.Fatalf("packet of %d bytes, want 160", len(pkt))
		}
		out = dec.Decode8k(out, pkt)
		if len(out) != (f+1)*160 {
			t.Fatalf("after %d packets %d samples", f+1, len(out))
		}
	}
	body := out[2000:] // past the filters' start-up
	ref := tone8k(1000, 8000, len(out), 0)
	best := 0.0
	for delay := 0; delay < 60; delay++ { // the filter chain's delay is a few samples
		var num, den float64
		for i := 2000; i < len(out)-60; i++ {
			num += float64(body[i-2000]) * float64(ref[i-delay])
		}
		den = rmsOf(body) * rmsOf(ref[:len(body)]) * float64(len(body))
		if c := num / den; c > best {
			best = c
		}
	}
	if best < 0.97 {
		t.Fatalf("round trip correlation %.3f, want at least 0.97", best)
	}
	if r := rmsOf(body) / rmsOf(ref[:len(body)]); r < 0.8 || r > 1.2 {
		t.Fatalf("level changed by a factor %.2f", r)
	}
}

func TestG722SilenceStaysSilent(t *testing.T) {
	enc, dec := NewG722Encoder(), NewG722Decoder()
	var out []int16
	for f := 0; f < 10; f++ {
		out = dec.Decode8k(out, enc.Encode8k(nil, make([]int16, 160)))
	}
	if r := rmsOf(out); r > 4 {
		t.Fatalf("silence came back with rms %.1f", r)
	}
}
