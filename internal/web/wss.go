package web

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// SIP over WebSocket (RFC 7118) for web dialers such as sip.js: a TLS port on
// which every WebSocket connection becomes a SIP transport of the switch.
// Registration, calls and BLF then work as for any other phone; the WebRTC
// media of those calls is handled by sipGateway.

const (
	wssIdleTimeout = 10 * time.Minute // clients ping with CRLFCRLF every 25 s
	wssWriteWait   = 5 * time.Second
	wssMaxMessage  = 256 << 10
)

var wssUpgrader = websocket.Upgrader{
	Subprotocols:    []string{"sip"},
	ReadBufferSize:  16 << 10,
	WriteBufferSize: 16 << 10,
	// The page that opens the socket is served by another origin (the
	// RingQ web app); SIP authentication protects the registration.
	CheckOrigin: func(*http.Request) bool { return true },
}

// startWSS opens the TLS WebSocket port.
func (s *Server) startWSS(host string, port int, tlsCfg *tls.Config) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return err
	}
	s.wss = &http.Server{
		Handler:           http.HandlerFunc(s.serveWSS),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          nil,
	}
	go func() { _ = s.wss.Serve(tls.NewListener(ln, tlsCfg)) }()
	s.log.Info("web: SIP over WebSocket listening", "url", fmt.Sprintf("wss://%s/", ln.Addr()))
	return nil
}

func (s *Server) serveWSS(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		http.NotFound(w, r)
		return
	}
	conn, err := wssUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ap, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		conn.Close()
		return
	}
	var wmu sync.Mutex
	h := s.sw.AttachWS(ap, func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(wssWriteWait))
		return conn.WriteMessage(websocket.TextMessage, b)
	}, conn.Close)
	defer h.Close()
	defer conn.Close()
	conn.SetReadLimit(wssMaxMessage)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(wssIdleTimeout))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		h.Receive(data)
	}
}
