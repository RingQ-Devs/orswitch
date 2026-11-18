package db

import "testing"

func TestCoreOutcome(t *testing.T) {
	for _, tc := range []struct {
		name               string
		v                  map[string]string
		other              string
		answered, bridged  bool
		wantArg, wantDir string
	}{
		{"internal call answered", map[string]string{"call_direction": "local", "destination_number": "1003", "hangup_cause": "NORMAL_CLEARING"}, "1003", true, true, "Inbound Answered", "inbound"},
		{"call out to a number", map[string]string{"call_direction": "outbound", "destination_number": "6591234567", "hangup_cause": "NORMAL_CLEARING"}, "6591234567", true, true, "Outbound Answered", "outbound"},
		{"caller hangs up while ringing", map[string]string{"call_direction": "local", "destination_number": "1003", "hangup_cause": "ORIGINATOR_CANCEL"}, "", false, false, "Inbound Caller Abandoned", "local"},
		{"callee rejects, dialplan goes on to voicemail", map[string]string{"call_direction": "local", "destination_number": "1003", "originate_disposition": "CALL_REJECTED", "hangup_cause": "NORMAL_CLEARING"}, "", true, false, "Inbound Callee Busy", "inbound"},
		{"callee rejects", map[string]string{"call_direction": "local", "destination_number": "1003", "hangup_cause": "CALL_REJECTED"}, "1003", false, true, "Inbound Callee Busy", "inbound"},
		{"no answer, voicemail", map[string]string{"call_direction": "local", "destination_number": "1003", "hangup_cause": "NORMAL_CLEARING", "voicemail_message": "true"}, "", true, false, "Inbound Missed", "local"},
		{"queue answered", map[string]string{"call_direction": "local", "destination_number": "9000", "cc_queue": "9000@d", "cc_result": "answered"}, "1000", true, true, "9000 - Queue Answered", "inbound"},
		{"queue abandoned", map[string]string{"call_direction": "local", "destination_number": "9000", "cc_queue": "9000@d", "cc_result": "abandoned"}, "", true, false, "Queue Caller Abandoned", "local"},
		{"queue timeout", map[string]string{"call_direction": "inbound", "destination_number": "9000", "cc_queue": "9000@d", "cc_result": "timeout"}, "", true, false, "Queue Missed", "inbound"},
		{"AI agent", map[string]string{"call_direction": "local", "destination_number": "5555", "hangup_cause": "NORMAL_CLEARING"}, "", true, false, "AI Answered", "local"},
		{"IVR", map[string]string{"call_direction": "local", "destination_number": "6000", "hangup_cause": "NORMAL_CLEARING"}, "", true, false, "IVR Answered", "local"},
		{"no route", map[string]string{"call_direction": "local", "destination_number": "1099", "hangup_cause": "UNALLOCATED_NUMBER"}, "", false, false, "Inbound Failed", "local"},
		{"outbound failed", map[string]string{"call_direction": "outbound", "destination_number": "6591234567", "hangup_cause": "NO_ROUTE_DESTINATION"}, "", false, false, "Outbound Failed", "outbound"},
	} {
		arg, dir := coreOutcome(tc.v, tc.other, tc.answered, tc.bridged)
		if arg != tc.wantArg || dir != tc.wantDir {
			t.Errorf("%s: got %q / %q, want %q / %q", tc.name, arg, dir, tc.wantArg, tc.wantDir)
		}
	}
}

func TestNumberKindAndTitleWords(t *testing.T) {
	for num, want := range map[string]string{"1003": "InternalExtension", "9000": "Queue", "3001": "SpecialIVR", "5555": "AI", "6000": "IVR", "4005": "Conference", "6591234567": "ExternalNumber", "abc": "InternalExtension"} {
		if got := numberKind(num); got != want {
			t.Errorf("numberKind(%s) = %s, want %s", num, got, want)
		}
	}
	if got := titleWords("auxcode 1 on-break"); got != "Auxcode 1 On-Break" {
		t.Errorf("titleWords: %q", got)
	}
}
