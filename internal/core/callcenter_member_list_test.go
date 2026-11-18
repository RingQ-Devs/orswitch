package core_test

import (
	"strings"
	"testing"
	"time"
)

// RingQ Core's queue service counts the callers of the wallboard with
// {"command":"callcenter_config","data":{"arguments":"member list"}} (no queue): every queue's callers, with
// their state, as rows.
func TestCallcenterMemberListAllQueues(t *testing.T) {
	h := tenantHarness(t, nil, queueConfig("ring-all", nil))
	// nobody waiting: the table is there, with its columns
	out := h.sw.API("callcenter_config member list")
	if !strings.HasPrefix(out, "queue|instance_id|uuid|session_uuid|cid_number|cid_name|") || !strings.Contains(out, "+OK") {
		t.Fatalf("empty member list: %q", out)
	}
	js := h.sw.API(`json {"command":"callcenter_config","data":{"arguments":"member list"}}`)
	if !strings.Contains(js, `"status":"success"`) || !strings.Contains(js, `"response":[]`) {
		t.Fatalf("json member list without callers: %s", js)
	}
	// a caller in the queue
	caller := h.tenantPhone("1000", "a.example")
	go func() { _, _ = caller.Dial("3000", []int{0}, 10*time.Second, false) }()
	h.waitFor("a member in the list", func() bool {
		return strings.Contains(h.sw.API("callcenter_config member list"), "3000@a.example|single_box|")
	})
	out = h.sw.API("callcenter_config member list")
	if strings.Count(out, "queue|instance_id") != 1 {
		t.Fatalf("one header expected:\n%s", out)
	}
	if one := h.sw.API("callcenter_config member list 3000@a.example"); !strings.Contains(one, "3000@a.example|single_box|") {
		t.Fatalf("member list of one queue: %s", one)
	}
	js = h.sw.API(`json {"command":"callcenter_config","data":{"arguments":"member list"}}`)
	if !strings.Contains(js, `"queue":"3000@a.example"`) || !strings.Contains(js, `"state":`) {
		t.Fatalf("json member list: %s", js)
	}
}
