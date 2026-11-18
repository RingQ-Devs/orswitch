package core

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// avmd: answering machine (voicemail) beep detection, the mod_avmd equivalent.
//
//	dialplan: <action application="avmd" data="start"/> (also avmd_start / avmd_stop)
//	API:      avmd <uuid> start|stop [inbound|outbound]
//
// It listens to what the leg's far end sends (the audio the switch receives on that leg) and fires
// CUSTOM avmd::beep when a clean, steady tone lasts long enough: Beep-Status DETECTED, Frequency and
// Frequency-variance (Hz and Hz^2) and Detection-time (ms since start), plus the channel's headers. It also
// sets avmd_detect=TRUE. avmd::start and avmd::stop mark the detector's life. RingQ's broadcast and
// "app.lua avmd" hang up on the beep.
//
// Channel variables: avmd_min_tone_ms (default 120, 40-2000) is how long the tone must hold;
// avmd_repeat=true keeps listening after the first beep (default: stop after it).
//
// A tone counts when one frequency between 300 and 3400 Hz holds at least 60 % of the frame's energy, the
// frame is louder than about -42 dBFS, and the frequency stays within 60 Hz for the whole time. Voice,
// DTMF (two tones) and ringback (two tones) do not satisfy that; a fax calling tone does, as in mod_avmd.
const (
	avmdFrame    = 160 // 20 ms at 8 kHz
	avmdMinRMS   = 250.0
	avmdPurity   = 0.60
	avmdSpread   = 60.0 // Hz: the most the frequency may move during one beep
	avmdLowHz    = 300
	avmdHighHz   = 3400
	avmdStepHz   = 20
	avmdDefaultMs = 120
)

type avmdHit struct {
	freq, variance float64
}

type avmdDetector struct {
	minFrames int
	coeff     []float64
	freq      []float64
	buf       []int16
	run       []float64
	mean      float64
	fired     bool // this beep was reported; re-armed when the tone stops
}

func newAVMDDetector(minToneMs int) *avmdDetector {
	d := &avmdDetector{minFrames: max(2, (minToneMs+10)/20)}
	for f := avmdLowHz; f <= avmdHighHz; f += avmdStepHz {
		d.freq = append(d.freq, float64(f))
		d.coeff = append(d.coeff, 2*math.Cos(2*math.Pi*float64(f)/8000))
	}
	return d
}

// Feed takes any amount of 8 kHz samples and returns a hit when the tone has held long enough.
func (d *avmdDetector) Feed(pcm []int16) *avmdHit {
	d.buf = append(d.buf, pcm...)
	var hit *avmdHit
	for len(d.buf) >= avmdFrame {
		if h := d.frame(d.buf[:avmdFrame]); h != nil && hit == nil {
			hit = h
		}
		d.buf = d.buf[avmdFrame:]
	}
	return hit
}

func (d *avmdDetector) frame(x []int16) *avmdHit {
	var e float64
	for _, v := range x {
		e += float64(v) * float64(v)
	}
	if rms := math.Sqrt(e / float64(len(x))); rms < avmdMinRMS {
		d.run, d.fired = d.run[:0], false
		return nil
	}
	best, bestP := 0, 0.0
	for i, c := range d.coeff {
		var s1, s2 float64
		for _, v := range x {
			s0 := float64(v) + c*s1 - s2
			s2, s1 = s1, s0
		}
		if p := s1*s1 + s2*s2 - c*s1*s2; p > bestP {
			best, bestP = i, p
		}
	}
	// A pure tone of amplitude A: |X|^2 = (A*N/2)^2 and energy = A^2*N/2, so purity is 1.
	if purity := bestP / (e * float64(len(x)) / 2); purity < avmdPurity {
		d.run, d.fired = d.run[:0], false
		return nil
	}
	f := d.freq[best]
	// The whole run must stay within avmdSpread Hz: a glide or a changing pitch drifts out of it even when
	// each step is small.
	lo, hi := f, f
	for _, v := range d.run {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if len(d.run) > 0 && hi-lo <= avmdSpread {
		d.run = append(d.run, f)
	} else {
		d.run = append(d.run[:0], f)
	}
	var sum float64
	for _, v := range d.run {
		sum += v
	}
	d.mean = sum / float64(len(d.run))
	if len(d.run) < d.minFrames {
		return nil
	}
	if d.fired { // the same beep, still going: one detection per beep
		d.run = d.run[:0]
		return nil
	}
	d.fired = true
	var variance float64
	for _, v := range d.run {
		variance += (v - d.mean) * (v - d.mean)
	}
	variance /= float64(len(d.run))
	d.run = d.run[:0]
	return &avmdHit{freq: d.mean, variance: variance}
}

// avmdSessions holds the running detectors by channel uuid.
var avmdSessions sync.Map

// startAVMD starts the detector on ch. It reports false when one already runs.
func startAVMD(c *Core, ch *Channel) bool {
	if _, busy := avmdSessions.LoadOrStore(ch.UUID, struct{}{}); busy {
		return false
	}
	frames, stop, err := c.Hear(ch.UUID)
	if err != nil {
		avmdSessions.Delete(ch.UUID)
		return false
	}
	ms := avmdDefaultMs
	if n, err := strconv.Atoi(strings.TrimSpace(ch.Var("avmd_min_tone_ms"))); err == nil && n >= 40 && n <= 2000 {
		ms = n
	}
	repeat := truthy(ch.Var("avmd_repeat"))
	ctx, cancel := context.WithCancel(ch.ctx)
	avmdSessions.Store(ch.UUID, cancel)
	d := newAVMDDetector(ms)
	started := time.Now()
	fire := func(sub, status string, kv ...string) {
		e := NewEvent(EvCustom, sub)
		ch.eventHeaders(e, true)
		e.Add("Beep-Status", status)
		for i := 0; i+1 < len(kv); i += 2 {
			e.Add(kv[i], kv[i+1])
		}
		c.events.Publish(e)
	}
	fire("avmd::start", "START")
	ch.log.Info("avmd: start", "min_tone_ms", ms)
	go func() {
		defer func() {
			stop()
			cancel()
			avmdSessions.Delete(ch.UUID)
			fire("avmd::stop", "STOP")
		}()
		for {
			select {
			case f, ok := <-frames:
				if !ok {
					return
				}
				if h := d.Feed(f); h != nil {
					ch.SetVar("avmd_detect", "TRUE")
					ch.log.Info("avmd: beep", "hz", int(h.freq))
					fire("avmd::beep", "DETECTED",
						"Frequency", fmt.Sprintf("%.2f", h.freq),
						"Frequency-variance", fmt.Sprintf("%.2f", h.variance),
						"Detection-time", strconv.FormatInt(time.Since(started).Milliseconds(), 10))
					if !repeat {
						return
					}
				}
			case <-ctx.Done():
				return
			case <-c.stop:
				return
			}
		}
	}()
	return true
}

// stopAVMD ends the detector on uuid. It reports false when none runs.
func stopAVMD(uuid string) bool {
	v, ok := avmdSessions.Load(uuid)
	if !ok {
		return false
	}
	if cancel, ok := v.(context.CancelFunc); ok {
		cancel()
	}
	return true
}

func appAVMD(_ context.Context, c *Core, ch *Channel, data string) error {
	switch strings.ToLower(strings.TrimSpace(strings.Fields(data + " start")[0])) {
	case "stop":
		stopAVMD(ch.UUID)
	default:
		if err := ch.AnswerLocal(true); err != nil {
			return err
		}
		startAVMD(c, ch)
	}
	return nil
}

func appAVMDStart(ctx context.Context, c *Core, ch *Channel, data string) error {
	return appAVMD(ctx, c, ch, "start "+data)
}

func appAVMDStop(ctx context.Context, c *Core, ch *Channel, data string) error {
	return appAVMD(ctx, c, ch, "stop")
}

func apiAVMD(c *Core, args []string) (string, error) {
	if len(args) < 2 {
		return "-USAGE: avmd <uuid> start|stop [inbound|outbound]\n", nil
	}
	ch := c.Channel(args[0])
	if ch == nil {
		return "-ERR No such channel!\n", nil
	}
	switch strings.ToLower(args[1]) {
	case "start":
		if !startAVMD(c, ch) {
			return "-ERR avmd is already running on this channel\n", nil
		}
		return "+OK Started\n", nil
	case "stop":
		if !stopAVMD(ch.UUID) {
			return "-ERR avmd is not running on this channel\n", nil
		}
		return "+OK Stopped\n", nil
	}
	return "-USAGE: avmd <uuid> start|stop [inbound|outbound]\n", nil
}
