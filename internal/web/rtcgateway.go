package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/ringq-devs/orswitch/internal/sdp"
	"github.com/ringq-devs/orswitch/internal/sip"
)

// sipGateway lets browsers register and call over SIP-over-WebSocket with
// WebRTC media (sip.js and similar). It implements sip.MediaGateway: SDP that
// crosses between a browser and the switch is translated, so the switch core
// only ever sees an ordinary RTP/AVP peer on the loopback interface.
//
//	browser <== ICE + DTLS-SRTP ==> pion PeerConnection <-- RTP/UDP loopback --> switch RTP endpoint
//
// Only G.711 (PCMU/PCMA) and telephone-event are offered to the switch, so
// audio passes between both sides without transcoding.
type sipGateway struct {
	e   *rtcEngine
	log *slog.Logger

	mu    sync.Mutex
	calls map[string]*rtcCall // by Call-ID
}

func newSIPGateway(e *rtcEngine, log *slog.Logger) *sipGateway {
	return &sipGateway{e: e, log: log, calls: map[string]*rtcCall{}}
}

type rtcCall struct {
	g      *sipGateway
	id     string
	remote netip.AddrPort

	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticRTP
	br      *net.UDPConn // loopback socket the switch's RTP endpoint talks to
	swallow *net.UDPConn // its RTCP port; RTCP is not forwarded
	brPort  int

	corePeer atomic.Pointer[netip.AddrPort] // where the browser's RTP goes (the switch's RTP endpoint)

	mu            sync.Mutex
	trackAdded    bool
	pendingRemote bool   // the browser offered; our answer is not made yet
	answerSDP     string // our answer to the browser's last offer
	pendingLocal  bool   // we offered; the browser's answer is awaited
	offerSDP      string // our last offer
	closed        bool
}

var isG711 = func(c sdp.Codec) bool {
	return c.Rate == 8000 && (strings.EqualFold(c.Name, "PCMU") || strings.EqualFold(c.Name, "PCMA"))
}

// usableCodecs keeps the G.711 voice codecs and the 8 kHz telephone-event.
func usableCodecs(in []sdp.Codec) (out []sdp.Codec, voice int) {
	for _, c := range in {
		switch {
		case isG711(c):
			out = append(out, c)
			voice++
		case c.IsDTMF() && c.Rate == 8000:
			out = append(out, c)
		}
	}
	return out, voice
}

func isWebRTCMedia(m *sdp.Media) bool {
	return strings.Contains(m.Proto, "SAVP") || strings.Contains(m.Proto, "UDP/TLS")
}

// get returns the call of a Call-ID.
func (g *sipGateway) get(id string) *rtcCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[id]
}

// call returns the call of a Call-ID, creating it with its peer connection
// and loopback sockets when new.
func (g *sipGateway) call(id string, remote netip.AddrPort) (*rtcCall, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c := g.calls[id]; c != nil {
		return c, nil
	}
	pc, err := g.e.sipAPI.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	br, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		pc.Close()
		return nil, err
	}
	c := &rtcCall{g: g, id: id, remote: remote, pc: pc, br: br, brPort: br.LocalAddr().(*net.UDPAddr).Port}
	// Swallow the RTCP the switch sends to RTP port + 1 (no ICMP errors).
	if sw, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: c.brPort + 1}); err == nil {
		c.swallow = sw
		go func() {
			b := make([]byte, 1500)
			for {
				if _, _, err := sw.ReadFromUDPAddrPort(b); err != nil {
					return
				}
			}
		}()
	}
	c.track, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "orswitch")
	if err != nil {
		c.close()
		return nil, err
	}
	pc.OnTrack(c.onTrack)
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		g.log.Debug("webrtc leg state", "call", id, "state", s.String())
	})
	go c.fromSwitch()
	g.calls[id] = c
	return c, nil
}

// onTrack forwards the browser's RTP (voice and telephone-event) to the switch.
func (c *rtcCall) onTrack(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	if tr.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			return
		}
		peer := c.corePeer.Load()
		if peer == nil {
			continue
		}
		if b, err := pkt.Marshal(); err == nil {
			_, _ = c.br.WriteToUDPAddrPort(b, *peer)
		}
	}
}

// fromSwitch forwards the switch's RTP (voice only) to the browser.
func (c *rtcCall) fromSwitch() {
	buf := make([]byte, 1600)
	for {
		n, _, err := c.br.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		// Voice only: the static track would stamp telephone-event packets
		// with the voice payload type.
		if pt := buf[1] & 0x7f; pt != 0 && pt != 8 {
			continue
		}
		_, _ = c.track.Write(buf[:n])
	}
}

func (c *rtcCall) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	if c.pc != nil {
		_ = c.pc.Close()
	}
	if c.br != nil {
		_ = c.br.Close()
	}
	if c.swallow != nil {
		_ = c.swallow.Close()
	}
}

// addTrack attaches the voice track to the call's audio transceiver.
func (c *rtcCall) addTrack() error {
	if c.trackAdded {
		return nil
	}
	sender, err := c.pc.AddTrack(c.track)
	if err != nil {
		return err
	}
	c.trackAdded = true
	go func() {
		b := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(b); err != nil {
				return
			}
		}
	}()
	return nil
}

// preferCodecs limits the peer connection's audio transceivers to the codecs
// of the plain-RTP side, in its order.
func (c *rtcCall) preferCodecs(codecs []sdp.Codec) error {
	registered := c.g.e.sipCodecs
	var prefs []webrtc.RTPCodecParameters
	for _, want := range codecs {
		for _, have := range registered {
			name := strings.TrimPrefix(strings.ToLower(have.MimeType), "audio/")
			if strings.EqualFold(name, want.Name) && int(have.ClockRate) == want.Rate {
				prefs = append(prefs, have)
				break
			}
		}
	}
	if len(prefs) == 0 {
		return errors.New("no codec in common")
	}
	for _, t := range c.pc.GetTransceivers() {
		if t.Kind() == webrtc.RTPCodecTypeAudio {
			if err := t.SetCodecPreferences(prefs); err != nil {
				return err
			}
		}
	}
	return nil
}

// gathered waits for ICE candidates and returns the local description.
func (c *rtcCall) gathered() (string, error) {
	select {
	case <-webrtc.GatheringCompletePromise(c.pc):
	case <-time.After(4 * time.Second):
	}
	ld := c.pc.LocalDescription()
	if ld == nil {
		return "", errors.New("no local description")
	}
	return ld.SDP, nil
}

// plainSession builds the RTP/AVP description the switch sees for this call.
func (c *rtcCall) plainSession(codecs []sdp.Codec, direction string) *sdp.Session {
	return sdp.NewOffer(sdp.Local{IP: "127.0.0.1", Port: c.brPort, SessionID: strconv.FormatInt(time.Now().Unix(), 10), Version: 1}, codecs, direction)
}

func setBody(m *sip.Message, s string) {
	m.Body = []byte(s)
	m.Set("Content-Type", "application/sdp")
}

// FromClient implements sip.MediaGateway.
func (g *sipGateway) FromClient(m *sip.Message) error {
	s, err := sdp.Parse(m.Body)
	if err != nil {
		return err
	}
	a, _ := s.Audio()
	if a == nil || !isWebRTCMedia(a) {
		return nil // a plain RTP client on a WebSocket: leave it alone
	}
	codecs, voice := usableCodecs(a.Codecs())
	if voice == 0 {
		return errors.New("the offer has no G.711 codec")
	}
	dir := s.Direction(a)
	if m.IsRequest() {
		c, err := g.call(m.CallID(), m.Src.AP)
		if err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(m.Body)}); err != nil {
			return fmt.Errorf("remote offer: %w", err)
		}
		if err := c.addTrack(); err != nil {
			return err
		}
		c.pendingRemote, c.answerSDP = true, ""
		setBody(m, string(c.plainSession(codecs, dir).Marshal()))
		return nil
	}
	// A response: the browser's answer to an offer we made.
	c := g.get(m.CallID())
	if c == nil {
		return errors.New("answer for an unknown call")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingLocal {
		if err := c.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(m.Body)}); err != nil {
			return fmt.Errorf("remote answer: %w", err)
		}
		c.pendingLocal = false
	}
	setBody(m, string(c.plainSession(codecs, dir).Marshal()))
	return nil
}

// ToClient implements sip.MediaGateway.
func (g *sipGateway) ToClient(m *sip.Message, dst sip.Addr) error {
	s, err := sdp.Parse(m.Body)
	if err != nil {
		return err
	}
	a, _ := s.Audio()
	if a == nil {
		return nil // nothing to translate (T.38, rejected stream)
	}
	if isWebRTCMedia(a) {
		return nil // already translated (a retransmission)
	}
	codecs, voice := usableCodecs(a.Codecs())
	if voice == 0 {
		return errors.New("the switch's SDP has no G.711 codec")
	}
	peer, err := netip.ParseAddr(s.Conn(a))
	if err != nil {
		return fmt.Errorf("media address: %w", err)
	}
	// The switch's RTP endpoint is on this host whatever address it
	// advertises (its public one included).
	local := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(a.Port))
	_ = peer

	if m.IsRequest() {
		// The switch offers (a call to the browser, or a re-INVITE).
		c, err := g.call(m.CallID(), dst.AP)
		if err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.offerSDP != "" && c.pendingLocal {
			setBody(m, c.offerSDP) // retransmission
			return nil
		}
		c.corePeer.Store(&local)
		if err := c.addTrack(); err != nil {
			return err
		}
		if err := c.preferCodecs(codecs); err != nil {
			return err
		}
		offer, err := c.pc.CreateOffer(nil)
		if err != nil {
			return fmt.Errorf("create offer: %w", err)
		}
		if err := c.pc.SetLocalDescription(offer); err != nil {
			return fmt.Errorf("local offer: %w", err)
		}
		out, err := c.gathered()
		if err != nil {
			return err
		}
		out = withDirection(out, s.Direction(a))
		c.pendingLocal, c.offerSDP = true, out
		setBody(m, out)
		return nil
	}

	// The switch answers the browser's offer.
	c := g.get(m.CallID())
	if c == nil {
		return errors.New("answer for an unknown call")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.corePeer.Store(&local)
	if c.answerSDP != "" {
		setBody(m, c.answerSDP) // 183 then 200, or a retransmission
		return nil
	}
	if !c.pendingRemote {
		return errors.New("no offer from the browser to answer")
	}
	if err := c.preferCodecs(codecs); err != nil {
		return err
	}
	ans, err := c.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := c.pc.SetLocalDescription(ans); err != nil {
		return fmt.Errorf("local answer: %w", err)
	}
	out, err := c.gathered()
	if err != nil {
		return err
	}
	out = withDirection(out, s.Direction(a)) // hold: the core's answer says recvonly or inactive
	c.pendingRemote, c.answerSDP = false, out
	setBody(m, out)
	return nil
}

// withDirection sets the media direction of a one-audio-stream description
// (pion always writes sendrecv; the switch core decides hold and resume).
func withDirection(s, dir string) string {
	for _, d := range []string{"sendrecv", "sendonly", "recvonly", "inactive"} {
		s = strings.ReplaceAll(s, "a="+d+"\r\n", "a="+dir+"\r\n")
	}
	return s
}

// Done implements sip.MediaGateway.
func (g *sipGateway) Done(callID string) {
	g.mu.Lock()
	c := g.calls[callID]
	delete(g.calls, callID)
	g.mu.Unlock()
	if c != nil {
		c.close()
	}
}

// ClientGone implements sip.MediaGateway.
func (g *sipGateway) ClientGone(remote netip.AddrPort) {
	g.mu.Lock()
	var gone []*rtcCall
	for id, c := range g.calls {
		if c.remote == remote {
			gone = append(gone, c)
			delete(g.calls, id)
		}
	}
	g.mu.Unlock()
	for _, c := range gone {
		c.close()
	}
}

var _ sip.MediaGateway = (*sipGateway)(nil)
