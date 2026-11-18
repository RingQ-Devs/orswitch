package core

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ringq-devs/orswitch/internal/media"
)

// uuid_openai_audio_stream: RingQswitch's own version of the FreeSWITCH module
// mod_openai_audio_stream (a fork of mod_audio_stream that talks to the OpenAI
// Realtime API). The RingQ AI dialplans (AI IVR, AI agent extensions) call it:
//
//	uuid_openai_audio_stream <uuid> start wss://api.openai.com/v1/realtime?model=gpt-realtime mono 24k
//	uuid_openai_audio_stream <uuid> stop
//
// What it does, as the module does:
//   - connects to the websocket (Authorization: Bearer STREAM_OPENAI_API_KEY, or
//     the JSON headers in STREAM_EXTRA_HEADERS) and, once open, sends session.update
//     (voice, language, noise reduction, transcription, instructions) and a
//     response.create with the greeting, from the STREAM_OPENAI_* channel variables
//   - streams what the caller says as input_audio_buffer.append (L16, base64)
//   - plays response.output_audio.delta to the caller; input_audio_buffer.speech_started
//     (the caller spoke over the AI) drops what is still to be played
//   - when the caller's transcript contains a keyword of STREAM_OPENAI_TRANSFER_MAP
//     ("sales:6000,support:6001"), transfers the call after the AI has spoken
//   - fires CUSTOM mod_openai_audio_stream::{connect,disconnect,error,json,
//     openai_speech_start,openai_speech_stop}
//
// While the stream runs the call's audio belongs to it: the dialplan's
// "playback silence_stream://-1" keeps the call up but says nothing.

const oaiUsage = "uuid_openai_audio_stream <uuid> [start | stop | send_json | pause | resume] [wss-url | base64 json] [mono | mixed | stereo] [8k | 16k | 24k]"

const (
	oaiDefaultInstructions = "You are a helpful assistant."
	oaiDefaultGreeting     = "\n\nGreeting Behavior:\nWhen greeting a caller, say politely: 'Welcome to RingQ support. How can I help you today?'. Speak only about the content above—do not add any unrelated information."
	oaiOpenAIRate          = 24000 // PCM16 rate of the Realtime API
	oaiEventPrefix         = "mod_openai_audio_stream::"
)

func init() {
	apiCommands["uuid_openai_audio_stream"] = apiCommand{oaiUsage, "stream call audio to the OpenAI Realtime API", apiOpenAIStream}
}

var openAIStreams sync.Map // *Channel -> *openAIStream

func (ch *Channel) oaiOwnsMedia() bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.oaiMedia
}

func (ch *Channel) setOAIMedia(v bool) {
	ch.mu.Lock()
	ch.oaiMedia = v
	ch.mu.Unlock()
}

func apiOpenAIStream(c *Core, args []string) (string, error) {
	const fail = "-ERR Operation Failed\n"
	if len(args) < 2 || (strings.EqualFold(args[1], "start") && len(args) < 4) {
		return "-USAGE: " + oaiUsage + "\n", nil
	}
	ch := c.Channel(args[0])
	if ch == nil {
		c.log.Error("uuid_openai_audio_stream: no such channel", "uuid", args[0])
		return fail, nil
	}
	cur := loadOpenAIStream(ch)
	// stereo and mixed streams are taps (live transcription): they never own the call's audio
	if out, ok := openAITapAPI(c, ch, args, cur); ok {
		return out, nil
	}
	switch strings.ToLower(args[1]) {
	case "stop":
		if cur == nil {
			return fail, nil
		}
		final := ""
		if len(args) > 2 {
			final = args[2]
		}
		cur.stop(final)
	case "pause", "resume":
		if cur == nil {
			return fail, nil
		}
		cur.setPaused(strings.EqualFold(args[1], "pause"))
	case "send_json":
		if cur == nil || len(args) < 3 || cur.sendBase64JSON(args[2]) != nil {
			return fail, nil
		}
	case "start":
		mix := strings.ToLower(args[3])
		if mix != "mono" && mix != "mixed" && mix != "stereo" {
			c.log.Error("uuid_openai_audio_stream: invalid mix type, must be mono, mixed, or stereo", "mix", mix)
			return fail, nil
		}
		rate := 8000
		if len(args) > 4 {
			switch strings.ToLower(args[4]) {
			case "8k":
				rate = 8000
			case "16k":
				rate = 16000
			case "24k":
				rate = 24000
			default:
				rate, _ = strconv.Atoi(args[4])
			}
		}
		if rate <= 0 || rate%8000 != 0 {
			c.log.Error("uuid_openai_audio_stream: invalid sample rate", "rate", rate)
			return fail, nil
		}
		if !strings.HasPrefix(args[2], "ws://") && !strings.HasPrefix(args[2], "wss://") {
			c.log.Error("uuid_openai_audio_stream: invalid websocket uri", "uri", args[2])
			return fail, nil
		}
		if err := startMedia(ch, false); err != nil {
			c.log.Error("uuid_openai_audio_stream: the call has no audio", "uuid", ch.UUID, "err", err)
			return fail, nil
		}
		if cur != nil {
			cur.stop("")
		}
		newOpenAIStream(c, ch, args[2], rate).start()
	default:
		c.log.Error("uuid_openai_audio_stream: unsupported command", "cmd", args[1])
		return fail, nil
	}
	return "+OK Success\n", nil
}

func loadOpenAIStream(ch *Channel) *openAIStream {
	if v, ok := openAIStreams.Load(ch); ok {
		return v.(*openAIStream)
	}
	return nil
}

type openAIStream struct {
	core *Core
	ch   *Channel
	url  string
	rate int // sample rate sent to the websocket

	ctx    context.Context
	cancel context.CancelFunc
	talk   *Talk
	once   sync.Once

	hdr         http.Header
	noReconnect bool
	heartbeat   time.Duration
	chunk       int // samples sent per websocket message (at rate)

	wmu  sync.Mutex
	conn *websocket.Conn // nil while not connected

	paused       atomic.Bool
	speaking     atomic.Bool
	audioDone    atomic.Bool
	transferring atomic.Bool
}

func newOpenAIStream(c *Core, ch *Channel, url string, rate int) *openAIStream {
	ctx, cancel := context.WithCancel(ch.ctx)
	s := &openAIStream{core: c, ch: ch, url: url, rate: rate, ctx: ctx, cancel: cancel}
	s.noReconnect = truthy(ch.Var("STREAM_NO_RECONNECT"))
	if n, err := strconv.Atoi(strings.TrimSpace(ch.Var("STREAM_HEART_BEAT"))); err == nil && n > 0 {
		s.heartbeat = time.Duration(n) * time.Second
	}
	packets := 1
	if n, err := strconv.Atoi(strings.TrimSpace(ch.Var("STREAM_BUFFER_SIZE"))); err == nil && n >= 20 && n%20 == 0 {
		packets = n / 20
	}
	s.chunk = rate / 50 * packets // 20 ms of audio per packet
	s.hdr = http.Header{}
	if key := ch.Var("STREAM_OPENAI_API_KEY"); key != "" {
		s.hdr.Set("Authorization", "Bearer "+key)
	} else {
		c.log.Error("uuid_openai_audio_stream: STREAM_OPENAI_API_KEY is not set, assuming STREAM_EXTRA_HEADERS", "uuid", ch.UUID)
		c.aiLog("ERR", ch.UUID, "STREAM_OPENAI_API_KEY is not set, assuming STREAM_EXTRA_HEADERS")
		var extra map[string]any
		if json.Unmarshal([]byte(ch.Var("STREAM_EXTRA_HEADERS")), &extra) == nil {
			for k, v := range extra {
				if sv, ok := v.(string); ok {
					s.hdr.Set(k, sv)
				}
			}
		}
	}
	return s
}

func (s *openAIStream) start() {
	s.talk = newTalk(s.ctx, s.ch)
	s.ch.setOAIMedia(true)
	openAIStreams.Store(s.ch, s)
	s.core.log.Info("openai audio stream: start", "uuid", s.ch.UUID, "rate", s.rate)
	s.core.aiLog("INFO", s.ch.UUID, "uuid_openai_audio_stream start")
	go s.run()
	go s.uplink()
	go s.speechWatcher()
}

// stop ends the stream; final is an optional base64 JSON sent first.
func (s *openAIStream) stop(final string) {
	s.once.Do(func() {
		if final != "" {
			_ = s.sendBase64JSON(final)
		}
		s.core.log.Info("openai audio stream: stop", "uuid", s.ch.UUID)
		s.cancel()
		s.wmu.Lock()
		if s.conn != nil {
			_ = s.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Normal closure"), time.Now().Add(time.Second))
			_ = s.conn.Close()
			s.conn = nil
		}
		s.wmu.Unlock()
		s.talk.close()
		s.ch.setOAIMedia(false)
		openAIStreams.CompareAndDelete(s.ch, s)
	})
}

func (s *openAIStream) setPaused(p bool) {
	s.paused.Store(p)
	if p {
		s.talk.Hush()
	}
}

func (s *openAIStream) fire(sub, body string) {
	e := NewEvent(EvCustom, oaiEventPrefix+sub)
	s.ch.eventHeaders(e, false)
	e.Body = body
	s.core.events.Publish(e)
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ---------------------------------------------------------------------------
// connection

func (s *openAIStream) tlsConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	ca := s.ch.Var("STREAM_TLS_CA_FILE")
	switch {
	case strings.EqualFold(ca, "NONE"):
		// The module accepts NONE to switch certificate checking off. We do not: the audio of a call and the API key
		// would go to whoever answers. The connection is verified with the system roots instead.
		s.ch.log.Warn("uuid_openai_audio_stream: STREAM_TLS_CA_FILE=NONE is not supported; the server certificate is verified")
	case ca != "" && !strings.EqualFold(ca, "SYSTEM"):
		if b, err := os.ReadFile(ca); err == nil {
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(b)
			cfg.RootCAs = pool
		}
	}
	if certFile, keyFile := s.ch.Var("STREAM_TLS_CERT_FILE"), s.ch.Var("STREAM_TLS_KEY_FILE"); certFile != "" && keyFile != "" {
		if pair, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
			cfg.Certificates = []tls.Certificate{pair}
		}
	}
	if truthy(s.ch.Var("STREAM_TLS_DISABLE_HOSTNAME_VALIDATION")) && !cfg.InsecureSkipVerify {
		// verify the chain, not the name
		roots := cfg.RootCAs
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			var certs []*x509.Certificate
			for _, r := range raw {
				c, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs = append(certs, c)
			}
			if len(certs) == 0 {
				return fmt.Errorf("no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range certs[1:] {
				inter.AddCert(c)
			}
			_, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
			return err
		}
	}
	return cfg
}

// run connects, reads until the connection drops, and (unless told not to)
// reconnects. A failed connection fires the error event and ends the stream,
// as the module closes its media bug then.
func (s *openAIStream) run() {
	defer s.stop("")
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second, TLSClientConfig: s.tlsConfig()}
	retries := 0
	for s.ctx.Err() == nil {
		conn, resp, err := dialer.DialContext(s.ctx, s.url, s.hdr)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			s.core.log.Warn("openai audio stream: connection failed", "uuid", s.ch.UUID, "err", err, "http_status", status)
			s.core.aiLog("INFO", s.ch.UUID, "connection error")
			s.fire("error", jsonString(map[string]any{"status": "error", "message": map[string]any{
				"retries": retries, "error": err.Error(), "wait_time": 100, "http_status": status}}))
			return
		}
		conn.SetReadLimit(32 << 20)
		s.wmu.Lock()
		s.conn = conn
		s.wmu.Unlock()
		if s.heartbeat > 0 {
			go s.pinger(conn)
		}
		s.opened()
		code, reason := s.readLoop(conn)
		s.wmu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		s.wmu.Unlock()
		_ = conn.Close()
		s.fire("disconnect", jsonString(map[string]any{"status": "disconnected", "message": map[string]any{"code": code, "reason": reason}}))
		s.core.aiLog("INFO", s.ch.UUID, "connection closed")
		if s.ctx.Err() != nil || s.noReconnect || code == websocket.CloseNormalClosure {
			return
		}
		retries++
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(time.Duration(min(retries, 10)) * 500 * time.Millisecond):
		}
	}
}

func (s *openAIStream) pinger(conn *websocket.Conn) {
	t := time.NewTicker(s.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.wmu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			s.wmu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// opened runs when the websocket is up: the connect event, the session
// settings and the greeting.
func (s *openAIStream) opened() {
	s.fire("connect", `{"status":"connected"}`)
	ch := s.ch
	session := map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input": map[string]any{
				"transcription":   map[string]any{"model": "gpt-4o-transcribe", "language": firstNonEmpty(ch.Var("STREAM_OPENAI_LANGUAGE"), "en")},
				"noise_reduction": map[string]any{"type": firstNonEmpty(ch.Var("STREAM_OPENAI_NOISE"), "near_field")},
			},
			"output": map[string]any{"voice": firstNonEmpty(ch.Var("STREAM_OPENAI_VOICE"), "sage"), "speed": 1.0},
		},
		"instructions": firstNonEmpty(ch.Var("STREAM_OPENAI_INSTRUCTIONS"), oaiDefaultInstructions),
	}
	_ = s.writeText(jsonString(map[string]any{"type": "session.update", "session": session}))
	_ = s.writeText(jsonString(map[string]any{"type": "response.create", "response": map[string]any{
		"instructions": firstNonEmpty(ch.Var("STREAM_OPENAI_GREETING"), oaiDefaultGreeting)}}))
}

func (s *openAIStream) writeText(text string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("not connected")
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.conn.WriteMessage(websocket.TextMessage, []byte(text))
}

func (s *openAIStream) sendBase64JSON(b64 string) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(b64)
	}
	if err != nil || len(raw) == 0 || !json.Valid(raw) {
		return fmt.Errorf("invalid json")
	}
	var compact map[string]any
	if json.Unmarshal(raw, &compact) == nil {
		return s.writeText(jsonString(compact))
	}
	return s.writeText(string(raw))
}

// readLoop handles what the websocket sends until it closes; it returns the
// close code and reason.
func (s *openAIStream) readLoop(conn *websocket.Conn) (int, string) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				return ce.Code, ce.Text
			}
			return websocket.CloseAbnormalClosure, err.Error()
		}
		s.handle(data)
	}
}

// ---------------------------------------------------------------------------
// caller -> OpenAI

// uplink sends what the caller says, resampled to the stream rate.
func (s *openAIStream) uplink() {
	buf := make([]int16, 0, s.chunk*2)
	for s.ctx.Err() == nil {
		f, err := s.talk.Listen()
		if err != nil {
			return
		}
		if s.paused.Load() {
			buf = buf[:0]
			continue
		}
		s.wmu.Lock()
		up := s.conn != nil
		s.wmu.Unlock()
		if !up {
			buf = buf[:0]
			continue
		}
		if s.rate != 8000 {
			f = media.Resample(f, 8000, s.rate)
		}
		buf = append(buf, f...)
		for len(buf) >= s.chunk {
			raw := make([]byte, s.chunk*2)
			for i, v := range buf[:s.chunk] {
				binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
			}
			buf = buf[s.chunk:]
			msg := `{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(raw) + `"}`
			if s.writeText(msg) != nil {
				buf = buf[:0]
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// OpenAI -> caller

func (s *openAIStream) handle(data []byte) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		s.core.log.Warn("openai audio stream: not json", "uuid", s.ch.UUID)
		return
	}
	typ, _ := m["type"].(string)
	handled := false
	switch typ {
	case "conversation.item.input_audio_transcription.completed":
		if t, _ := m["transcript"].(string); t != "" {
			s.core.log.Info("openai audio stream: caller said", "uuid", s.ch.UUID, "text", t)
			s.checkTransfer(t)
		}
	case "input_audio_buffer.speech_started":
		s.talk.Hush() // the caller spoke over the AI
		if s.speaking.Load() {
			s.audioDone.Store(true)
		}
		handled = true
	case "response.output_audio.delta", "response.audio.delta":
		if b64, _ := m["delta"].(string); b64 != "" {
			s.audioDone.Store(false)
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				s.core.log.Error("openai audio stream: bad audio", "uuid", s.ch.UUID, "err", err)
				return
			}
			if !s.paused.Load() {
				samples := make([]int16, len(raw)/2)
				for i := range samples {
					samples[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
				}
				if s.speaking.CompareAndSwap(false, true) {
					s.fire("openai_speech_start", `{"status":"started"}`)
				}
				s.talk.Say(media.Resample(samples, oaiOpenAIRate, 8000))
			}
			handled = true
		}
	case "response.output_audio.done", "response.audio.done":
		s.audioDone.Store(true)
	default:
		if strings.Contains(typ, "error") {
			s.core.log.Error("openai audio stream: error from OpenAI", "uuid", s.ch.UUID, "message", clipText(string(data), 300))
			s.core.aiLog("ERR", s.ch.UUID, "("+s.ch.UUID+") processMessage - error: "+string(data))
		}
	}
	if !handled {
		s.fire("json", string(data))
	}
}

// speechWatcher reports when the AI stopped talking: its last audio has been played.
func (s *openAIStream) speechWatcher() {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if s.speaking.Load() && s.audioDone.Load() && s.talk.Pending() == 0 {
			if s.speaking.CompareAndSwap(true, false) {
				s.fire("openai_speech_stop", `{"status":"stopped"}`)
			}
		}
	}
}

// checkTransfer looks for a keyword of STREAM_OPENAI_TRANSFER_MAP
// ("keyword:destination,keyword:destination") in what the caller said and
// transfers the call: after the AI has said its goodbye the stream stops and
// the call goes to the destination in the call's domain.
func (s *openAIStream) checkTransfer(transcript string) {
	spec := s.ch.Var("STREAM_OPENAI_TRANSFER_MAP")
	if spec == "" {
		return
	}
	said := strings.ToLower(transcript)
	for _, pair := range strings.Split(spec, ",") {
		kw, dest, ok := strings.Cut(pair, ":")
		kw, dest = strings.ToLower(strings.TrimSpace(kw)), strings.TrimSpace(dest)
		if !ok || kw == "" || dest == "" || !strings.Contains(said, kw) {
			continue
		}
		if !s.transferring.CompareAndSwap(false, true) {
			return
		}
		s.core.log.Info("openai audio stream: keyword matched, transferring", "uuid", s.ch.UUID, "keyword", kw, "to", dest)
		go func() {
			// 1 s, then 3 s for the AI to finish speaking, as transfer_ai_call.lua did
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
			s.stop("")
			time.Sleep(500 * time.Millisecond)
			if s.ch.IsHungUp() {
				return
			}
			// the destination is looked up in the call's domain (the dialplan context of a
			// RingQ tenant is its domain name); otherwise in the context the call is in
			ctxName := ""
			if d := s.ch.Var("domain_name"); d != "" && s.core.config().Dialplan.Contexts[d] != nil {
				ctxName = d
			}
			s.core.transfer(s.ch, dest, ctxName)
		}()
		return
	}
}
