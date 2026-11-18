package core_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// status carries the lines RingQ Core's capacity test reads (CapacityTest::parseStatus).
func TestStatusHasFreeSwitchLines(t *testing.T) {
	h := newHarness(t, "")
	out := h.sw.API("status")
	for name, re := range map[string]string{
		"max":     `(\d+)\s+session\(s\)\s+max`,
		"per sec": `session\(s\)\s+per\s+Sec\s+out\s+of\s+max\s+(\d+)`,
		"current": `(?m)^\s*(\d+)\s+session\(s\)\s+-`,
	} {
		if !regexp.MustCompile(`(?i)` + re).MatchString(out) {
			t.Errorf("status lacks the %q line:\n%s", name, out)
		}
	}
	if m := regexp.MustCompile(`(\d+)\s+session\(s\)\s+max`).FindStringSubmatch(out); m == nil || m[1] != "1000" {
		t.Errorf("session max = %v, want 1000", m)
	}
}

// hupall with a variable only ends the calls that have it (the capacity test's cleanup).
func TestHupallVariableFilter(t *testing.T) {
	h := newHarness(t, "")
	alice := h.phone("1000")
	call, err := alice.Dial("9196", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("echo call failed: %v", err)
	}
	h.waitChannels(1)
	for _, cmd := range []string{
		"hupall NORMAL_CLEARING capacity_test run123",     // no channel has it
		"hupall NORMAL_CLEARING caller_id_number 9999999", // wrong value
	} {
		if out := h.sw.API(cmd); !strings.Contains(out, "hung up 0") {
			t.Fatalf("%s = %q, want nothing hung up", cmd, out)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(h.sw.Channels()); n != 1 {
		t.Fatalf("a filtered hupall ended the call (%d channels left)", n)
	}
	if out := h.sw.API("hupall NORMAL_CLEARING caller_id_number 1000"); !strings.Contains(out, "hung up 1") {
		t.Fatalf("matching hupall = %q", out)
	}
	h.waitChannels(0)
}

func TestShutdownCommand(t *testing.T) {
	var calls atomic.Int32
	var restart atomic.Bool
	defer core.SetShutdownExec(func(r bool) { calls.Add(1); restart.Store(r) })()
	h := newHarness(t, "")
	// cancel: nothing happens
	h.sw.API("shutdown elegant")
	h.sw.API("shutdown cancel")
	time.Sleep(1500 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("a cancelled shutdown ran")
	}
	if out := h.sw.API("fsctl shutdown restart"); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("fsctl shutdown restart = %q", out)
	}
	h.waitFor("the shutdown", func() bool { return calls.Load() == 1 })
	if !restart.Load() {
		t.Fatal("restart was not passed on")
	}
}

// The FreeSWITCH-format log: failed registrations and invites in the layout fail2ban reads,
// appended to a file somebody else may truncate (Call Debug).
func TestFSLogFile(t *testing.T) {
	h := newHarness(t, "")
	path := filepath.Join(t.TempDir(), "freeswitch.log")
	fl, err := h.sw.StartFSLog(h.logs, path)
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Stop()

	bad, err := testua.New("1001", "wrong-password", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if resp, err := bad.Register(300); err == nil && resp.StatusCode == 200 {
		t.Fatal("a wrong password registered")
	}
	read := func() string { b, _ := os.ReadFile(path); return string(b) }
	failure := regexp.MustCompile(`\[WARNING\] sofia_reg\.c:\d+ SIP auth failure \(REGISTER\) on sofia profile '.*' for \[1001@.*\] from ip 127\.0\.0\.1\n`)
	h.waitFor("the auth failure line", func() bool { return failure.MatchString(read()) })

	alice := h.phone("1000")
	call, err := alice.Dial("9196", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("echo call failed: %v", err)
	}
	invite := regexp.MustCompile(`\[INFO\] sofia\.c:\d+ receiving invite from 127\.0\.0\.1:\d+ version:`)
	h.waitFor("the invite line", func() bool { return invite.MatchString(read()) })

	// Call Debug truncates the file; what the switch logs next is appended from the start.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Dial("9196", []int{0}, 5*time.Second, false); err != nil {
		t.Fatal(err)
	}
	h.waitFor("lines after a truncate", func() bool { return read() != "" })
	if s := read(); strings.Contains(s, "\x00") {
		t.Fatalf("a hole in the log after truncating: %q", s[:40])
	}
}
