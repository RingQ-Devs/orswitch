package core

import (
	"os"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Shared pieces of the call-feature scripts: the Go versions of the FusionPBX
// Lua scripts follow_me, call_flow, call_divert, park, page, eavesdrop...
// (script_followme.go, script_callflow.go, script_park.go, script_spy.go,
// script_page.go, script_misc.go, script_fifo.go).

// callFeatState is what those scripts keep between calls.
type callFeatState struct {
	mu    sync.Mutex
	lamps map[string]lampState // resource@domain (lower case): BLF lamps of call flows and the like
	lots  map[string]*parkEntry
	fifos map[string]*fifoQueue
	svc   map[string]context.CancelFunc // running service scripts (call_flow_monitor)
}

// ---------------------------------------------------------------------------
// lamps (presence_in.turn_lamp)

type lampState struct {
	id    string
	on    bool
	since time.Time
}

// TurnLamp lights or clears the BLF lamp of a resource that is not a call
// (call flows *7x, "flow+*74"): the Go version of presence_in.turn_lamp. The
// phones watching the resource (SUBSCRIBE dialog) are told, and a PRESENCE_IN
// event is published as FreeSWITCH did.
func (c *Core) TurnLamp(resource, domain string, on bool, id string) {
	resource = strings.TrimSpace(resource)
	if resource == "" || domain == "" {
		return
	}
	if id == "" {
		id = c.API("create_uuid")
		id = strings.TrimSpace(id)
	}
	key := strings.ToLower(resource + "@" + domain)
	c.callfeat.mu.Lock()
	if c.callfeat.lamps == nil {
		c.callfeat.lamps = map[string]lampState{}
	}
	old, had := c.callfeat.lamps[key]
	if !had || old.on != on || old.id != id {
		c.callfeat.lamps[key] = lampState{id: id, on: on, since: time.Now()}
	}
	c.callfeat.mu.Unlock()

	// the event, as presence_in.lua fired it: proto "sip" unless the resource has a "proto+" prefix
	proto, user := "sip", resource
	if p, u, ok := strings.Cut(resource, "+"); ok {
		proto, user = p, u
	}
	e := NewEvent(EvPresenceIn, "")
	e.Add("proto", proto)
	e.Add("event_type", "presence")
	e.Add("alt_event_type", "dialog")
	e.Add("Presence-Call-Direction", "outbound")
	e.Add("from", user+"@"+domain)
	e.Add("login", user+"@"+domain)
	e.Add("unique-id", id)
	e.Add("status", "Active (1 waiting)")
	if on {
		e.Add("answer-state", "confirmed")
		e.Add("rpid", "unknown")
		e.Add("event_count", "1")
	} else {
		e.Add("answer-state", "terminated")
	}
	c.events.Publish(e)
	c.presenceKick()
}

// LampOn is the state of a lamp set with TurnLamp.
func (c *Core) LampOn(resource, domain string) bool {
	c.callfeat.mu.Lock()
	defer c.callfeat.mu.Unlock()
	return c.callfeat.lamps[strings.ToLower(resource+"@"+domain)].on
}

// addLamps adds the lit lamps to the BLF states (computeBLF).
func (c *Core) addLamps(add func(res, domain string, d blfDialog)) {
	c.callfeat.mu.Lock()
	defer c.callfeat.mu.Unlock()
	for key, l := range c.callfeat.lamps {
		if !l.on {
			continue
		}
		res, domain, _ := strings.Cut(key, "@")
		add(res, domain, blfDialog{id: l.id, state: "confirmed", direction: "recipient", since: l.since})
	}
}

// onDialogSubscribe runs when a phone starts watching a BLF resource: the call
// flow keys ("flow+*74") get their lamp from the database (blf_subscribe.lua).
func (c *Core) onDialogSubscribe(resource, domain string) {
	if !strings.HasPrefix(strings.ToLower(resource), "flow+") || !hasScript("call_flow_subscribe") {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _, _ = c.runScript(ctx, nil, "call_flow_subscribe", []string{"", resource + "@" + domain, "3600"})
	}()
}

// ---------------------------------------------------------------------------
// script helpers

// Rows runs a SELECT for the script ("?" placeholders are $1, $2: pass them as Postgres does).
func (s *Script) Rows(query string, args ...any) []map[string]string {
	rows, err := s.Core.sqlRows(s.Ctx, query, args...)
	if err != nil {
		s.Log("warn", "database read failed", "err", err)
		return nil
	}
	return rows
}

// Row is the first row of a SELECT (nil when there is none).
func (s *Script) Row(query string, args ...any) map[string]string {
	if rows := s.Rows(query, args...); len(rows) > 0 {
		return rows[0]
	}
	return nil
}

// Exec runs an UPDATE, INSERT or DELETE; false when it failed.
func (s *Script) Exec(query string, args ...any) bool {
	if _, err := s.Core.sqlExec(s.Ctx, query, args...); err != nil {
		s.Log("warn", "database write failed", "err", err)
		return false
	}
	return true
}

// Extension settings as FreeSWITCH's user_data gives them ("" when unknown).
func (c *Core) extData(ext, domain, name string) string {
	if v, ok := c.userOverride(ext, domain, name); ok {
		return v
	}
	u := c.config().Directory.LookupIn(domain, ext)
	if u == nil {
		return ""
	}
	switch name {
	case "id":
		return u.ID
	}
	return u.Vars[name]
}

// userExists is FreeSWITCH's user_exists id <ext> <domain> (aliases count).
func (c *Core) userExists(ext, domain string) bool {
	return ext != "" && c.config().Directory.LookupIn(domain, ext) != nil
}

// userID gives the extension of an id or number alias (user_data <id>@<domain> attr id); the id itself when unknown.
func (c *Core) userID(ext, domain string) string {
	if u := c.config().Directory.LookupIn(domain, ext); u != nil {
		return u.ID
	}
	return ext
}

// registered is true when the extension has a registered phone (sofia_contact).
func (c *Core) registered(ext, domain string) bool {
	return len(c.reg.Lookup(ext, domain)) > 0
}

// ---------------------------------------------------------------------------
// sounds: the standard FreeSWITCH prompts when they are installed, the
// switch's own prompts or a tone when they are not

type fsSound struct {
	file    string // the standard sounds file (relative, as the Lua scripts name it)
	builtin string // the switch's own prompt that says the same ("" none)
	tone    string // what to play when neither exists
}

const (
	toneOK     = "tone_stream://%(150,100,880);%(150,100,1320)"
	toneFail   = "tone_stream://%(250,250,480,620);loops=3"
	tonePrompt = "tone_stream://%(200,100,800);%(200,100,1000)"
)

var fsSounds = map[string]fsSound{
	"phrase:voicemail_enter_pass":                {"voicemail/vm-enter_pass.wav", "pin-enter", tonePrompt},
	"phrase:voicemail_enter_id":                  {"voicemail/vm-enter_id.wav", "pin-enter", tonePrompt},
	"phrase:voicemail_fail_auth":                 {"voicemail/vm-fail_auth.wav", "pin-invalid", toneFail},
	"phrase:voicemail_record_name":               {"voicemail/vm-record_name1.wav", "", tonePrompt},
	"phrase:agent_status":                        {"ivr/ivr-agent_status.wav", "", tonePrompt},
	"ivr/ivr-please_enter_pin_followed_by_pound.wav": {"ivr/ivr-please_enter_pin_followed_by_pound.wav", "pin-enter", tonePrompt},
	"ivr/ivr-enter_destination_telephone_number.wav": {"ivr/ivr-enter_destination_telephone_number.wav", "", tonePrompt},
	"ivr/ivr-accept_reject_voicemail.wav":        {"ivr/ivr-accept_reject_voicemail.wav", "", tonePrompt},
	"ivr/ivr-call_forwarding_has_been_set.wav":   {"ivr/ivr-call_forwarding_has_been_set.wav", "", toneOK},
	"ivr/ivr-call_forwarding_has_been_cancelled.wav": {"ivr/ivr-call_forwarding_has_been_cancelled.wav", "dnd-off", toneOK},
	"ivr/ivr-you_are_now_logged_in.wav":          {"ivr/ivr-you_are_now_logged_in.wav", "", toneOK},
	"ivr/ivr-you_are_now_logged_out.wav":         {"ivr/ivr-you_are_now_logged_out.wav", "", toneOK},
	"voicemail/vm-fail_auth.wav":                 {"voicemail/vm-fail_auth.wav", "pin-invalid", toneFail},
	"voicemail/vm-saved.wav":                     {"voicemail/vm-saved.wav", "vm-saved", toneOK},
	"voicemail/vm-deleted.wav":                   {"voicemail/vm-deleted.wav", "vm-deleted", toneOK},
	"misc/you_have_a_call.wav":                   {"misc/you_have_a_call.wav", "", tonePrompt},
	"misc/parkslot.wav":                          {"misc/parkslot.wav", "", toneOK},
}

// Sound is the file (or tone) to play for a FreeSWITCH sound or phrase name:
// "phrase:name[:args]", a file of the standard sounds, a recording of the
// domain, an absolute path, or a tone_stream. It never gives "".
func (s *Script) Sound(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "silence_stream://0"
	}
	key := name
	if strings.HasPrefix(key, "phrase:") {
		parts := strings.SplitN(key, ":", 3)
		key = parts[0] + ":" + parts[1]
	}
	if s.Ch != nil {
		if fs, ok := fsSounds[key]; ok {
			if p := s.Core.resolveSound(s.Ch, fs.file); p != "" {
				return p
			}
			if fs.builtin != "" {
				if p := s.Core.prompt(s.Ch, fs.builtin); p != "" {
					return p
				}
			}
			return fs.tone
		}
		if strings.HasPrefix(key, "phrase:") {
			return tonePrompt
		}
		if strings.HasPrefix(name, "digits/") {
			// digits/5.wav: the switch's own number prompts
			if n := strings.TrimSuffix(strings.TrimPrefix(name, "digits/"), ".wav"); n != "" {
				if p := s.Core.resolveSound(s.Ch, name); p != "" {
					return p
				}
				if p := s.Core.prompt(s.Ch, "num-"+n); p != "" {
					return p
				}
			}
		}
		if p := s.Core.resolveSound(s.Ch, name); p != "" {
			return p
		}
	}
	if playable(name) {
		return name
	}
	return tonePrompt // no such file: a tone, so the caller still hears something
}

// PlaySound plays a sound (see Sound) to the call; it stops when the call ends.
func (s *Script) PlaySound(name string) {
	if s.Ch == nil || !s.Ready() {
		return
	}
	if err := s.Execute("playback", s.Sound(name)); err != nil {
		s.Log("debug", "playback failed", "sound", name, "err", err)
	}
}

// GetDigits is session:playAndGetDigits(min, max, tries, timeout, "#", prompt, "", regex): the digits, "" when none.
func (s *Script) GetDigits(min, max, tries, timeoutMs int, prompt, regex string) string {
	const v = "script_digits"
	s.SetVar(v, "")
	if regex == "" {
		regex = `\d+`
	}
	data := fmt.Sprintf("%d %d %d %d # %s silence_stream://0 %s %s", min, max, tries, timeoutMs, s.Sound(prompt), v, regex)
	if err := s.Execute("play_and_get_digits", data); err != nil {
		s.Log("debug", "digits not read", "err", err)
		return ""
	}
	return s.Var(v)
}

// CheckPIN asks for a PIN (any of the comma separated ones) the way the Lua
// scripts did (min = len, max = len + 1 digits); when it is wrong it plays the
// failure and hangs up. It returns the PIN that was used.
func (s *Script) CheckPIN(pins, prompt string, maxLen int) (string, bool) {
	list := splitList(pins, ",")
	if len(list) == 0 {
		return "", true
	}
	minD, maxD := 2, 20
	if maxLen > 0 {
		minD, maxD = maxLen, maxLen+1
	}
	digits := s.GetDigits(minD, maxD, 3, 5000, prompt, `\d+`)
	for _, p := range list {
		if digits != "" && digits == p {
			return p, true
		}
	}
	s.PlaySound("phrase:voicemail_fail_auth:#")
	s.Hangup("NORMAL_CLEARING")
	return "", false
}

func splitList(v, sep string) []string {
	var out []string
	for _, p := range strings.Split(v, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// chanSafe keeps a value usable inside a dial string or a variable.
func chanSafe(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ',', '{', '}', '[', ']', '\'', '"', '\\', '\n', '\r', '|':
			return -1
		}
		return r
	}, v)
}

// optNum is a number out of a variable ("" or bad: def).
func optNum(v string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return n
	}
	return def
}

// subScript runs another registered script on the same call (a script that
// FusionPBX wrote as a wrapper of another one).
func (s *Script) subScript(name string, args ...string) error {
	_, found, err := s.Core.runScript(s.Ctx, s.Ch, name, args)
	if !found {
		return fmt.Errorf("no such script %s", name)
	}
	return err
}

// playable: a tone, silence or a file that exists.
func playable(spec string) bool {
	if strings.HasPrefix(spec, "tone_stream://") || strings.HasPrefix(spec, "silence_stream://") {
		return true
	}
	st, err := os.Stat(spec)
	return err == nil && !st.IsDir()
}
var builtinAppsExtra = map[string]App{}
