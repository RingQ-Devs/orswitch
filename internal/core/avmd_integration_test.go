package core_test

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/media"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// sineUlaw is n samples of a tone as G.711 mu-law, starting at sample start.
func sineUlaw(hz, amp float64, start, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = media.LinearToUlaw(int16(amp * math.Sin(2*math.Pi*hz*float64(start+i)/8000)))
	}
	return b
}

// avmd on a live call: a steady 1000 Hz tone from the far end fires CUSTOM avmd::beep once
// (Unique-ID, Beep-Status, Frequency), sets avmd_detect, and the detector ends; voice-free silence does not fire.
func TestAVMDBeepOnACall(t *testing.T) {
	dp := aiDialplan(`
  <extension name="vm"><condition field="destination_number" expression="^9400$">
    <action application="answer"/>
    <action application="avmd" data="start"/>
    <action application="playback" data="silence_stream://-1"/>
  </condition></extension>`)
	h := newHarnessWith(t, "", twoUsers(), dp, nil)
	sub := h.sw.Events().Subscribe(64, func(e *core.Event) bool {
		return strings.HasPrefix(e.Subclass(), "avmd::")
	})
	defer sub.Close()
	if got := strings.TrimSpace(h.sw.API("module_exists mod_avmd")); got != "true" {
		t.Fatalf("module_exists mod_avmd = %q", got)
	}

	alice := h.phone("1000")
	call, err := alice.Dial("9400", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", err)
	}
	var uuid string
	h.waitFor("the call's channel", func() bool {
		for _, ch := range h.sw.Channels() {
			if ch.Var("destination_number") == "9400" {
				uuid = ch.UUID
			}
		}
		return uuid != ""
	})
	if ev := waitSub(t, sub, "avmd::start", 3*time.Second); ev.Get("Unique-ID") != uuid {
		t.Fatalf("avmd::start for %q, call is %q", ev.Get("Unique-ID"), uuid)
	}
	if out := h.sw.API("avmd " + uuid + " start"); !strings.HasPrefix(out, "-ERR") {
		t.Fatalf("a second start should be refused, got %q", out)
	}

	// 0.4 s of silence, then 0.6 s of 1000 Hz
	seq, ts := uint16(0), uint32(0)
	send := func(p []byte) {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, seq, ts, p)
		seq++
		ts += 160
		time.Sleep(20 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		send(ulaw(0, 160))
	}
	select {
	case e := <-sub.C:
		t.Fatalf("silence fired %s", e.Subclass())
	default:
	}
	for i := 0; i < 30; i++ {
		send(sineUlaw(1000, 8000, i*160, 160))
	}
	ev := waitSub(t, sub, "avmd::beep", 3*time.Second)
	if ev.Get("Unique-ID") != uuid || ev.Get("Beep-Status") != "DETECTED" {
		t.Fatalf("beep event: uuid %q status %q", ev.Get("Unique-ID"), ev.Get("Beep-Status"))
	}
	if f, _ := strconv.ParseFloat(ev.Get("Frequency"), 64); math.Abs(f-1000) > 60 {
		t.Errorf("Frequency %q, want about 1000", ev.Get("Frequency"))
	}
	for _, k := range []string{"Frequency-variance", "Detection-time"} {
		if ev.Get(k) == "" {
			t.Errorf("beep event lacks %s", k)
		}
	}
	if v := strings.TrimSpace(h.sw.API("uuid_getvar " + uuid + " avmd_detect")); v != "TRUE" {
		t.Errorf("avmd_detect = %q", v)
	}
	waitSub(t, sub, "avmd::stop", 3*time.Second) // one beep ends the detector
	for i := 30; i < 60; i++ {
		send(sineUlaw(1000, 8000, i*160, 160))
	}
	select {
	case e := <-sub.C:
		t.Fatalf("a second beep fired %s although avmd stopped", e.Subclass())
	case <-time.After(300 * time.Millisecond):
	}
	_, _ = call.Bye()
	h.waitChannels(0)
}
