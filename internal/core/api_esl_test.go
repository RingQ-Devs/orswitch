package core

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSchedulerFiresAndDeletesByGroup(t *testing.T) {
	var s scheduler
	fired := make(chan string, 4)
	s.add("uuid-a", 20*time.Millisecond, func() { fired <- "a" })
	s.add("uuid-b", 20*time.Millisecond, func() { fired <- "b" })
	s.add("uuid-b", 20*time.Millisecond, func() { fired <- "b2" })
	if s.count() != 3 {
		t.Fatalf("count = %d, want 3", s.count())
	}
	if n := s.del("uuid-b"); n != 2 {
		t.Fatalf("del group removed %d, want 2", n)
	}
	select {
	case got := <-fired:
		if got != "a" {
			t.Fatalf("fired %q, want a", got)
		}
	case <-time.After(time.Second):
		t.Fatal("task a did not fire")
	}
	select {
	case got := <-fired:
		t.Fatalf("deleted task %q fired", got)
	case <-time.After(100 * time.Millisecond):
	}
	if s.count() != 0 {
		t.Fatalf("count = %d after firing, want 0", s.count())
	}
}

func TestSchedulerDeleteByID(t *testing.T) {
	var s scheduler
	id := s.add("g", time.Hour, func() {})
	if n := s.del("999"); n != 0 {
		t.Fatalf("unknown id removed %d", n)
	}
	if n := s.del(itoa(id)); n != 1 {
		t.Fatalf("del by id removed %d, want 1", n)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestTableToRows(t *testing.T) {
	out := "name|status|state\n1001@d|Available|Waiting\n1002@d|On Break|Idle\n+OK\n"
	got, ok := tableToRows(out).([]map[string]string)
	if !ok || len(got) != 2 {
		t.Fatalf("rows = %#v", tableToRows(out))
	}
	want := map[string]string{"name": "1002@d", "status": "On Break", "state": "Idle"}
	if !reflect.DeepEqual(got[1], want) {
		t.Fatalf("row 2 = %v, want %v", got[1], want)
	}
	if v := tableToRows("3\n"); v != "3" {
		t.Fatalf("non-table = %#v", v)
	}
	if rows, ok := tableToRows("name|status\n+OK\n").([]map[string]string); !ok || len(rows) != 0 {
		t.Fatalf("empty table = %#v", tableToRows("name|status\n+OK\n"))
	}
}

func TestJSONCommandErrors(t *testing.T) {
	out, _ := apiJSON(nil, []string{"not", "json"})
	var r map[string]string
	if err := json.Unmarshal([]byte(out), &r); err != nil || r["status"] != "error" {
		t.Fatalf("bad request reply = %q", out)
	}
	out, _ = apiJSON(nil, []string{`{"command":"json","data":{"arguments":"x"}}`})
	if !strings.Contains(out, `"error"`) {
		t.Fatalf("recursive reply = %q", out)
	}
}

func TestSchedTime(t *testing.T) {
	if d, err := schedTime("+5"); err != nil || d != 5*time.Second {
		t.Fatalf("+5 = %v, %v", d, err)
	}
	if _, err := schedTime("soon"); err == nil {
		t.Fatal("bad time accepted")
	}
}
