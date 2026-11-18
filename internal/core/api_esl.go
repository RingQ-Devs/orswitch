package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ESL commands that the RingQ backend (ringq_core) sends and that were not
// available before: scheduled actions (sched_api, sched_transfer,
// sched_hangup, sched_del), uuid_break, uuid_audio, reloadacl and the json
// wrapper used for callcenter_config.

func init() {
	for name, cmd := range map[string]apiCommand{
		"sched_api":      {"sched_api [+]<time> <group> <command> [args]", "run an API command later", apiSchedAPI},
		"sched_transfer": {"sched_transfer [+]<time> <uuid> <extension> [<dialplan>] [<context>]", "transfer a channel later", apiSchedTransfer},
		"sched_hangup":   {"sched_hangup [+]<time> <uuid> [<cause>]", "hang up a channel later", apiSchedHangup},
		"sched_del":      {"sched_del <group|task id>", "cancel scheduled tasks", apiSchedDel},
		"uuid_break":     {"uuid_break <uuid> [all]", "stop the media (playback) a channel is hearing", apiUUIDBreak},
		"uuid_audio":     {"uuid_audio <uuid> start|stop [read|write] [mute|level] <n>", "adjust call audio (accepted; no effect yet)", apiUUIDAudio},
		"reloadacl":      {"reloadacl", "reload access lists", apiReloadACL},
		"uuid_buglist":   {"uuid_buglist <uuid>", "list the recordings (media bugs) of a channel as XML", apiUUIDBugList},
		"json":           {`json {"command":"<api command>","data":{"arguments":"<args>"}}`, "run an API command and reply as JSON", apiJSON},
	} {
		apiCommands[name] = cmd
	}
}

// ---------------------------------------------------------------------------
// scheduler

type schedTask struct {
	id    int
	group string
	timer *time.Timer
}

// scheduler keeps the tasks sched_* created, so sched_del can cancel them by
// group (usually a channel uuid) or by task id. The zero value is ready to use.
type scheduler struct {
	mu    sync.Mutex
	seq   int
	tasks map[int]*schedTask
}

func (s *scheduler) add(group string, d time.Duration, fn func()) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = map[int]*schedTask{}
	}
	s.seq++
	t := &schedTask{id: s.seq, group: group}
	id := t.id
	t.timer = time.AfterFunc(d, func() {
		s.mu.Lock()
		delete(s.tasks, id)
		s.mu.Unlock()
		fn()
	})
	s.tasks[id] = t
	return id
}

// del cancels every task whose group equals key, or whose id equals key.
func (s *scheduler) del(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, numeric := strconv.Atoi(key)
	removed := 0
	for id, t := range s.tasks {
		if t.group == key || (numeric == nil && id == n) {
			if t.timer.Stop() {
				removed++
			}
			delete(s.tasks, id)
		}
	}
	return removed
}

func (s *scheduler) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tasks)
}

func schedTime(arg string) (time.Duration, error) {
	d, ok := schedDelay(arg)
	if !ok {
		return 0, fmt.Errorf("bad time %q", arg)
	}
	if d < 0 {
		d = 0
	}
	return d, nil
}

func apiSchedAPI(c *Core, args []string) (string, error) {
	if len(args) < 3 {
		return "", fmt.Errorf("usage: %s", apiCommands["sched_api"].usage)
	}
	d, err := schedTime(args[0])
	if err != nil {
		return "", err
	}
	group, line := args[1], joinArgs(args[2:])
	id := c.sched.add(group, d, func() { c.API(line) })
	return fmt.Sprintf("+OK Added task %d\n", id), nil
}

func apiSchedTransfer(c *Core, args []string) (string, error) {
	if len(args) < 3 {
		return "", fmt.Errorf("usage: %s", apiCommands["sched_transfer"].usage)
	}
	d, err := schedTime(args[0])
	if err != nil {
		return "", err
	}
	uuid, dest, ctxName := args[1], args[2], ""
	switch {
	case len(args) > 4:
		ctxName = args[4]
	case len(args) == 4:
		ctxName = args[3]
	}
	c.sched.add(uuid, d, func() {
		if ch := c.Channel(uuid); ch != nil {
			c.transfer(ch, dest, ctxName)
		}
	})
	return "+OK\n", nil
}

func apiSchedHangup(c *Core, args []string) (string, error) {
	if len(args) < 2 {
		return "", fmt.Errorf("usage: %s", apiCommands["sched_hangup"].usage)
	}
	d, err := schedTime(args[0])
	if err != nil {
		return "", err
	}
	uuid, cause := args[1], CauseAllottedTimeout
	if len(args) > 2 {
		cause = ParseCause(args[2])
	}
	c.sched.add(uuid, d, func() {
		if ch := c.Channel(uuid); ch != nil {
			ch.Hangup(cause)
		}
	})
	return "+OK\n", nil
}

func apiSchedDel(c *Core, args []string) (string, error) {
	if len(args) < 1 {
		return "", fmt.Errorf("usage: %s", apiCommands["sched_del"].usage)
	}
	return fmt.Sprintf("+OK Deleted: %d\n", c.sched.del(args[0])), nil
}

// ---------------------------------------------------------------------------
// uuid_break, uuid_audio, reloadacl

// mediaApps are the applications whose only job is to play something to the
// caller; breaking them lets the dialplan carry on with the next action.
var mediaApps = map[string]bool{
	"playback": true, "endless_playback": true, "gentones": true,
	"play_and_get_digits": true, "read": true, "play_and_detect_speech": true,
}

func apiUUIDBreak(c *Core, args []string) (string, error) {
	ch, err := c.mustChannel(args, 1, apiCommands["uuid_break"].usage)
	if err != nil {
		return "", err
	}
	ch.breakMedia()
	return "+OK\n", nil
}

// breakMedia stops a file started by uuid_broadcast and, when the channel's
// dialplan is only playing something, that playback. A bridge is left alone.
func (ch *Channel) breakMedia() {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.bcastCancel != nil {
		ch.bcastCancel()
		ch.bcastCancel = nil
	}
	if mediaApps[ch.app] && ch.appCancel != nil {
		ch.appCancel()
	}
}

// uuid_audio is accepted so the attended-transfer "mute the held party" calls
// succeed. Media is relayed without decoding, so a level change is not applied;
// the request is recorded on the channel for diagnosis.
func apiUUIDAudio(c *Core, args []string) (string, error) {
	ch, err := c.mustChannel(args, 2, apiCommands["uuid_audio"].usage)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(args[1]) {
	case "start":
		ch.SetVar("rq_uuid_audio", strings.Join(args[2:], " "))
	case "stop":
		ch.SetVar("rq_uuid_audio", "")
	default:
		return "", fmt.Errorf("usage: %s", apiCommands["uuid_audio"].usage)
	}
	return "+OK Success\n", nil
}

// apiUUIDBugList reports the call recording of a channel in FreeSWITCH's
// media bug XML (function session_record, target = the file), which the
// RingQ backend reads to pause and resume recordings.
func apiUUIDBugList(c *Core, args []string) (string, error) {
	ch, err := c.mustChannel(args, 1, apiCommands["uuid_buglist"].usage)
	if err != nil {
		return "", err
	}
	ch.mu.Lock()
	rec := ch.rec
	ch.mu.Unlock()
	if rec == nil {
		return "<media-bugs>\n</media-bugs>\n", nil
	}
	return "<media-bugs>\n  <media-bug>\n    <function>session_record</function>\n    <target>" +
		xmlText(rec.path) + "</target>\n  </media-bug>\n</media-bugs>\n", nil
}

func apiReloadACL(c *Core, _ []string) (string, error) {
	c.requestReload()
	return "+OK acl reloaded\n", nil
}

// ---------------------------------------------------------------------------
// json

// apiJSON runs {"command": "...", "data": {"arguments": "..."}} like the json
// API of FreeSWITCH: list output becomes an array of row objects.
func apiJSON(c *Core, args []string) (string, error) {
	var req struct {
		Command string         `json:"command"`
		Data    map[string]any `json:"data"`
		Format  string         `json:"format"`
	}
	if err := json.Unmarshal([]byte(strings.Join(args, " ")), &req); err != nil || req.Command == "" {
		return jsonError("invalid json request"), nil
	}
	if strings.EqualFold(req.Command, "json") {
		return jsonError("json cannot call itself"), nil
	}
	arguments, _ := req.Data["arguments"].(string)
	// callcenter_config takes the queue as a separate field
	// ({"arguments":"queue list agents","queue_name":"5000@domain"}).
	if q, _ := req.Data["queue_name"].(string); q != "" && strings.EqualFold(req.Command, "callcenter_config") && !strings.Contains(arguments, q) {
		arguments = strings.TrimSpace(arguments + " " + q)
	}
	out := c.API(strings.TrimSpace(req.Command + " " + arguments))
	if req.Data == nil {
		req.Data = map[string]any{}
	}
	// FreeSWITCH echoes the request's data object, and callers read fields back from it.
	resp := map[string]any{"command": req.Command, "data": req.Data}
	if strings.HasPrefix(out, "-ERR") {
		resp["status"] = "error"
		resp["message"] = strings.TrimSpace(strings.TrimPrefix(out, "-ERR"))
	} else {
		resp["status"] = "success"
		resp["response"] = tableToRows(out)
	}
	var b []byte
	if req.Format == "pretty" {
		b, _ = json.MarshalIndent(resp, "", "  ")
	} else {
		b, _ = json.Marshal(resp)
	}
	return string(b) + "\n", nil
}

func jsonError(msg string) string {
	b, _ := json.Marshal(map[string]string{"status": "error", "message": msg})
	return string(b) + "\n"
}

// tableToRows turns "col1|col2\nv1|v2\n+OK" into [{"col1":"v1","col2":"v2"}].
// Output that is not a table is returned as one trimmed string.
func tableToRows(out string) any {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "+OK") {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return []map[string]string{}
	}
	if !strings.Contains(lines[0], "|") {
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}
	cols := strings.Split(lines[0], "|")
	rows := make([]map[string]string, 0, len(lines)-1)
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		f := strings.Split(l, "|")
		row := make(map[string]string, len(cols))
		for i, col := range cols {
			if i < len(f) {
				row[col] = f[i]
			} else {
				row[col] = ""
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// broadcastCtx returns a context that uuid_break can cancel.
func (ch *Channel) broadcastCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ch.ctx)
	ch.mu.Lock()
	ch.bcastCancel = cancel
	ch.mu.Unlock()
	return ctx, cancel
}

// joinArgs puts a command line back together after splitArgs took the quotes off arguments that
// had spaces ('On Break'): they are quoted again, so the command it later runs reads the same.
func joinArgs(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t") && !strings.ContainsAny(a[:1], "{[&'\"") && !strings.ContainsAny(a, "'") {
			a = "'" + a + "'"
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}
