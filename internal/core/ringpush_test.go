package core

import "testing"

func TestInviteCallID(t *testing.T) {
	if got := inviteCallID("0b0e8a2c-1d3f-4c55-9a77-2f7e3b6d1a10"); got != "0b0e8a2c-1d3f-4c55-9a77-2f7e3b6d1a10" {
		t.Fatalf("ring id not kept: %q", got)
	}
	for _, bad := range []string{"", "  ", "a b", "x@y", "<id>", "a\r\nVia: x"} {
		if got := inviteCallID(bad); got == bad || got == "" {
			t.Fatalf("%q should give a fresh Call-ID, got %q", bad, got)
		}
	}
}

func TestHashInsertSelectDelete(t *testing.T) {
	c := &Core{}
	if out := c.callFunc("hash", "insert/ringq_ring/abc/2/1", nil, 0); out != "+OK" {
		t.Fatalf("insert: %q", out)
	}
	if out := c.callFunc("hash", "select/ringq_ring/abc", nil, 0); out != "2/1" {
		t.Fatalf("select: %q", out)
	}
	if out := c.callFunc("hash", "delete/ringq_ring/abc", nil, 0); out != "+OK" {
		t.Fatalf("delete: %q", out)
	}
	if out := c.callFunc("hash", "exists/ringq_ring/abc", nil, 0); out != "false" {
		t.Fatalf("exists after delete: %q", out)
	}
}
