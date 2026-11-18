package sip

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxHeaderBytes = 64 << 10
	maxBodyBytes   = 256 << 10
	tcpIdleTimeout = 70 * time.Minute
	tcpDialTimeout = 5 * time.Second
	tcpWriteTimout = 5 * time.Second
)

// TransportConfig configures listening sockets.
type TransportConfig struct {
	ListenIP  string // empty or "0.0.0.0" listens on all interfaces
	Port      int    // 0 picks a free port
	EnableTCP bool
	// TLSNoVerify stops us checking the certificate of a TLS destination (a trunk with a private or mismatched
	// certificate). By default the chain and the name are verified.
	TLSNoVerify bool
}

type tcpConn struct {
	c      net.Conn
	remote netip.AddrPort
	wmu    sync.Mutex
	secure bool // TLS: messages on it are "tls", not "tcp"

	// A WebSocket connection (RFC 7118) has no net.Conn here: the web layer
	// owns the socket and gives us these two functions.
	ws     func([]byte) error
	closer func() error
}

func (tc *tcpConn) write(b []byte) error {
	tc.wmu.Lock()
	defer tc.wmu.Unlock()
	if tc.ws != nil {
		return tc.ws(b)
	}
	_ = tc.c.SetWriteDeadline(time.Now().Add(tcpWriteTimout))
	_, err := tc.c.Write(b)
	return err
}

// name is the transport of the messages on this connection.
func (tc *tcpConn) name() string {
	if tc.secure {
		return "tls"
	}
	return "tcp"
}

func (tc *tcpConn) close() {
	if tc.c != nil {
		tc.c.Close()
	} else if tc.closer != nil {
		_ = tc.closer()
	}
}

type transport struct {
	stats   *counters
	notice  *noticer
	udp     *net.UDPConn
	tcp     *net.TCPListener
	log     *slog.Logger
	deliver func(*Message)
	closed  func(Addr)

	mu      sync.Mutex
	conns   map[netip.AddrPort]*tcpConn
	extra   map[uint16]*listener // more ports (SIP profiles), by port
	closing atomic.Bool
	wg      sync.WaitGroup

	// SIP over TLS (RFC 3261 26): the listeners, the server certificate and what connections we open ourselves.
	tlsLns     []net.Listener
	tlsServer  *tls.Config
	tlsPort    int
	tlsNoCheck bool // TransportConfig.TLSNoVerify
}

// clientTLS is how we connect to a TLS destination (a trunk): the certificate chain and the name (the host the
// gateway was configured with, or the IP) are verified unless the operator switched that off (sip-tls-verify=false).
func (t *transport) clientTLS(serverName string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	cfg.InsecureSkipVerify = t.tlsNoCheck
	return cfg
}

// addTLS listens for SIP over TLS on port.
func (t *transport) addTLS(listenIP string, port int, cfg *tls.Config) error {
	var ip net.IP
	if listenIP != "" && listenIP != "0.0.0.0" {
		if ip = net.ParseIP(listenIP); ip == nil {
			return fmt.Errorf("sip: invalid listen ip %q", listenIP)
		}
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("sip: invalid tls port %d", port)
	}
	if cfg == nil {
		return errors.New("sip: tls needs a certificate")
	}
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: port})
	if err != nil {
		return fmt.Errorf("sip: tls listen on %d: %w", port, err)
	}
	t.mu.Lock()
	t.tlsLns = append(t.tlsLns, l)
	t.tlsServer = cfg
	if t.tlsPort == 0 {
		t.tlsPort = l.Addr().(*net.TCPAddr).Port
	}
	t.mu.Unlock()
	t.wg.Add(1)
	go t.acceptTLS(l, cfg)
	return nil
}

func (t *transport) acceptTLS(l *net.TCPListener, cfg *tls.Config) {
	defer t.wg.Done()
	for {
		c, err := l.AcceptTCP()
		if err != nil {
			if t.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			t.log.Warn("sip tls accept error", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = c.SetKeepAlive(true)
		go t.handshake(c, cfg)
	}
}

// handshake completes the TLS handshake of an accepted connection, then serves it like a TCP one.
func (t *transport) handshake(c *net.TCPConn, cfg *tls.Config) {
	remote := unmapAP(c.RemoteAddr().(*net.TCPAddr).AddrPort())
	tc := tls.Server(c, cfg)
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		if !t.closing.Load() && t.notice.allow("tls|"+remote.Addr().String()) {
			t.log.Info("sip tls handshake failed (logged once a minute per address)", "remote", remote.String(), "err", err)
		}
		c.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})
	conn := &tcpConn{c: tc, remote: remote, secure: true}
	t.addConn(conn)
	t.serveTCP(conn)
}

// listener is an extra pair of sockets on another port.
type listener struct {
	udp *net.UDPConn
	tcp *net.TCPListener
}

func listenTransport(cfg TransportConfig, log *slog.Logger, deliver func(*Message), closed func(Addr), stats *counters, notice *noticer) (*transport, error) {
	var ip net.IP
	if cfg.ListenIP != "" && cfg.ListenIP != "0.0.0.0" {
		if ip = net.ParseIP(cfg.ListenIP); ip == nil {
			return nil, fmt.Errorf("sip: invalid listen ip %q", cfg.ListenIP)
		}
	}
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: cfg.Port})
	if err != nil {
		return nil, fmt.Errorf("sip: udp listen: %w", err)
	}
	_ = u.SetReadBuffer(4 << 20)
	t := &transport{udp: u, log: log, deliver: deliver, closed: closed, conns: map[netip.AddrPort]*tcpConn{}, extra: map[uint16]*listener{},
		stats: stats, notice: notice, tlsNoCheck: cfg.TLSNoVerify}
	if cfg.EnableTCP {
		port := u.LocalAddr().(*net.UDPAddr).Port
		l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: port})
		if err != nil {
			u.Close()
			return nil, fmt.Errorf("sip: tcp listen: %w", err)
		}
		t.tcp = l
	}
	return t, nil
}

// start begins reading; it is separate from listenTransport so the owner
// is fully initialised before the first message is delivered.
func (t *transport) start() {
	if t.tcp != nil {
		t.wg.Add(1)
		go t.acceptLoop(t.tcp)
	}
	t.wg.Add(1)
	go t.udpLoop(t.udp, 0)
}

// addListener opens another port (UDP, and TCP when tcp is set).
func (t *transport) addListener(listenIP string, port int, tcp bool) error {
	var ip net.IP
	if listenIP != "" && listenIP != "0.0.0.0" {
		if ip = net.ParseIP(listenIP); ip == nil {
			return fmt.Errorf("sip: invalid listen ip %q", listenIP)
		}
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("sip: invalid port %d", port)
	}
	t.mu.Lock()
	_, have := t.extra[uint16(port)]
	t.mu.Unlock()
	if have || port == t.port() {
		return nil
	}
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return fmt.Errorf("sip: udp listen on %d: %w", port, err)
	}
	_ = u.SetReadBuffer(4 << 20)
	l := &listener{udp: u}
	if tcp {
		if l.tcp, err = net.ListenTCP("tcp", &net.TCPAddr{IP: ip, Port: port}); err != nil {
			u.Close()
			return fmt.Errorf("sip: tcp listen on %d: %w", port, err)
		}
	}
	t.mu.Lock()
	t.extra[uint16(port)] = l
	t.mu.Unlock()
	t.wg.Add(1)
	go t.udpLoop(u, uint16(port))
	if l.tcp != nil {
		t.wg.Add(1)
		go t.acceptLoop(l.tcp)
	}
	return nil
}

// removeListener closes an extra port.
func (t *transport) removeListener(port uint16) {
	t.mu.Lock()
	l := t.extra[port]
	delete(t.extra, port)
	t.mu.Unlock()
	if l != nil {
		l.udp.Close()
		if l.tcp != nil {
			l.tcp.Close()
		}
	}
}

func (t *transport) listening(port uint16) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.extra[port] != nil
}

func (t *transport) port() int { return t.udp.LocalAddr().(*net.UDPAddr).Port }

func unmapAP(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func isKeepalive(b []byte) bool {
	if len(b) > 4 {
		return false
	}
	for _, c := range b {
		if c != '\r' && c != '\n' && c != 0 && c != ' ' {
			return false
		}
	}
	return true
}

func (t *transport) udpLoop(conn *net.UDPConn, local uint16) {
	defer t.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if t.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			t.log.Warn("sip udp read error", "err", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if isKeepalive(buf[:n]) {
			continue
		}
		t.stats.received.Add(1)
		msg, err := Parse(buf[:n])
		if err != nil {
			t.unparsable(unmapAP(from), buf[:n], err)
			continue
		}
		msg.Src = Addr{Transport: "udp", AP: unmapAP(from), Local: local}
		t.deliver(msg)
	}
}

func (t *transport) unparsable(from netip.AddrPort, b []byte, err error) {
	t.stats.unparsable.Add(1)
	if t.notice.allow("parse|" + from.Addr().String()) {
		t.log.Warn("dropping packet that is not valid SIP (logged once a minute per address)",
			"from", from.String(), "err", err, "first_line", firstLine(b))
	}
}

func (t *transport) acceptLoop(l *net.TCPListener) {
	defer t.wg.Done()
	for {
		c, err := l.AcceptTCP()
		if err != nil {
			if t.closing.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			t.log.Warn("sip tcp accept error", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = c.SetKeepAlive(true)
		remote := unmapAP(c.RemoteAddr().(*net.TCPAddr).AddrPort())
		tc := &tcpConn{c: c, remote: remote}
		t.addConn(tc)
		go t.serveTCP(tc)
	}
}

func (t *transport) addConn(tc *tcpConn) {
	t.mu.Lock()
	if old := t.conns[tc.remote]; old != nil && old != tc {
		old.close()
	}
	t.conns[tc.remote] = tc
	t.mu.Unlock()
}

func (t *transport) serveTCP(tc *tcpConn) {
	defer func() {
		tc.c.Close()
		t.mu.Lock()
		if t.conns[tc.remote] == tc {
			delete(t.conns, tc.remote)
		}
		t.mu.Unlock()
		if t.closed != nil && !t.closing.Load() {
			t.closed(Addr{Transport: tc.name(), AP: tc.remote})
		}
	}()
	r := bufio.NewReaderSize(tc.c, 32<<10)
	for {
		_ = tc.c.SetReadDeadline(time.Now().Add(tcpIdleTimeout))
		raw, err := readStreamMessage(r, func() { _ = tc.write([]byte("\r\n")) })
		if err != nil {
			if !errors.Is(err, io.EOF) && !t.closing.Load() {
				t.log.Debug("sip tcp connection closed", "remote", tc.remote, "err", err)
			}
			return
		}
		t.stats.received.Add(1)
		msg, err := Parse(raw)
		if err != nil {
			t.unparsable(tc.remote, raw, err)
			continue
		}
		msg.Src = Addr{Transport: tc.name(), AP: tc.remote}
		t.deliver(msg)
	}
}

// readStreamMessage reads one message framed by Content-Length from a stream.
// RFC 5626 CRLF keep-alive pings are answered through pong.
func readStreamMessage(r *bufio.Reader, pong func()) ([]byte, error) {
	emptyLines := 0
	for {
		b, err := r.Peek(1)
		if err != nil {
			return nil, err
		}
		if b[0] != '\r' && b[0] != '\n' {
			break
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if strings.TrimRight(line, "\r\n") == "" {
			emptyLines++
			if emptyLines == 2 {
				pong()
				emptyLines = 0
			}
		}
	}
	var head bytes.Buffer
	contentLength := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		head.WriteString(line)
		if head.Len() > maxHeaderBytes {
			return nil, errors.New("sip: header section too large")
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		if name, value, ok := strings.Cut(trimmed, ":"); ok {
			if n := strings.ToLower(strings.TrimSpace(name)); n == "content-length" || n == "l" {
				contentLength, err = strconv.Atoi(strings.TrimSpace(value))
				if err != nil || contentLength < 0 || contentLength > maxBodyBytes {
					return nil, fmt.Errorf("sip: bad Content-Length %q", value)
				}
			}
		}
	}
	if contentLength > 0 {
		body := make([]byte, contentLength)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		head.Write(body)
	}
	return head.Bytes(), nil
}

func (t *transport) send(b []byte, dst Addr) error {
	switch dst.Transport {
	case "udp":
		conn := t.udp
		if dst.Local != 0 {
			t.mu.Lock()
			if l := t.extra[dst.Local]; l != nil {
				conn = l.udp
			}
			t.mu.Unlock()
		}
		_, err := conn.WriteToUDPAddrPort(b, dst.AP)
		return err
	case "tcp":
		tc, err := t.conn(dst.AP)
		if err != nil {
			return err
		}
		if err := tc.write(b); err != nil {
			tc.close()
			return err
		}
		return nil
	case "tls":
		tc, err := t.connTLS(dst.AP, dst.Host)
		if err != nil {
			return err
		}
		if err := tc.write(b); err != nil {
			tc.close()
			return err
		}
		return nil
	case "wss":
		// A WebSocket client is only reachable over the connection it opened.
		t.mu.Lock()
		tc := t.conns[dst.AP]
		t.mu.Unlock()
		if tc == nil || tc.ws == nil {
			return errors.New("sip: websocket connection closed")
		}
		if err := tc.write(b); err != nil {
			tc.close()
			return err
		}
		return nil
	}
	return fmt.Errorf("sip: unsupported transport %q", dst.Transport)
}

// connTLS returns the TLS connection to ap: the one the peer opened to us, or a new one we dial (a trunk).
func (t *transport) connTLS(ap netip.AddrPort, serverName string) (*tcpConn, error) {
	t.mu.Lock()
	tc := t.conns[ap]
	t.mu.Unlock()
	if tc != nil && tc.secure {
		return tc, nil
	}
	d := &net.Dialer{Timeout: tcpDialTimeout}
	c, err := tls.DialWithDialer(d, "tcp", ap.String(), t.clientTLS(serverName))
	if err != nil {
		return nil, err
	}
	tc = &tcpConn{c: c, remote: ap, secure: true}
	t.addConn(tc)
	go t.serveTCP(tc)
	return tc, nil
}

func (t *transport) conn(ap netip.AddrPort) (*tcpConn, error) {
	t.mu.Lock()
	tc := t.conns[ap]
	t.mu.Unlock()
	if tc != nil {
		return tc, nil
	}
	if t.tcp == nil {
		return nil, errors.New("sip: tcp transport disabled")
	}
	c, err := net.DialTimeout("tcp", ap.String(), tcpDialTimeout)
	if err != nil {
		return nil, err
	}
	tc = &tcpConn{c: c, remote: ap}
	t.addConn(tc)
	go t.serveTCP(tc)
	return tc, nil
}

func (t *transport) close() {
	t.closing.Store(true)
	t.udp.Close()
	if t.tcp != nil {
		t.tcp.Close()
	}
	t.mu.Lock()
	for _, l := range t.tlsLns {
		l.Close()
	}
	for _, tc := range t.conns {
		tc.close()
	}
	for _, l := range t.extra {
		l.udp.Close()
		if l.tcp != nil {
			l.tcp.Close()
		}
	}
	t.mu.Unlock()
	t.wg.Wait()
}
