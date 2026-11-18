package core

import (
	"math"
	"math/rand"
	"testing"
)

func avmdTone(freqs []float64, amp float64, ms int, startSample int) []int16 {
	n := ms * 8
	out := make([]int16, n)
	if len(freqs) == 0 {
		return out // silence
	}
	for i := range out {
		var v float64
		for _, f := range freqs {
			v += math.Sin(2 * math.Pi * f * float64(startSample+i) / 8000)
		}
		out[i] = int16(amp * v / float64(len(freqs)))
	}
	return out
}

func avmdRun(d *avmdDetector, pcm []int16) *avmdHit {
	var first *avmdHit
	for i := 0; i+160 <= len(pcm); i += 160 {
		if h := d.Feed(pcm[i : i+160]); h != nil && first == nil {
			first = h
		}
	}
	return first
}

func TestAVMDDetectsSteadyBeep(t *testing.T) {
	for _, hz := range []float64{440, 800, 1000, 1400, 2000, 2950} {
		d := newAVMDDetector(avmdDefaultMs)
		pre := avmdTone(nil, 0, 200, 0) // silence
		h := avmdRun(d, append(pre, avmdTone([]float64{hz}, 8000, 400, 0)...))
		if h == nil {
			t.Fatalf("%v Hz beep not detected", hz)
		}
		if math.Abs(h.freq-hz) > 40 {
			t.Errorf("%v Hz beep reported as %.0f Hz", hz, h.freq)
		}
		if h.variance > 1500 {
			t.Errorf("%v Hz: variance %.0f Hz^2 for a steady tone", hz, h.variance)
		}
	}
}

func TestAVMDIgnoresWhatIsNotABeep(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	noise := make([]int16, 8000)
	for i := range noise {
		noise[i] = int16(rng.Intn(16000) - 8000)
	}
	// a voice-like sound: a harmonic series whose pitch glides, as speech does
	voice := make([]int16, 8000)
	phase := 0.0
	for i := range voice {
		f0 := 110 + 40*math.Sin(2*math.Pi*3*float64(i)/8000)
		phase += 2 * math.Pi * f0 / 8000
		var v float64
		for h := 1; h <= 12; h++ {
			v += math.Sin(float64(h)*phase) / float64(h)
		}
		voice[i] = int16(3500 * v)
	}
	cases := map[string][]int16{
		"noise":                 noise,
		"voice-like":            voice,
		"dtmf 1 (697+1209 Hz)":  avmdTone([]float64{697, 1209}, 9000, 1000, 0),
		"ringback 440+480 Hz":   avmdTone([]float64{440, 480}, 9000, 1000, 0),
		"too quiet (amp 150)":   avmdTone([]float64{1000}, 150, 1000, 0),
		"too short (80 ms)":     avmdTone([]float64{1000}, 8000, 80, 0),
		"silence":               make([]int16, 8000),
		"glide 800 to 1800 Hz":  glide(800, 1800, 1000),
	}
	for name, pcm := range cases {
		if h := avmdRun(newAVMDDetector(avmdDefaultMs), pcm); h != nil {
			t.Errorf("%s: detected as a %.0f Hz beep", name, h.freq)
		}
	}
}

func glide(from, to float64, ms int) []int16 {
	n := ms * 8
	out := make([]int16, n)
	phase := 0.0
	for i := range out {
		f := from + (to-from)*float64(i)/float64(n)
		phase += 2 * math.Pi * f / 8000
		out[i] = int16(8000 * math.Sin(phase))
	}
	return out
}

func TestAVMDHandlesAnyFrameSize(t *testing.T) {
	d := newAVMDDetector(avmdDefaultMs)
	pcm := avmdTone([]float64{1000}, 8000, 600, 0)
	var hit *avmdHit
	for i := 0; i < len(pcm); i += 53 { // odd chunk sizes
		end := min(i+53, len(pcm))
		if h := d.Feed(pcm[i:end]); h != nil && hit == nil {
			hit = h
		}
	}
	if hit == nil {
		t.Fatal("no beep with odd frame sizes")
	}
}

func TestAVMDFiresAgainAfterTheToneEnds(t *testing.T) {
	d := newAVMDDetector(avmdDefaultMs)
	beeps := 0
	for round := 0; round < 3; round++ {
		pcm := append(avmdTone([]float64{1000}, 8000, 300, 0), avmdTone(nil, 0, 300, 0)...)
		for i := 0; i+160 <= len(pcm); i += 160 {
			if d.Feed(pcm[i:i+160]) != nil {
				beeps++
			}
		}
	}
	if beeps != 3 {
		t.Fatalf("three separate beeps gave %d detections", beeps)
	}
}
