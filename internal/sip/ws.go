package sip

import (
	"net/netip"
	"sync"
)

// SIP over WebSocket (RFC 7118). The web layer owns the TLS listener and the
// sockets; each connection is attached to the stack as a reliable transport
// named "wss". Browsers speak WebRTC, so SDP crossing this boundary is
// translated by a MediaGateway: the rest of the switch only ever sees plain
// RTP/AVP offers and answers.

// MediaGateway translates between WebRTC SDP (browsers) and plain RTP SDP
// (the switch core). Implementations must be idempotent for retransmitted
// messages: the same message may be offered to ToClient more than once.
type MediaGateway interface {
	// FromClient rewrites the SDP body of a message received from a
	// WebSocket client into plain RTP form. An error on an INVITE is
	// answered with 488.
	FromClient(m *Message) error
	// ToClient rewrites the plain RTP SDP body of a message about to be sent
	// to a WebSocket client into WebRTC form.
	ToClient(m *Message, dst Addr) error
	// Done releases the media of a call that ended.
	Done(callID string)
	// ClientGone releases everything of a WebSocket connection that closed.
	ClientGone(remote netip.AddrPort)
}

type gwRef struct {
	mu sync.RWMutex
	g  MediaGateway
}

func (r *gwRef) get() MediaGateway {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.g
}

func (r *gwRef) toClient(m *Message, dst Addr) error {
	g := r.get()
	if g == nil {
		return nil
	}
	if m.HasSDP() {
		if err := g.ToClient(m, dst); err != nil {
			return err
		}
	}
	endsCall(g, m)
	return nil
}

// endsCall tells the gateway when a message (either way) ends the media of a call.
func endsCall(g MediaGateway, m *Message) {
	if m.IsRequest() {
		if m.Method == "BYE" || m.Method == "CANCEL" {
			g.Done(m.CallID())
		}
		return
	}
	if _, method := m.CSeq(); method == "INVITE" && m.StatusCode >= 300 {
		g.Done(m.CallID())
	}
}

// SetMediaGateway sets the translator used for WebSocket clients. Without
// one, WebSocket clients can only use plain RTP offers.
func (s *Stack) SetMediaGateway(g MediaGateway) {
	s.gw.mu.Lock()
	s.gw.g = g
	s.gw.mu.Unlock()
}

// WSConn is one attached WebSocket connection.
type WSConn struct {
	s      *Stack
	tc     *tcpConn
	remote netip.AddrPort
}

// AttachWS registers a WebSocket connection with the stack. write sends one
// SIP message as one WebSocket message; closer closes the socket. Every
// message read from the socket goes to Receive, and Close is called when the
// socket is gone.
func (s *Stack) AttachWS(remote netip.AddrPort, write func([]byte) error, closer func() error) *WSConn {
	remote = unmapAP(remote)
	tc := &tcpConn{remote: remote, ws: write, closer: closer}
	s.tr.addConn(tc)
	return &WSConn{s: s, tc: tc, remote: remote}
}

// Receive handles one message read from the socket.
func (w *WSConn) Receive(raw []byte) {
	t := w.s.tr
	if isKeepalive(raw) {
		// RFC 5626: a CRLFCRLF ping is answered with a CRLF pong.
		_ = w.tc.write([]byte("\r\n"))
		return
	}
	t.stats.received.Add(1)
	msg, err := Parse(raw)
	if err != nil {
		t.unparsable(w.remote, raw, err)
		return
	}
	msg.Src = Addr{Transport: "wss", AP: w.remote}
	if g := w.s.gw.get(); g != nil {
		if msg.HasSDP() {
			if err := g.FromClient(msg); err != nil {
				w.s.log.Warn("sip: websocket client media refused", "src", msg.Src.String(), "msg", msg.Short(), "err", err)
				if msg.IsRequest() && msg.Method == "INVITE" {
					resp := NewResponse(msg, 488, "")
					resp.Add("Warning", `399 ORswitch "no usable audio codec (G.711 needed)"`)
					_ = w.s.send(resp, msg.Src)
				}
				return
			}
		}
		endsCall(g, msg)
	}
	t.deliver(msg)
}

// Close detaches the connection (the socket is already closed or closing).
func (w *WSConn) Close() {
	t := w.s.tr
	t.mu.Lock()
	if t.conns[w.remote] == w.tc {
		delete(t.conns, w.remote)
	}
	t.mu.Unlock()
	if g := w.s.gw.get(); g != nil {
		g.ClientGone(w.remote)
	}
	if t.closed != nil && !t.closing.Load() {
		t.closed(Addr{Transport: "wss", AP: w.remote})
	}
}
