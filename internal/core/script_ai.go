package core

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The AI Lua scripts of the RingQ backend, in Go: live_transcribe.lua, ivr_ai.lua,
// ivr_ai3.lua and transfer_ai_call.lua (ivr_ai2.lua is in script_stock.go).
//
//	live_transcribe.lua <uuid> <from> <to>   luarun'd by the "global-live-transcript" dialplan
//	                                         (api_on_answer): streams both sides of the call to
//	                                         node/transcript_live.js (live transcription, Agent Coach)
//	ivr_ai.lua | ivr_ai2.lua | ivr_ai3.lua   the "AI External" extensions: fire the custom event
//	                                         ai_ivr_recording | 2 | 3; node/server.js answers
//	                                         ai_ivr_recording2/3 with uuid_audio_stream ... ws://127.0.0.1:7777|3333
//	transfer_ai_call.lua <uuid> <ext>        what mod_openai_audio_stream runs when a transfer
//	                                         keyword is heard (openai_stream.go does the same itself)

func init() {
	RegisterScript("live_transcribe", scriptLiveTranscribe)
	RegisterScript("ivr_ai", func(s *Script) error { return scriptAIIVRNotify(s, "ai_ivr_recording") })
	RegisterScript("ivr_ai3", func(s *Script) error { return scriptAIIVRNotify(s, "ai_ivr_recording3") })
	RegisterScript("transfer_ai_call", scriptTransferAICall)
}

// liveTranscribeBridge is where live_transcribe.lua sends the audio (node/transcript_live.js).
var liveTranscribeBridge = "ws://127.0.0.1:5000"

// live_transcribe.lua <uuid> <from> <to>: starts the stereo 24 kHz tap towards the
// transcription bridge, with the call's details in the query string, and stays up as long as
// the call does (the Lua thread did; luarun'd scripts are not waited for).
func scriptLiveTranscribe(s *Script) error {
	uuid := strings.TrimSpace(s.Arg(1))
	from := firstNonEmpty(s.Arg(2), "unknown")
	to := firstNonEmpty(s.Arg(3), "unknown")
	if uuid == "" {
		s.Log("err", "[TRANSCRIPT] No UUID provided - exiting.")
		return nil
	}
	ch := s.Core.Channel(uuid)
	if ch == nil || ch.IsHungUp() {
		s.Log("err", "[TRANSCRIPT] Session not ready.", "uuid", uuid)
		return nil
	}
	s.Log("info", "[TRANSCRIPT] Starting audio stream", "uuid", uuid)

	ch.SetVar("X_UUID", uuid)
	ch.SetVar("X_FROM", from)
	ch.SetVar("X_TO", to)
	domain := firstNonEmpty(ch.Var("domain_name"), ch.Var("context"), "unknown")
	ch.SetVar("X_DOMAIN", domain)

	url := liveTranscribeBridge + "/?uuid=" + uuid + "&from=" + from + "&to=" + to + "&domain=" + domain
	if res := s.Core.API("uuid_audio_stream " + uuid + " start " + url + " stereo 24000"); !strings.HasPrefix(strings.TrimSpace(res), "+OK") {
		s.Log("err", "[TRANSCRIPT] uuid_audio_stream failed", "uuid", uuid, "result", strings.TrimSpace(res))
	}

	// keep the script alive while the call is up
	select {
	case <-ch.Done():
	case <-s.Ctx.Done():
	}
	s.Log("info", "[TRANSCRIPT] Stream ended", "uuid", uuid)
	return nil
}

// scriptAIIVRNotify is ivr_ai.lua / ivr_ai3.lua: <caller uuid> <domain>; once the call is up the
// event tells node/server.js a caller is on the AI IVR.
func scriptAIIVRNotify(s *Script, subclass string) error {
	if s.Arg(1) == "" || !s.Ready() {
		return nil
	}
	e := NewEvent(EvCustom, subclass)
	e.Add("Recording-Filename", "")
	e.Add("Caller-uuid", s.Arg(1))
	e.Add("Caller-subdomain", s.Arg(2))
	s.Core.events.Publish(e)
	s.Log("info", "ai ivr call", "uuid", s.Arg(1), "event", subclass)
	return nil
}

// transfer_ai_call.lua <uuid> <extension>: moves an AI call to a person. After 3 s (for the AI to
// finish its sentence) the AI stream stops, and half a second later the call goes to the
// extension in its own domain.
func scriptTransferAICall(s *Script) error {
	uuid, target := strings.TrimSpace(s.Arg(1)), strings.TrimSpace(s.Arg(2))
	if uuid == "" || target == "" {
		s.Log("err", "[transfer_call] usage: transfer_ai_call.lua <uuid> <extension>")
		return nil
	}
	c := s.Core
	domain := c.API("uuid_getvar " + uuid + " domain_name")
	domain = strings.TrimSpace(domain)
	if domain == "" || domain == "_undef_" || strings.HasPrefix(domain, "-ERR") {
		domain = strings.TrimSpace(c.API("global_getvar domain"))
		if domain == "" || strings.HasPrefix(domain, "-ERR") {
			domain = "default"
		}
	}
	s.Log("info", fmt.Sprintf("[transfer_call] Initiating transfer for UUID=%s to queue=%s (domain %s)", uuid, target, domain))

	if !sleepFor(s.Ctx, 3*time.Second) {
		return nil
	}
	// stop any active audio streams (mod_openai_audio_stream)
	c.API("uuid_openai_audio_stream " + uuid + " stop")
	if !sleepFor(s.Ctx, 500*time.Millisecond) {
		return nil
	}
	res := strings.TrimSpace(c.API(fmt.Sprintf("uuid_transfer %s %s XML %s", uuid, target, domain)))
	s.Log("info", fmt.Sprintf("[transfer_call] Transfer result for %s -> %s@%s: %s", uuid, target, domain, res))
	if res == "" || res == "false" || strings.HasPrefix(res, "-ERR") {
		s.Log("err", "[transfer_call] Transfer failed!", "uuid", uuid)
	}
	return nil
}

// sleepFor waits d; false when ctx ended first.
func sleepFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
