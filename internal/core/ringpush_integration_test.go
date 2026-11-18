package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// A call to an extension in "wake" mode fires ringq::ring, waits for the pushed
// app to register, then dials it with the ring id as the SIP Call-ID, and ends with
// ringq::ring_end.
func TestRingPushWake(t *testing.T) {
	users := "<include>"
	for _, u := range []string{"1000", "1001"} {
		users += `<user id="` + u + `"><params><param name="password" value="` + testPassword + `"/></params></user>`
	}
	users += "</include>"
	h := newHarnessWith(t, "", users, testDialplan, func(c *config.Config) {
		c.Directory.Default().RingPushMode = "wake"
	})
	sub := h.sw.Events().Subscribe(64, func(e *core.Event) bool { return strings.HasPrefix(e.Subclass(), "ringq::ring") })
	alice := h.phone("1000")

	// the push service (ringPush.js): answer the ring "1 phone pushed", and let the app wake and register
	bob, err := testua.New("1001", testPassword, h.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bob.Close)
	bob.UserAgent = "RingQ android - test-device"
	ringID := make(chan string, 1)
	go func() {
		for e := range sub.C {
			if e.Subclass() != "ringq::ring" {
				continue
			}
			if e.Get("ringq-ext") != "1001" || e.Get("ringq-mode") != "wake" || e.Get("ringq-registered") != "false" ||
				e.Get("ringq-caller-number") != "1000" {
				t.Errorf("unexpected ring event: %v", e.Headers)
			}
			id := e.Get("ringq-ring-id")
			ringID <- id
			time.Sleep(200 * time.Millisecond)
			h.sw.API("hash insert/ringq_ring/" + id + "/1/1")
			time.Sleep(700 * time.Millisecond)
			if resp, err := bob.Register(300); err != nil || resp.StatusCode != 200 {
				t.Errorf("woken app register: %v %v", resp, err)
			}
			return
		}
	}()

	done := make(chan error, 1)
	var call *testua.Call
	start := time.Now()
	go func() {
		var err error
		call, err = alice.Dial("1001", nil, 10*time.Second, false)
		done <- err
	}()
	in, err := bob.WaitInvite(8 * time.Second)
	if err != nil {
		t.Fatalf("the woken app got no INVITE: %v", err)
	}
	id := <-ringID
	if got := in.Invite.Get("Call-ID"); got != id {
		t.Fatalf("INVITE Call-ID %q should be the ring id %q", got, id)
	}
	if time.Since(start) < 800*time.Millisecond {
		t.Fatalf("the call did not wait for the app to register (%v)", time.Since(start))
	}
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if call.Final.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", call.Final.StatusCode)
	}
	select {
	case e := <-sub.C:
		if e.Subclass() != "ringq::ring_end" || e.Get("ringq-ring-id") != id || e.Get("ringq-answered") != "true" || e.Get("ringq-registered") != "true" {
			t.Fatalf("unexpected end event: %v", e.Headers)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no ringq::ring_end")
	}
}

// With nothing pushed (no mobile device) the call does not wait.
func TestRingPushNothingPushed(t *testing.T) {
	users := "<include>"
	for _, u := range []string{"1000", "1001"} {
		users += `<user id="` + u + `"><params><param name="password" value="` + testPassword + `"/></params></user>`
	}
	users += "</include>"
	h := newHarnessWith(t, "", users, testDialplan, func(c *config.Config) {
		c.Directory.Default().RingPushMode = "wake"
	})
	sub := h.sw.Events().Subscribe(64, func(e *core.Event) bool { return e.Subclass() == "ringq::ring" })
	go func() {
		for e := range sub.C {
			h.sw.API("hash insert/ringq_ring/" + e.Get("ringq-ring-id") + "/0/0")
		}
	}()
	alice := h.phone("1000")
	// 1001 is not registered and nothing was pushed: fails fast, user not registered
	start := time.Now()
	call, err := alice.Dial("1001", nil, 10*time.Second, false)
	if err == nil && call != nil && call.Final.StatusCode == 200 {
		t.Fatal("should not connect")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("waited %v with nothing pushed", time.Since(start))
	}
}
