package db

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/ringq-devs/orswitch/internal/core"
)

// The row RingQ Core's call record import writes into q_ringq_cdr.
//
// On a server that runs RingQ Core's jobs, Core's node service listens to this switch's events, builds one record
// per call outcome (with the wording the Reports page reads: last_arg "Inbound Answered", "9000 - Queue Answered",
// "Queue Caller Abandoned" ...) and its calldetailrecord job inserts it; the writer then leaves q_ringq_cdr alone
// (coreImportsCalls). Where those jobs do not run (a test server, a stand-alone switch) and q_ringq_cdr is listed
// in cdr-tables, the writer makes the same row itself, from the A leg's record and the headers of its event.

// numberKind classifies a dialled number as RingQ Core does (node/server.js extensionRules).
func numberKind(num string) string {
	n, err := strconv.Atoi(strings.TrimSpace(num))
	if err != nil {
		return "InternalExtension"
	}
	switch {
	case n >= 9000 && n <= 9500:
		return "Queue"
	case n >= 3000 && n <= 3100:
		return "SpecialIVR"
	case n >= 5555 && n <= 5600:
		return "AI"
	case n >= 7777 && n <= 7780:
		return "ExternalAI"
	case n >= 6000 && n <= 6500:
		return "IVR"
	case n >= 2000 && n <= 2500:
		return "Teams"
	case n >= 4000 && n <= 4020:
		return "Conference"
	case n >= 10000:
		return "ExternalNumber"
	}
	return "InternalExtension"
}

// titleWords upper-cases the first letter of every word (PHP ucwords), as the AUX history shows codes.
func titleWords(s string) string {
	up := true
	return strings.Map(func(r rune) rune {
		if up && unicode.IsLetter(r) {
			up = false
			return unicode.ToUpper(r)
		}
		up = r == ' ' || r == '-' || r == '_'
		return r
	}, s)
}

// coreOutcome is the call's outcome in the words RingQ Core stores in q_ringq_cdr.last_arg, and the direction it
// gives that kind of record.
func coreOutcome(v map[string]string, otherDest string, answered, bridged bool) (lastArg, direction string) {
	cause := firstNonEmpty(v["hangup_cause"])
	callDir := firstNonEmpty(v["call_direction"], "local")
	dialled := firstNonEmpty(v["caller_destination"], v["destination_number"])
	qext, _, _ := strings.Cut(v["cc_queue"], "@")
	kind := numberKind(firstNonEmpty(qext, dialled))
	external := func(inbound, outbound string) string {
		if kind == "ExternalNumber" {
			return outbound
		}
		return inbound
	}
	switch {
	case qext != "":
		switch strings.ToLower(v["cc_result"]) {
		case "answered":
			return qext + " - Queue Answered", "inbound"
		case "abandoned":
			return "Queue Caller Abandoned", callDir
		case "":
			if v["cc_queue_answered_epoch"] != "" && v["cc_queue_answered_epoch"] != "0" {
				return qext + " - Queue Answered", "inbound"
			}
			return "Queue Missed", callDir
		}
		return "Queue Missed", callDir // timeout, no agent, key press
	case !bridged && (v["originate_disposition"] == "USER_BUSY" || v["originate_disposition"] == "CALL_REJECTED"):
		// the callee refused the call and the dialplan went on (voicemail, a busy tone)
		return external("Inbound Callee Busy", "Outbound Callee Busy"), external("inbound", "outbound")
	case answered && bridged:
		if numberKind(firstNonEmpty(otherDest, dialled)) == "ExternalNumber" {
			return "Outbound Answered", "outbound"
		}
		return "Inbound Answered", "inbound"
	case answered && kind == "AI":
		return "AI Answered", callDir
	case answered && kind == "IVR":
		return "IVR Answered", callDir
	case !answered && (cause == "USER_BUSY" || cause == "CALL_REJECTED") && bridged:
		return external("Inbound Callee Busy", "Outbound Callee Busy"), external("inbound", "outbound")
	case cause == "ORIGINATOR_CANCEL":
		return external("Inbound Caller Abandoned", "Outbound Caller Abandoned"), callDir
	case cause == "NO_ANSWER" || v["voicemail_message"] == "true":
		return external("Inbound Missed", "Outbound Missed"), callDir
	case cause == "UNALLOCATED_NUMBER", cause == "NO_ROUTE_DESTINATION", cause == "NORMAL_UNSPECIFIED", cause == "OUTGOING_CALL_BARRED",
		cause == "INCOMPATIBLE_DESTINATION", cause == "SERVICE_UNAVAILABLE", cause == "SUBSCRIBER_ABSENT", cause == "NO_USER_RESPONSE",
		cause == "USER_NOT_REGISTERED":
		switch kind {
		case "AI":
			return "AI Failed", callDir
		case "IVR":
			return "IVR Failed", callDir
		}
		return external("Inbound Failed", "Outbound Failed"), callDir
	}
	return external("Inbound Missed", "Outbound Missed"), callDir
}

// coreRow turns the v_xml_cdr row of a call into the q_ringq_cdr row RingQ Core would have imported.
func coreRow(row map[string]string, e *core.Event, v map[string]string, answered, bridged bool) map[string]string {
	out := make(map[string]string, len(row))
	for k, val := range row {
		out[k] = val
	}
	otherDest := e.Get("Other-Leg-Destination-Number")
	last, dir := coreOutcome(v, otherDest, answered, bridged)
	out["last_arg"], out["direction"] = last, dir
	out["caller_id_number"] = firstNonEmpty(v["effective_caller_id_number"], v["caller_id_number"])
	out["caller_id_name"] = firstNonEmpty(v["effective_caller_id_name"], v["caller_id_name"])
	out["source_number"] = v["caller_id_number"]
	out["cc_agent"] = v["cc_agent"]
	out["waitsec"] = firstNonEmpty(v["waitsec"], "0")
	out["record_length"] = firstNonEmpty(v["record_seconds"], "0")
	answeredByPerson := strings.HasSuffix(last, "Answered") && !strings.HasPrefix(last, "AI") && !strings.HasPrefix(last, "IVR")
	out["missed_call"] = strconv.FormatBool(!answeredByPerson)
	if bridged && otherDest != "" {
		out["caller_destination"], out["destination_number"] = otherDest, otherDest
	}
	if strings.HasSuffix(last, "- Queue Answered") {
		// Core logs a queue call from the agent's leg: it talked from the agent's answer; the caller's wait is
		// the time to that answer.
		ep := func(k string) int64 { n, _ := strconv.ParseInt(v[k], 10, 64); return n }
		if ans, joined, end := ep("cc_queue_answered_epoch"), ep("cc_queue_joined_epoch"), ep("end_epoch"); ans > 0 {
			out["answer_epoch"], out["answer_stamp"] = strconv.FormatInt(ans, 10), epochStamp(strconv.FormatInt(ans, 10))
			out["billsec"] = strconv.FormatInt(max(end-ans, 0), 10)
			if joined > 0 && ans >= joined {
				out["waitsec"] = strconv.FormatInt(ans-joined, 10)
			}
		}
	}
	return out
}
