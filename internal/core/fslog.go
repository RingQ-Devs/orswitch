package core

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ringq-devs/orswitch/internal/logbus"
)

// FreeSWITCH-format log file. RingQ Core reads and writes
// /var/log/freeswitch/freeswitch.log: Call Debug truncates it and shows what
// is appended, and the fail2ban filters of Security (Anti-hacking) match its
// lines ("[WARNING] sofia_reg.c:NNN SIP auth failure (REGISTER) on sofia
// profile 'internal' for [user@dom] from ip A.B.C.D", "[INFO] sofia.c:NNN
// receiving invite from A.B.C.D:port"). With the setting
// freeswitch-log-file the switch writes its log there in that layout.
//
// It logs at info level. While Call Debug is switched on for a domain
// (Redis call_debug:<domain> = 1) it logs at debug level, with the dialplan
// trace, so nothing is generated for debug lines the rest of the time.

var (
	reAuthFail  = regexp.MustCompile(`^authentication failed \(wrong user or password\) method=(\S+) user=(\S*) src=(?:\w+:)?(\S+?)(?::\d+)? `)
	reChallenge = regexp.MustCompile(`^SIP auth challenge \((\w+)\) for \[(.*)\] from ip (\S+)$`)
	reInvite    = regexp.MustCompile(`^\S+ receiving invite from (\S+) (version: .*)$`)
)

// fsLogLine renders one log line as FreeSWITCH writes it.
func fsLogLine(l logbus.Line, host string) string {
	if l.Raw {
		return l.Text + "\n"
	}
	text, file, num := l.Text, l.File, l.Num
	level := logbus.LevelName(l.Level)
	switch {
	case reAuthFail.MatchString(text):
		m := reAuthFail.FindStringSubmatch(text)
		user := m[2]
		if !strings.Contains(user, "@") && host != "" {
			user += "@" + host
		}
		text = fmt.Sprintf("SIP auth failure (%s) on sofia profile 'internal' for [%s] from ip %s", m[1], user, m[3])
		file, num, level = "sofia_reg.c", 3072, "WARNING"
	case reChallenge.MatchString(text):
		m := reChallenge.FindStringSubmatch(text)
		text = fmt.Sprintf("SIP auth challenge (%s) on sofia profile 'internal' for [%s] from ip %s", m[1], m[2], m[3])
		file, num = "sofia_reg.c", 2999
	case reInvite.MatchString(text):
		m := reInvite.FindStringSubmatch(text)
		text = "receiving invite from " + m[1] + " " + m[2]
		file, num = "sofia.c", 1389
	}
	src := ""
	if file != "" {
		src = fmt.Sprintf("%s:%d ", file, num)
	}
	return fmt.Sprintf("%s %s 100.00%% [%s] %s%s\n", firstNonEmpty(l.UUID, "00000000-0000-0000-0000-000000000000"),
		l.Time.Format("2006-01-02 15:04:05.000000"), level, src, text)
}

// FSLog writes the log to a file in FreeSWITCH's layout.
type FSLog struct {
	c    *Core
	bus  *logbus.Bus
	path string
	sub  *logbus.Sub
	mu   sync.Mutex
	f    *os.File
	done chan struct{}
}

// StartFSLog follows bus and writes it to path. Stop ends it.
func (c *Core) StartFSLog(bus *logbus.Bus, path string) (*FSLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o664)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o664) // Core (another user) truncates it for Call Debug
	l := &FSLog{c: c, bus: bus, path: path, f: f, done: make(chan struct{})}
	l.sub = bus.Subscribe(6)
	go l.run()
	go l.watchDebug()
	return l, nil
}

// Stop closes the file.
func (l *FSLog) Stop() {
	select {
	case <-l.done:
		return
	default:
	}
	close(l.done)
	l.bus.Unsubscribe(l.sub)
}

func (l *FSLog) run() {
	host := ""
	for line := range l.sub.C {
		if host == "" {
			host = l.c.config().Settings.Domain
		}
		l.mu.Lock()
		w := bufio.NewWriter(l.f)
		_, _ = w.WriteString(fsLogLine(line, host))
		// drain what is queued, one write for the burst
		for n := len(l.sub.C); n > 0; n-- {
			if next, ok := <-l.sub.C; ok {
				_, _ = w.WriteString(fsLogLine(next, host))
			}
		}
		_ = w.Flush()
		l.mu.Unlock()
	}
	l.mu.Lock()
	_ = l.f.Close()
	l.mu.Unlock()
}

// debugWanted says whether Call Debug is on for some domain.
func (c *Core) debugWanted() bool {
	v, err := c.redisDo("KEYS", "call_debug:*")
	keys, _ := v.([]any)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if s, ok := k.(string); ok && c.redisStr("GET", s) == "1" {
			return true
		}
	}
	return false
}

func (l *FSLog) watchDebug() {
	level := 6
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
			want := 6
			if l.c.debugWanted() {
				want = 7
			}
			if want != level {
				level = want
				l.sub.SetLevel(level)
			}
		}
	}
}
