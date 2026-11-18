package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The remaining top-level FusionPBX / RingQ Lua scripts:
//
//	callcenter-announce-position.lua  tells a queued caller their place, every N ms
//	callcenter-announce-messages.lua  plays the queue's hold messages in turn, every N ms
//	email.lua                         mail from the dialplan (queued like all FusionPBX mail)
//	roku.lua                          remote control of a Roku from a phone
//	handler.lua                       starts a call's audio stream to a bridge server
//	ha_monitor.lua                    passes cache flushes and gateway changes to the other servers

func init() {
	RegisterScript("callcenter-announce-position", scriptCCAnnouncePosition)
	RegisterScript("callcenter-announce-messages", scriptCCAnnounceMessages)
	RegisterScript("email", scriptEmail)
	RegisterScript("roku", scriptRoku)
	RegisterScript("handler", scriptHandler)
	RegisterScript("ha_monitor", scriptHAMonitor)
}

// ccAnnounceSleep waits ms milliseconds (freeswitch.msleep); false when the switch is stopping.
func ccAnnounceSleep(s *Script, ms int) bool {
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.Core.stop:
		return false
	case <-s.Ctx.Done():
		return false
	}
}

// ccQueueLines are the Waiting/Trying members of a queue (callcenter_config queue list members).
func ccQueueLines(s *Script, queue string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(s.Core.API("callcenter_config queue list members "+queue), func(r rune) bool { return r == '\n' || r == '\r' }) {
		if strings.Contains(line, "Trying") || strings.Contains(line, "Waiting") {
			out = append(out, line)
		}
	}
	return out
}

// callcenter-announce-position.lua <uuid> <queue> <ms>: every <ms> the caller hears "you are
// number N in the queue" for as long as they are Waiting or Trying in the queue.
func scriptCCAnnouncePosition(s *Script) error {
	uuid, queue, msArg := s.Arg(1), s.Arg(2), s.Arg(3)
	ms, err := strconv.Atoi(msArg)
	if uuid == "" || queue == "" || err != nil {
		return nil
	}
	for ccAnnounceSleep(s, ms) {
		pos, exists := 1, false
		for _, line := range ccQueueLines(s, queue) {
			if strings.Contains(line, uuid) {
				exists = true
				s.Core.API("uuid_broadcast " + uuid + " ivr/ivr-you_are_number.wav aleg")
				s.Core.API("uuid_broadcast " + uuid + " digits/" + strconv.Itoa(pos) + ".wav aleg")
				s.Core.API("uuid_broadcast " + uuid + " ivr/ivr-in-the-queue.wav aleg")
			}
			pos++
		}
		if !exists {
			return nil
		}
	}
	return nil
}

// callcenter-announce-messages.lua <uuid> <queue> <ms> [announce1 [announce2 [announce3]]]: the
// first announcement once, then announce2, announce3, announce1 ... in turn, every <ms>.
// The files are in the RingQ audio folder (/var/www/ringq/public/audio, or the global variable
// ringq_audio_dir).
func scriptCCAnnounceMessages(s *Script) error {
	uuid, queue := s.Arg(1), s.Arg(2)
	ms, err := strconv.Atoi(s.Arg(3))
	if uuid == "" || queue == "" || err != nil {
		return nil
	}
	a1, a2, a3 := s.Arg(4), s.Arg(5), s.Arg(6)
	dir := strings.TrimRight(firstNonEmpty(s.Core.globalVar("ringq_audio_dir"), "/var/www/ringq/public/audio"), "/")
	playedFirst := false
	loop := 0
	for ccAnnounceSleep(s, ms) {
		exists := false
		for _, line := range ccQueueLines(s, queue) {
			if !strings.Contains(line, uuid) {
				continue
			}
			exists = true
			if !playedFirst && a1 != "" {
				s.Core.API("uuid_broadcast " + uuid + " " + dir + "/" + a1 + " aleg")
				playedFirst = true
				continue
			}
			next := ""
			switch {
			case loop%3 == 0 && a2 != "":
				next = a2
			case loop%3 == 1 && a3 != "":
				next = a3
			case loop%3 == 2 && a1 != "":
				next = a1
			}
			if next != "" {
				s.Core.API("uuid_broadcast " + uuid + " " + dir + "/" + next + " aleg")
			}
			loop++
		}
		if !exists {
			return nil
		}
	}
	return nil
}

// email.lua <to> <from> <headers> <subject> <body> [file] [delete]: queues a mail. The body's
// &#39; and &#34; entities are turned back into quotes; a call adds its domain and uuid headers.
func scriptEmail(s *Script) error {
	to, from, subject, body, file := s.Arg(1), s.Arg(2), s.Arg(4), s.Arg(5), s.Arg(6)
	body = strings.ReplaceAll(strings.ReplaceAll(body, "&#39;", "'"), "&#34;", `"`)
	m := EmailMessage{To: to, From: from, Subject: subject, Body: body, Attachment: file, Type: "app"}
	if s.Ready() {
		m.DomainUUID = s.Var("domain_uuid")
		m.CallUUID = s.UUID()
	}
	if err := s.Core.QueueEmail(s.Ctx, m); err != nil {
		s.Log("warn", "email not queued", "to", to, "err", err)
		return err
	}
	return nil
}

// rokuKeys: what the digits press on the Roku (External Control Protocol key names).
var rokuKeys = map[rune]string{'1': "Back", '2': "Up", '3': "Home", '4': "Left", '5': "Select", '6': "Right",
	'7': "Rev", '8': "Down", '9': "Fwd", '0': "Play", '#': "InstantReplay"}

// roku.lua: a phone controls a Roku. With pin_number (comma separated PINs) the caller enters
// one; then every digit pressed is a key of the Roku at the call's "host" (port 8080 as in the Lua;
// roku_port overrides it). The Lua ran FusionPBX's roku.php for each digit; this sends the key to
// the Roku's External Control Protocol (POST /keypress/<key>), so the digit-to-key map above is
// the one assumption. * ends the call; so does 17500 idle rounds of 40 s.
func scriptRoku(s *Script) error {
	if !s.Ready() {
		return nil
	}
	if err := s.Answer(); err != nil {
		return err
	}
	pins := s.Var("pin_number")
	host := s.Var("host")
	port := disaDefault(s.Var("roku_port"), "8080")
	if pins != "" {
		digits := s.PlayAndGetDigits(2, 20, 3, 5000*time.Millisecond, "#", "phrase:voicemail_enter_pass:#", "", `\d+`)
		ok := false
		for _, p := range strings.Split(pins, ",") {
			if digits != "" && digits == strings.TrimSpace(p) {
				ok = true
				s.SetVar("pin_number", strings.TrimSpace(p))
				break
			}
		}
		if !ok {
			s.Stream("phrase:voicemail_fail_auth:#")
			s.Hangup("NORMAL_CLEARING")
			return nil
		}
	}
	if !s.Ready() {
		return nil
	}
	digits := s.PlayAndGetDigits(1, 1, 3, 5000*time.Millisecond, "#", "ivr/ivr-enter_destination_telephone_number.wav", "", `\d+`)
	for x := 0; s.Ready(); x++ {
		if digits == "" {
			digits = s.GetDigitsLoose(1, "#", 40*time.Second)
		}
		if digits == "*" {
			break
		}
		if digits != "" {
			if key, ok := rokuKeys[rune(digits[0])]; ok && host != "" {
				if err := rokuKey(s.Ctx, host, port, key); err != nil {
					s.Log("warn", "roku key failed", "host", host, "key", key, "err", err)
				}
			}
		}
		digits = ""
		if x > 17500 {
			break
		}
	}
	s.Hangup("NORMAL_CLEARING")
	return nil
}

func rokuKey(ctx context.Context, host, port, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+":"+port+"/keypress/"+key, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// handler.lua: answers, tells the stream bridge who is calling (STREAM_META_CALLER, STREAM_META_DID),
// starts "uuid_audio_stream <uuid> start <bridge url> mono 8k" and holds the call until it ends.
// The bridge URL is the call variable stream_bridge_url (the Lua had it in the file).
func scriptHandler(s *Script) error {
	if !s.Ready() {
		return nil
	}
	caller := disaDefault(s.Var("caller_id_number"), "unknown")
	did := disaDefault(s.Var("destination_number"), "unknown")
	bridge := disaDefault(s.Var("stream_bridge_url"), "wss://your-server.example.com:8000/fs")
	if err := s.Answer(); err != nil {
		return err
	}
	s.SetVar("STREAM_META_CALLER", caller)
	s.SetVar("STREAM_META_DID", did)
	s.Log("info", "starting stream", "caller", caller, "did", did)
	res := s.Core.API("uuid_audio_stream " + s.UUID() + " start " + bridge + " mono 8k")
	s.Log("info", "uuid_audio_stream", "result", strings.TrimSpace(res))
	for s.Ready() {
		s.Sleep(1000)
	}
	s.Log("info", "call ended", "caller", caller)
	return nil
}

// ---------------------------------------------------------------------------
// ha_monitor

// HAServer is one peer of the HA monitor: the global variable ha_monitor_servers is a JSON list
// of {"method":"curl"|"ssh","username":..,"password":..,"hostname":..,"port":..}.
type HAServer struct {
	Method   string `json:"method"`
	Username string `json:"username"`
	Password string `json:"password"`
	Hostname string `json:"hostname"`
	Port     string `json:"port"`
}

// haURLEncode is the monitor's urlencode: every character but letters and digits as %XX.
func haURLEncode(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// haRemote runs "command args" on a peer: by its web API (curl) or over ssh (fs_cli).
func haRemote(ctx context.Context, log func(level, msg string, kv ...any), srv HAServer, command, args string) {
	method := disaDefault(srv.Method, "curl")
	switch method {
	case "curl":
		u := "http://"
		if srv.Username != "" {
			u += url.PathEscape(srv.Username) + ":" + url.PathEscape(srv.Password) + "@"
		}
		u += srv.Hostname
		if srv.Port != "" {
			u += ":" + srv.Port
		}
		u += "/webapi/" + command
		if args != "" {
			u += "?" + haURLEncode(args)
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
		if err != nil {
			log("warn", "ha: bad url", "err", err)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log("warn", "ha: curl failed", "host", srv.Hostname, "command", command, "err", err)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log("warn", "ha: curl error", "host", srv.Hostname, "status", resp.StatusCode)
			return
		}
		log("info", "ha: curl pass", "host", srv.Hostname, "command", command, "args", args, "response", strings.TrimSpace(string(body)))
	case "ssh":
		remote := "fs_cli -x '" + strings.ReplaceAll(command+" "+args, "'", `'\''`) + "'"
		argv := []string{"-o", "BatchMode=yes"}
		if srv.Port != "" {
			argv = append(argv, "-p", srv.Port)
		}
		argv = append(argv, srv.Username+"@"+srv.Hostname, remote)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := exec.CommandContext(cctx, "ssh", argv...).Run(); err != nil {
			log("warn", "ha: ssh failed", "host", srv.Hostname, "err", err)
			return
		}
		log("info", "ha: ssh pass", "host", srv.Hostname, "command", command, "args", args)
	default:
		log("warn", "ha: unknown execute method", "method", method)
	}
}

// ha_monitor.lua (run it with luarun ha_monitor.lua): a service that keeps the other servers'
// caches and gateways like this one: a "memcache flush" or "memcache delete <key>" here is sent
// to every peer, and so are added and deleted gateways ("sofia profile <p> start|killgw <gw>").
func scriptHAMonitor(s *Script) error {
	c := s.Core
	var servers []HAServer
	if v := c.globalVar("ha_monitor_servers"); v != "" {
		if err := json.Unmarshal([]byte(v), &servers); err != nil {
			s.Log("err", "ha_monitor_servers is not a JSON list", "err", err)
			return err
		}
	}
	all := func(command, args string) {
		for _, srv := range servers {
			haRemote(s.Ctx, s.Log, srv, command, args)
		}
	}
	sub := c.events.Subscribe(256, func(e *Event) bool {
		return e.Name == "MEMCACHE" || e.Name == EvCustom && (e.Subclass() == "fusion::memcache" || e.Subclass() == "sofia::gateway_add" || e.Subclass() == "sofia::gateway_delete")
	})
	defer sub.Close()
	s.Log("info", "ha_monitor started", "servers", len(servers))
	for {
		select {
		case e := <-sub.C:
			switch {
			case e.Name == "MEMCACHE" || e.Subclass() == "fusion::memcache":
				if strings.TrimSpace(e.Get("API-Command")) != "memcache" {
					continue
				}
				arg := strings.TrimSpace(e.Get("API-Command-Argument"))
				if arg == "flush" || strings.HasPrefix(arg, "delete") {
					all("memcache", arg)
				}
			case e.Subclass() == "sofia::gateway_add":
				all("sofia", "profile "+e.Get("profile-name")+" start "+e.Get("Gateway"))
			case e.Subclass() == "sofia::gateway_delete":
				all("sofia", "profile "+e.Get("profile-name")+" killgw "+e.Get("Gateway"))
			}
		case <-c.stop:
			s.Log("info", "ha_monitor stopped")
			return nil
		case <-s.Ctx.Done():
			return nil
		}
	}
}
