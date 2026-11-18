package core_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// Tests of the call-feature scripts (script_followme.go, script_callflow.go, script_park.go, script_spy.go,
// script_page.go, script_misc.go, script_fifo.go, script_intercept.go).

const cfDomain = "127.0.0.1"

// cfDB is a fake RingQ database: rows answers the SELECTs of the scripts, execs records the writes.
type cfDB struct {
	mu    sync.Mutex
	rows  func(q string, args []any) []map[string]string
	execs []string
}

func (d *cfDB) install(h *harness) {
	h.sw.SetSQL(func(_ context.Context, q string, args ...any) ([]map[string]string, error) {
		if d.rows == nil {
			return nil, nil
		}
		return d.rows(q, args), nil
	})
	h.sw.SetSQLExec(func(_ context.Context, q string, args ...any) (int64, error) {
		d.mu.Lock()
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = fmt.Sprint(a)
		}
		d.execs = append(d.execs, q+" | "+strings.Join(parts, " "))
		d.mu.Unlock()
		return 1, nil
	})
}

func (d *cfDB) has(parts ...string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.execs {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(e, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func cfHarness(t *testing.T, extra string, vars map[string]string, ids ...string) *harness {
	t.Helper()
	dp := strings.Replace(testDialplan, `</context></include>`, extra+"\n</context></include>", 1)
	if len(ids) == 0 {
		ids = []string{"1000", "1001", "1002", "1003"}
	}
	users := "<include>"
	for _, u := range ids {
		users += fmt.Sprintf(`<user id="%s"><params><param name="password" value="%s"/></params><variables>`+
			`<variable name="effective_caller_id_number" value="%s"/><variable name="effective_caller_id_name" value="Ext %s"/>`+
			`<variable name="user_context" value="default"/>`, u, testPassword, u, u)
		if v := vars[u]; v != "" {
			for _, kv := range strings.Split(v, ";") {
				k, val, _ := strings.Cut(kv, "=")
				users += fmt.Sprintf(`<variable name="%s" value="%s"/>`, k, val)
			}
		}
		users += `</variables></user>`
	}
	users += "</include>"
	return newHarnessWith(t, "", users, dp, func(c *config.Config) {})
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nonSilent(p []byte) bool {
	for _, b := range p {
		if b != 0xff && b != 0x7f {
			return true
		}
	}
	return false
}

// hears sends payloads from one side until the other side hears it (relayed intact).
func rtpFlows(from *net.UDPConn, to netip.AddrPort, at *net.UDPConn) bool {
	payload := ulaw(2000, 160)
	for i := 0; i < 40; i++ {
		_ = testua.SendRTP(from, to, 0, uint16(i), uint32(i*160), payload)
		if _, p, err := testua.RecvRTP(at, 100*time.Millisecond, func(pt uint8, p []byte) bool { return pt == 0 && bytes.Equal(p, payload) }); err == nil && bytes.Equal(p, payload) {
			return true
		}
	}
	return false
}

// hearsAnything is true when the phone receives audio that is not silence within d (the switch plays it).
func hearsAnything(at *net.UDPConn, d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if _, p, err := testua.RecvRTP(at, 200*time.Millisecond, nil); err == nil && nonSilent(p) {
			return true
		}
	}
	return false
}

func dtmf(c *testua.Call, digits string) {
	for _, d := range digits {
		_, _ = c.Request("INFO", "application/dtmf-relay", []byte("Signal="+string(d)+"\r\nDuration=160\r\n"))
		time.Sleep(60 * time.Millisecond)
	}
}

func channelOf(h *harness, user string) string {
	for _, ch := range h.sw.Channels() {
		if ch.Var("user_name") == user && ch.Direction == "inbound" {
			return ch.UUID
		}
	}
	return ""
}

// follow_me.lua: *21 toggles follow me and saves it.
func TestScriptFollowMeToggle(t *testing.T) {
	h := cfHarness(t, `<extension name="fm"><condition field="destination_number" expression="^8020$">
    <action application="set" data="domain_uuid=du1"/><action application="set" data="extension_uuid=eu1"/>
    <action application="set" data="domain_name=127.0.0.1"/><action application="lua" data="follow_me.lua"/>
  </condition></extension>`, nil)
	db := &cfDB{rows: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "FROM v_extensions") {
			return []map[string]string{{"extension": "1000", "follow_me_uuid": "fm1", "follow_me_enabled": "false"}}
		}
		return nil
	}}
	db.install(h)
	alice := h.phone("1000")
	call, err := alice.Dial("8020", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("*21 call: %v", err)
	}
	waitUntil(t, "follow me saved", 10*time.Second, func() bool {
		return db.has("UPDATE v_follow_me", "true du1 fm1") && db.has("UPDATE v_extensions", "follow_me_enabled", "do_not_disturb = 'false'")
	})
	if h.sw.UserVar("1000", cfDomain, "follow_me_enabled") != "true" {
		t.Fatalf("the directory does not show follow me on: %q", h.sw.UserVar("1000", cfDomain, "follow_me_enabled"))
	}
	waitUntil(t, "the call to end", 10*time.Second, func() bool { return len(h.sw.Channels()) == 0 })
}

func followMeDB(prompt string) *cfDB {
	return &cfDB{rows: func(q string, args []any) []map[string]string {
		switch {
		case strings.Contains(q, "v_follow_me_destinations"):
			return []map[string]string{{"domain_uuid": "du1", "domain_name": cfDomain, "destination_number": "1002",
				"destination_delay": "0", "destination_timeout": "10", "destination_prompt": prompt}}
		case strings.Contains(q, "FROM v_extensions"):
			return []map[string]string{{"follow_me_uuid": "fm1"}}
		}
		return nil
	}}
}

const followMeDialplan = `<extension name="fmapp"><condition field="destination_number" expression="^8021$">
    <action application="set" data="domain_uuid=du1"/><action application="set" data="domain_name=127.0.0.1"/>
    <action application="lua" data="app.lua follow_me fm1"/>
  </condition></extension>`

// app.lua follow_me: the destinations of the follow me ring, and the "prompt to accept" asks the person who answered.
func TestScriptFollowMeApp(t *testing.T) {
	for _, tc := range []struct {
		name, prompt, digit string
		connected           bool
	}{{"plain", "", "", true}, {"accept", "1", "1", true}, {"reject", "1", "2", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := cfHarness(t, followMeDialplan, nil)
			followMeDB(tc.prompt).install(h)
			alice, bob := h.phone("1000"), h.phone("1002")
			type res struct {
				c   *testua.Call
				err error
			}
			done := make(chan res, 1)
			go func() { c, err := alice.Dial("8021", []int{0}, 15*time.Second, false); done <- res{c, err} }()
			in, err := bob.WaitInvite(6 * time.Second)
			if err != nil {
				t.Fatalf("the follow me destination was not rung: %v", err)
			}
			_ = in.Ring()
			if err := in.Answer(0); err != nil {
				t.Fatal(err)
			}
			if tc.digit != "" {
				select {
				case <-done:
					t.Fatal("the caller was connected before the person who answered pressed a key")
				case <-time.After(1200 * time.Millisecond):
				}
				dtmf(in.Dialog(), tc.digit)
			}
			r := <-done
			if tc.connected {
				if r.err != nil || r.c.Final.StatusCode != 200 {
					t.Fatalf("the caller was not connected: %v", r.err)
				}
				if !rtpFlows(r.c.RTP, r.c.RemoteRTP, in.RTP) {
					t.Fatal("no audio between the caller and the follow me destination")
				}
			} else if r.err == nil && r.c.Final.StatusCode == 200 {
				t.Fatal("a rejected call was connected")
			}
		})
	}
}

// call_flow.lua: the feature code toggles the flow (after the PIN), the other numbers go to the active destination.
func TestScriptCallFlow(t *testing.T) {
	flow := func(status, pin, app, data string) []map[string]string {
		return []map[string]string{{"call_flow_status": status, "call_flow_pin_number": pin, "call_flow_feature_code": "*77",
			"call_flow_label": "Day", "call_flow_alternate_label": "Night", "call_flow_app": app, "call_flow_data": data,
			"call_flow_alternate_app": "hangup", "call_flow_alternate_data": "NORMAL_CLEARING"}}
	}
	dp := `<extension name="cf"><condition field="destination_number" expression="^8031$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="set" data="call_flow_uuid=cf1"/>
    <action application="set" data="feature_code=true"/><action application="lua" data="call_flow.lua"/>
  </condition></extension>
  <extension name="cfgo"><condition field="destination_number" expression="^8032$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="set" data="call_flow_uuid=cf1"/>
    <action application="lua" data="call_flow.lua"/>
  </condition></extension>`
	t.Run("toggle", func(t *testing.T) {
		h := cfHarness(t, dp, nil)
		db := &cfDB{rows: func(q string, a []any) []map[string]string { return flow("true", "", "transfer", "9196") }}
		db.install(h)
		call, err := h.phone("1000").Dial("8031", []int{0}, 5*time.Second, false)
		if err != nil || call.Final.StatusCode != 200 {
			t.Fatalf("call: %v", err)
		}
		waitUntil(t, "the status saved", 3*time.Second, func() bool { return db.has("UPDATE v_call_flows", "false cf1") })
		if !h.sw.LampOn("*77", cfDomain) || !h.sw.LampOn("flow+*77", cfDomain) {
			t.Fatal("the lamps of the call flow (alternate destination active) are not lit")
		}
	})
	t.Run("pin", func(t *testing.T) {
		h := cfHarness(t, dp, nil)
		db := &cfDB{rows: func(q string, a []any) []map[string]string { return flow("false", "1234", "transfer", "9196") }}
		db.install(h)
		alice := h.phone("1000")
		call, err := alice.Dial("8031", []int{0}, 5*time.Second, false)
		if err != nil || call.Final.StatusCode != 200 {
			t.Fatalf("call: %v", err)
		}
		time.Sleep(800 * time.Millisecond)
		dtmf(call, "1234#")
		waitUntil(t, "the status saved after the PIN", 3*time.Second, func() bool { return db.has("UPDATE v_call_flows", "true cf1") })
		if h.sw.LampOn("*77", cfDomain) {
			t.Fatal("the lamp should be off while the main destination is active")
		}
	})
	t.Run("wrong pin", func(t *testing.T) {
		h := cfHarness(t, dp, nil)
		db := &cfDB{rows: func(q string, a []any) []map[string]string { return flow("false", "1234", "transfer", "9196") }}
		db.install(h)
		call, err := h.phone("1000").Dial("8031", []int{0}, 5*time.Second, false)
		if err != nil || call.Final.StatusCode != 200 {
			t.Fatalf("call: %v", err)
		}
		for i := 0; i < 3; i++ {
			time.Sleep(700 * time.Millisecond)
			dtmf(call, "9999#")
		}
		waitUntil(t, "the call to be hung up", 8*time.Second, func() bool { return len(h.sw.Channels()) == 0 })
		if db.has("UPDATE v_call_flows") {
			t.Fatal("the flow was toggled with a wrong PIN")
		}
	})
	t.Run("destination", func(t *testing.T) {
		h := cfHarness(t, dp, nil)
		db := &cfDB{rows: func(q string, a []any) []map[string]string { return flow("true", "", "transfer", "9196") }}
		db.install(h)
		call, err := h.phone("1000").Dial("8032", []int{0}, 5*time.Second, false)
		if err != nil || call.Final.StatusCode != 200 {
			t.Fatalf("the main destination (echo) did not answer: %v", err)
		}
		if db.has("UPDATE") {
			t.Fatal("a plain call must not change the flow")
		}
	})
}

