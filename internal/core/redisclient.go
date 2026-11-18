package core

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// A minimal Redis client (RESP2) for the RingQ scripts: the backend keeps the
// extensions' selected call profile ("<ext>+<domain>") and the shared parking
// slots ("<domain>:parked") in its Redis. One short connection per command; the
// scripts only run once per call.

const defaultRedisSocket = "/var/run/redis/redis-server.sock"

func (c *Core) redisAddr() (network, addr string) {
	a := strings.TrimSpace(c.config().Settings.RedisAddr)
	switch {
	case strings.HasPrefix(a, "unix:"):
		return "unix", strings.TrimPrefix(a, "unix:")
	case strings.HasPrefix(a, "/"):
		return "unix", a
	case a != "":
		return "tcp", a
	}
	if _, err := os.Stat(defaultRedisSocket); err == nil {
		return "unix", defaultRedisSocket
	}
	return "tcp", "127.0.0.1:6379"
}

// redisDo runs one command. The reply is a string, int64, []any (arrays, with
// nil for null elements) or nil.
func (c *Core) redisDo(args ...string) (any, error) {
	network, addr := c.redisAddr()
	conn, err := net.DialTimeout(network, addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	return readRESP(bufio.NewReader(conn))
}

func readRESP(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, fmt.Errorf("redis: empty reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, fmt.Errorf("redis: %s", line[1:])
	case ':':
		n, _ := strconv.ParseInt(line[1:], 10, 64)
		return n, nil
	case '$':
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := readFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return nil, nil
		}
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			v, err := readRESP(r)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("redis: bad reply %q", line)
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// redisStr runs a command and gives its reply as text ("" for null or an error).
func (c *Core) redisStr(args ...string) string {
	v, err := c.redisDo(args...)
	if err != nil {
		c.log.Debug("redis", "cmd", args[0], "err", err)
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}
