package core_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/core"
)

// ccEvents collects callcenter::info events until pred is satisfied or the time is up.
func ccEvents(t *testing.T, sub *core.Subscription, wait time.Duration, pred func(all []*core.Event) bool) []*core.Event {
	t.Helper()
	var all []*core.Event
	deadline := time.After(wait)
	for {
		if pred(all) {
			return all
		}
		select {
		case e := <-sub.C:
			all = append(all, e)
		case <-deadline:
			return all
		}
	}
}

func ccFind(all []*core.Event, action string) *core.Event {
	for _, e := range all {
		if e.Get("CC-Action") == action {
			return e
		}
	}
	return nil
}

// RingQ Core's wallboard is built from callcenter::info events and "callcenter_config member list".
// This pins what it reads (see ringq_core node/server.js and queue.js) to what mod_callcenter sends.
func TestQueueWallboardData(t *testing.T) {
	h := tenantHarness(t, nil, queueConfig("longest-idle-agent", nil))
	sub := h.sw.Events().Subscribe(512, func(e *core.Event) bool { return e.Subclass() == "callcenter::info" })
	defer sub.Close()
	caller := h.tenantPhone("1000", "a.example")
	a1, a2 := h.tenantPhone("1001", "a.example"), h.tenantPhone("1002", "a.example")

	// An answered call.
	call, err := caller.Dial("3000", nil, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	_, in := waitAny(t, 5*time.Second, a1, a2)
	if in == nil {
		t.Fatal("no agent was offered the call")
	}
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	h.waitFor("bridged", func() bool { return strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA") == 2 })
	_, _ = call.Bye()
	all := ccEvents(t, sub, 5*time.Second, func(all []*core.Event) bool { return ccFind(all, "member-queue-end") != nil })

	start := ccFind(all, "member-queue-start")
	if start == nil {
		t.Fatal("no member-queue-start")
	}
	// Core only counts the caller while the channel is ACTIVE, and keys the queue board on these.
	for k, want := range map[string]string{"Channel-Call-State": "ACTIVE", "variable_domain_name": "a.example",
		"variable_cc_queue": "3000@a.example", "CC-Queue": "3000@a.example", "Caller-Destination-Number": "3000"} {
		if got := start.Get(k); got != want {
			t.Errorf("member-queue-start %s = %q, want %q", k, got, want)
		}
	}
	for _, k := range []string{"Channel-Call-UUID", "Unique-ID", "variable_cc_queue_joined_epoch", "variable_caller_id_number",
		"Caller-Channel-Answered-Time", "CC-Member-Session-UUID", "CC-Member-CID-Number", "CC-Member-Joined-Time"} {
		if start.Get(k) == "" {
			t.Errorf("member-queue-start lacks %s", k)
		}
	}
	off := ccFind(all, "agent-offering")
	if off == nil {
		t.Fatal("no agent-offering event")
	}
	for _, k := range []string{"CC-Agent", "CC-Agent-System", "CC-Agent-Type", "CC-Agent-Called-Time", "CC-Member-Session-UUID", "Channel-Call-UUID"} {
		if off.Get(k) == "" {
			t.Errorf("agent-offering lacks %s", k)
		}
	}
	bs := ccFind(all, "bridge-agent-start")
	if bs == nil {
		t.Fatal("no bridge-agent-start event")
	}
	for _, k := range []string{"CC-Agent", "CC-Agent-UUID", "CC-Agent-Called-Time", "CC-Agent-Answered-Time", "CC-Member-Session-UUID"} {
		if bs.Get(k) == "" {
			t.Errorf("bridge-agent-start lacks %s", k)
		}
	}
	if bs.Get("CC-Agent-UUID") == bs.Get("CC-Agent") {
		t.Error("CC-Agent-UUID must be the agent's channel, CC-Agent the agent's name")
	}
	be := ccFind(all, "bridge-agent-end")
	if be == nil || be.Get("CC-Bridge-Terminated-Time") == "" || be.Get("CC-Hangup-Cause") == "" {
		t.Errorf("bridge-agent-end incomplete: %v", be)
	}
	end := ccFind(all, "member-queue-end")
	if end.Get("CC-Cause") != "Terminated" || end.Get("CC-Cancel-Reason") != "" {
		t.Errorf("answered member-queue-end: cause %q reason %q", end.Get("CC-Cause"), end.Get("CC-Cancel-Reason"))
	}
	for _, k := range []string{"CC-Member-Leaving-Time", "CC-Member-Joined-Time", "CC-Agent-Answered-Time"} {
		if end.Get(k) == "" {
			t.Errorf("member-queue-end lacks %s", k)
		}
	}
	h.waitChannels(0)
	if out := h.sw.API("callcenter_config member list"); strings.Contains(out, "|Answered") {
		t.Errorf("an answered caller who left stays in the member list:\n%s", out)
	}

	// A caller who hangs up while the agents ring.
	call, err = caller.Dial("3000", nil, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, in = waitAny(t, 5*time.Second, a1, a2); in == nil {
		t.Fatal("no offer for the second call")
	}
	time.Sleep(1200 * time.Millisecond)
	before := time.Now().Unix()
	_, _ = call.Bye()
	all = ccEvents(t, sub, 5*time.Second, func(all []*core.Event) bool {
		e := ccFind(all, "member-queue-end")
		return e != nil && e.Get("CC-Cause") == "Cancel"
	})
	end = ccFind(all, "member-queue-end")
	if end == nil {
		t.Fatal("no member-queue-end for the abandoned call")
	}
	if end.Get("CC-Cause") != "Cancel" || end.Get("CC-Cancel-Reason") != "BREAK_OUT" || end.Get("Answer-State") != "hangup" {
		t.Errorf("abandoned: cause %q reason %q answer-state %q, want Cancel BREAK_OUT hangup",
			end.Get("CC-Cause"), end.Get("CC-Cancel-Reason"), end.Get("Answer-State"))
	}
	if end.Get("CC-Member-UUID") == "" || end.Get("variable_domain_name") != "a.example" {
		t.Errorf("abandoned member-queue-end lacks the member or domain: %q %q", end.Get("CC-Member-UUID"), end.Get("variable_domain_name"))
	}
	// The agent whose phone rang when the caller left "missed" the call (Core counts ORIGINATOR_CANCEL).
	more := ccEvents(t, sub, 2*time.Second, func(x []*core.Event) bool { return ccFind(append(all, x...), "bridge-agent-fail") != nil })
	if f := ccFind(append(all, more...), "bridge-agent-fail"); f == nil || f.Get("CC-Hangup-Cause") != "ORIGINATOR_CANCEL" ||
		f.Get("CC-Member-Session-UUID") == "" || f.Get("CC-Agent") == "" {
		t.Errorf("caller left while the agent's phone rang: want bridge-agent-fail ORIGINATOR_CANCEL with member and agent, got %v", f)
	}
	// Core reads an abandoned call's wait from the member list row (abandoned_epoch - joined_epoch).
	h.waitChannels(0)
	out := h.sw.API("callcenter_config member list")
	var row []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasSuffix(l, "|Abandoned") {
			row = strings.Split(l, "|")
		}
	}
	if len(row) != 16 {
		t.Fatalf("the abandoned caller is not in the member list:\n%s", out)
	}
	joined, _ := strconv.ParseInt(row[7], 10, 64)
	abandoned, _ := strconv.ParseInt(row[10], 10, 64)
	if row[2] != end.Get("CC-Member-UUID") || abandoned < before || abandoned-joined < 1 || abandoned-joined > 4 {
		t.Errorf("abandoned row uuid %s joined %d abandoned_epoch %d (left about %d)", row[2], joined, abandoned, before)
	}
	if n := strings.TrimSpace(h.sw.API("callcenter_config queue count members 3000@a.example")); n != "0" {
		t.Errorf("queue count members = %s, abandoned callers are not waiting", n)
	}
}

// Core's "Missed" count for an agent (wallboard Agent Details) keys on the caller's session uuid in
// bridge-agent-fail.
func TestQueueAgentNoAnswerEvent(t *testing.T) {
	h := tenantHarness(t, nil, queueConfig("ring-all", nil))
	sub := h.sw.Events().Subscribe(512, func(e *core.Event) bool { return e.Subclass() == "callcenter::info" })
	defer sub.Close()
	caller := h.tenantPhone("1000", "a.example")
	_, _ = h.tenantPhone("1001", "a.example"), h.tenantPhone("1002", "a.example")

	call, err := caller.Dial("3000", nil, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	all := ccEvents(t, sub, 12*time.Second, func(all []*core.Event) bool { return ccFind(all, "bridge-agent-fail") != nil })
	_, _ = call.Bye()
	fail := ccFind(all, "bridge-agent-fail")
	if fail == nil {
		t.Fatal("no bridge-agent-fail although nobody answered")
	}
	for _, k := range []string{"CC-Agent", "CC-Member-Session-UUID", "CC-Member-UUID", "CC-Queue", "CC-Hangup-Cause"} {
		if fail.Get(k) == "" {
			t.Errorf("bridge-agent-fail lacks %s", k)
		}
	}
	if c := fail.Get("CC-Hangup-Cause"); c != "NO_ANSWER" && c != "NO_USER_RESPONSE" {
		t.Errorf("CC-Hangup-Cause = %q", c)
	}
}
