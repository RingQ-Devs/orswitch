package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RingQ scripts: the replacement of the FreeSWITCH Lua scripts of the RingQ
// backend. Instead of an embedded Lua interpreter (the scripts lean on FusionPBX
// libraries and on shelling out to redis-cli and curl), each script is a Go
// function that is registered under the name it had, so the dialplans, the
// backend's `luarun` calls and `${lua(script args)}` keep working unchanged:
//
//	<action application="lua" data="ringq_call_control.lua failover"/>
//	<action application="set" data="rq_cc=${lua(ringq_call_control.lua resolve ${uuid})}" inline="true"/>
//	luarun ringq_desk_park.lua lamps <domain>        (event socket / API)
//
// A script gets a Script: the call it runs on (nil when it was started from the
// API), its arguments (Arg(1) is argv[1]), the output it gives back, and the
// session-like helpers the Lua scripts used (Var, SetVar, Execute, Hangup,
// Answer, Sleep, API). Add a script with RegisterScript in its own file.

// ScriptFunc is one script.
type ScriptFunc func(s *Script) error

var (
	scriptMu  sync.RWMutex
	scriptMap = map[string]ScriptFunc{}
)

// RegisterScript installs a script under name (with or without ".lua").
func RegisterScript(name string, fn ScriptFunc) {
	scriptMu.Lock()
	scriptMap[scriptName(name)] = fn
	scriptMu.Unlock()
}

func scriptName(name string) string {
	return strings.TrimSuffix(filepath.Base(strings.TrimSpace(name)), ".lua")
}

func lookupScript(name string) ScriptFunc {
	scriptMu.RLock()
	defer scriptMu.RUnlock()
	return scriptMap[scriptName(name)]
}

// hasScript reports whether a script is installed (for the compat report).
func hasScript(name string) bool { return lookupScript(name) != nil }

// Script is one run of a script.
type Script struct {
	Core *Core
	Ch   *Channel // the call; nil when started from the API
	Ctx  context.Context
	Name string
	Args []string // argv[1:]
	// Env is the hangup event the script was started by (api_hangup_hook): the
	// call's variables. Nil otherwise.
	Env map[string]string
	out strings.Builder
}

// Arg is argv[i] (1-based); "" when absent.
func (s *Script) Arg(i int) string {
	if i < 1 || i > len(s.Args) {
		return ""
	}
	return s.Args[i-1]
}

// Write adds to the output a ${lua(...)} or `lua` API call gives back.
func (s *Script) Write(text string) { s.out.WriteString(text) }

// Ready is session:ready(): there is a call and it is up.
func (s *Script) Ready() bool { return s.Ch != nil && !s.Ch.IsHungUp() && s.Ctx.Err() == nil }

// Var reads a channel variable ("" without a call or when unset).
func (s *Script) Var(name string) string {
	if s.Ch == nil {
		return ""
	}
	return s.Ch.Var(name)
}

// SetVar sets a channel variable.
func (s *Script) SetVar(name, value string) {
	if s.Ch != nil {
		s.Ch.SetVar(name, value)
	}
}

// UUID is the call's uuid.
func (s *Script) UUID() string {
	if s.Ch == nil {
		return ""
	}
	return s.Ch.UUID
}

// Execute is session:execute(app, data): a dialplan application run on the call.
func (s *Script) Execute(app, data string) error {
	if s.Ch == nil {
		return fmt.Errorf("%s: no call", app)
	}
	return s.Core.execApp(s.Ctx, s.Ch, app, data, "")
}

// Hangup is session:hangup(cause).
func (s *Script) Hangup(cause string) {
	if s.Ch != nil {
		s.Ch.Hangup(ParseCause(cause))
	}
}

// Answer is session:answer(): answers the call with media the script can use.
func (s *Script) Answer() error {
	if s.Ch == nil {
		return fmt.Errorf("answer: no call")
	}
	return startMedia(s.Ch, false)
}

// Sleep is session:sleep(ms); it returns early when the call ends.
func (s *Script) Sleep(ms int) {
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	var done <-chan struct{}
	if s.Ch != nil {
		done = s.Ch.Done()
	}
	select {
	case <-t.C:
	case <-s.Ctx.Done():
	case <-done:
	}
}

// API runs an API command and gives its trimmed output ("" for _undef_ and errors).
func (s *Script) API(line string) string {
	out := strings.TrimSpace(s.Core.API(line))
	if out == "_undef_" || strings.HasPrefix(out, "-ERR") || strings.HasPrefix(out, "-USAGE") {
		return ""
	}
	return out
}

// Log writes to the switch's log under the script's name.
func (s *Script) Log(level, msg string, kv ...any) {
	args := append([]any{"script", s.Name}, kv...)
	switch level {
	case "err", "error":
		s.Core.log.Error(msg, args...)
	case "warn", "warning":
		s.Core.log.Warn(msg, args...)
	default:
		s.Core.log.Info(msg, args...)
	}
}

// runScript runs a registered script and returns what it wrote.
func (c *Core) runScript(ctx context.Context, ch *Channel, name string, args []string) (string, bool, error) {
	return c.runScriptEnv(ctx, ch, name, args, nil)
}

// runScriptEnv is runScript with the hangup event (see Script.Env). "app.lua <name> ..." finds the
// FusionPBX application script registered as app_<name> (RegisterAppScript).
func (c *Core) runScriptEnv(ctx context.Context, ch *Channel, name string, args []string, env map[string]string) (string, bool, error) {
	fn := lookupScript(name)
	if fn == nil && scriptName(name) == "app" && len(args) > 0 {
		// FusionPBX apps (app.lua follow_me <uuid>) are scripts named "app.<name>"
		if fn = lookupScript("app." + args[0]); fn != nil {
			name, args = "app."+args[0], args[1:]
		} else if fn = lookupScript("app_" + args[0]); fn != nil {
			name = "app_" + args[0] // RegisterAppScript: Arg(1) stays the application name, like argv[1]
		}
	}
	if fn == nil {
		return "", false, nil
	}
	s := &Script{Core: c, Ch: ch, Ctx: ctx, Name: scriptName(name), Args: args, Env: env}
	err := fn(s)
	return s.out.String(), true, err
}

func init() {
	// lua <script> [args]: run a script and give its output; luarun does the same in the background.
	apiCommands["lua"] = apiCommand{"lua <script> [args]", "run a RingQ script", func(c *Core, args []string) (string, error) {
		if len(args) == 0 {
			return "", fmt.Errorf("usage: lua <script> [args]")
		}
		out, found, err := c.runScript(context.Background(), nil, args[0], args[1:])
		if !found {
			return "", fmt.Errorf("no such script %s", args[0])
		}
		if err != nil {
			return "", err
		}
		return out, nil
	}}
	apiCommands["luarun"] = apiCommand{"luarun <script> [args]", "run a RingQ script in the background", func(c *Core, args []string) (string, error) {
		if len(args) == 0 {
			return "", fmt.Errorf("usage: luarun <script> [args]")
		}
		if !c.canRunScript(args[0], args[1:]) {
			return "", fmt.Errorf("no such script %s", args[0])
		}
		go func() {
			if _, _, err := c.runScript(context.Background(), nil, args[0], args[1:]); err != nil {
				c.log.Warn("luarun failed", "script", args[0], "err", err)
			}
		}()
		return "+OK\n", nil
	}}
}

// SQLFunc runs a SELECT on the RingQ database and gives each row as text
// columns (set by the database writer with SetSQL).
type SQLFunc func(ctx context.Context, query string, args ...any) ([]map[string]string, error)

// SetSQL installs the database reader the scripts use (nil: none).
func (c *Core) SetSQL(fn SQLFunc) {
	if fn == nil {
		c.sql.Store(nil)
		return
	}
	c.sql.Store(&fn)
}

// sqlRows runs a SELECT for a script; it fails when the switch has no database.
func (c *Core) sqlRows(ctx context.Context, query string, args ...any) ([]map[string]string, error) {
	fn := c.sql.Load()
	if fn == nil {
		return nil, fmt.Errorf("no database")
	}
	return (*fn)(ctx, query, args...)
}

// SQLExecFunc runs an INSERT, UPDATE or DELETE on the RingQ database and gives
// the number of rows it changed (set by the database writer with SetSQLExec).
type SQLExecFunc func(ctx context.Context, query string, args ...any) (int64, error)

// SetSQLExec installs the database writer the scripts use (nil: none). The
// scripts that save a setting (follow me, call flows, dial strings...) use it,
// as the Lua ones did with dbh:query.
func (c *Core) SetSQLExec(fn SQLExecFunc) {
	if fn == nil {
		c.sqlx.Store(nil)
		return
	}
	c.sqlx.Store(&fn)
}

// sqlExec runs a data-changing statement for a script; it fails when the switch has no database.
func (c *Core) sqlExec(ctx context.Context, query string, args ...any) (int64, error) {
	fn := c.sqlx.Load()
	if fn == nil {
		return 0, fmt.Errorf("no database")
	}
	return (*fn)(ctx, query, args...)
}

// canRunScript reports whether runScript would find the script (including "app.lua <name>").
func (c *Core) canRunScript(name string, args []string) bool {
	if hasScript(name) {
		return true
	}
	return scriptName(name) == "app" && len(args) > 0 && (hasScript("app."+args[0]) || hasScript("app_"+args[0]))
}

// RegisterAppScript installs a FusionPBX application script (app/<name>/index.lua), started as
// "app.lua <name> [args]". Its Arg(1) is the application name, like argv[1] of the Lua.
func RegisterAppScript(name string, fn ScriptFunc) { RegisterScript("app_"+name, fn) }
