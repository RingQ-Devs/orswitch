package core

import (
	"context"
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
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/ringq-devs/orswitch/internal/media"
)

// uuid_audio_stream: RingQswitch's version of the FreeSWITCH module mod_audio_stream
// (amigniter's, as built on the RingQ servers). A *tap*: it copies what the call says
// to a websocket and never takes over the call's audio. RingQ uses it for
//
//	live transcription / Agent Coach   luarun live_transcribe.lua  -> ws://127.0.0.1:5000/?uuid=..&from=..&to=..&domain=.. stereo 24000
//	the AI IVR hooks                   ai_ivr_recording2/3 (node/server.js) -> ws://127.0.0.1:7777|3333 mono 16k <uuid>
//	the legacy AI receptionist route   ws://127.0.0.1:8081 mono 8k <uuid|caller|ext|domain>   (the bridge plays its voice with uuid_broadcast)
//
// The command, exactly as the module has it:
//
//	uuid_audio_stream <uuid> start <ws-url> <mono|mixed|stereo> [8k|16k|24k|<rate>] [metadata]
//	uuid_audio_stream <uuid> stop [final text]
//	uuid_audio_stream <uuid> pause | resume
//	uuid_audio_stream <uuid> send_text <text>
//
// Wire format, as the module sends it:
//   - the metadata argument, when given, is sent as the first TEXT frame as soon as the
//     websocket is open (the AI bridge reads the call's uuid from it), then
//     CUSTOM mod_audio_stream::connect {"status":"connected"} is fired
//   - audio goes up as BINARY frames of raw L16 little-endian, 20 ms per frame (or
//     STREAM_BUFFER_SIZE ms, a multiple of 20). mono = what the leg says; mixed = both
//     directions summed; stereo = interleaved, left = what the leg says (the caller on
//     an inbound call), right = what the leg hears (the other party)
//   - a websocket TEXT message {"type":"streamAudio","data":{"audioDataType":"raw|wav|mp3|ogg|pcmu|pcma",
//     "sampleRate":8000..,"audioData":"<base64>"}} is written to <tmp>/<uuid>_<n>.tmp<.r8|.wav...> and
//     announced as CUSTOM mod_audio_stream::play with the data object plus "file" (the module
//     itself plays nothing; the AI bridge uses uuid_broadcast), every other message is
//     CUSTOM mod_audio_stream::json with the message as the body
//   - a refused connection fires mod_audio_stream::error and ends the stream; a closed one
//     fires mod_audio_stream::disconnect (the stream stays until stop or hangup, nothing
//     reconnects)
//
// uuid_openai_audio_stream <uuid> start <ws> stereo|mixed ... is the same tap, in
// mod_openai_audio_stream's framing (TEXT frames {"type":"input_audio_buffer.append","audio":
// "<base64>"}); live transcription (node/transcript_live.js, port 5000) accepts both.
// Differences from the module: the right channel only carries the other party of a bridge
// (audio the switch itself plays to the leg - prompts, music - is not in it), and only legs
// that use G.711 can be heard (media.Endpoint decodes nothing else).

const audioStreamSyntax = "<uuid> [start | stop | send_text | pause | resume | graceful-shutdown ] [wss-url | path] [mono | mixed | stereo] [8000 | 16000] [metadata]"

const (
	audioStreamEventPrefix = "mod_audio_stream::"
	audioStreamMaxMeta     = 8192
	audioStreamMaxURI      = 4096
	tapQueueDepth          = 6 // frames kept per side while waiting for the pacing tick
)

func init() {
	apiCommands["uuid_audio_stream"] = apiCommand{"uuid_audio_stream " + audioStreamSyntax, "tap call audio to a websocket (live transcription, AI IVR)", apiAudioStream}
}

type tapFlavor int

const (
	tapAudioStream tapFlavor = iota // mod_audio_stream: binary frames, metadata frame, play/json events
	tapOpenAI                       // mod_openai_audio_stream in stereo/mixed: JSON text frames
)

type tapKey struct {
	ch     *Channel
	flavor tapFlavor
}

var audioTaps sync.Map // tapKey -> *audioTap

func loadAudioTap(ch *Channel, f tapFlavor) *audioTap {
	if v, ok := audioTaps.Load(tapKey{ch, f}); ok {
		return v.(*audioTap)
	}
	return nil
}

// validStreamURI is the module's validate_ws_uri: ws:// or wss://, a host of letters, digits, '-'
// and '.', an optional numeric port.
func validStreamURI(u string) bool {
	var rest string
	switch {
	case strings.HasPrefix(u, "ws://"):
		rest = u[5:]
	case strings.HasPrefix(u, "wss://"):
		rest = u[6:]
	default:
		return false
	}
	if len(u) >= audioStreamMaxURI {
		return false
	}
	i := 0
	for i < len(rest) && rest[i] != ':' && rest[i] != '/' {
		c := rest[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '.') {
			return false
		}
		i++
	}
	if i == 0 {
		return false
	}
	if i < len(rest) && rest[i] == ':' {
		for j := i + 1; j < len(rest) && rest[j] != '/'; j++ {
			if rest[j] < '0' || rest[j] > '9' {
				return false
			}
		}
	}
	return true
}

// parseStreamRate reads the rate argument: 8k, 16k, 24k or a number of Hz (default 8000).
func parseStreamRate(arg string, given bool) (int, bool) {
	rate := 8000
	if given {
		switch strings.ToLower(arg) {
		case "8k":
			rate = 8000
		case "16k":
			rate = 16000
		case "24k":
			rate = 24000
		default:
			rate, _ = strconv.Atoi(arg)
		}
	}
	return rate, rate > 0 && rate%8000 == 0
}

func apiAudioStream(c *Core, args []string) (string, error) {
	const fail = "-ERR Operation Failed\n"
	if len(args) < 2 || (strings.EqualFold(args[1], "start") && len(args) < 4) {
		c.log.Error("uuid_audio_stream: bad command", "args", strings.Join(args, " "))
		return "-USAGE: " + audioStreamSyntax + "\n", nil
	}
	ch := c.Channel(args[0])
	if ch == nil {
		c.log.Error("uuid_audio_stream: error locating session", "uuid", args[0])
		return fail, nil
	}
	cur := loadAudioTap(ch, tapAudioStream)
	switch strings.ToLower(args[1]) {
	case "stop":
		final := ""
		if len(args) > 2 {
			if !utf8.ValidString(args[2]) {
				ch.log.Error("uuid_audio_stream: stop text contains invalid utf8 characters")
				return fail, nil
			}
			final = args[2]
		}
		if cur == nil {
			ch.log.Debug("uuid_audio_stream: stop: no stream - websocket connection already closed")
			return fail, nil
		}
		cur.stop(final)
	case "pause", "resume":
		if cur == nil {
			ch.log.Error("uuid_audio_stream: pause/resume failed because no stream")
			return fail, nil
		}
		cur.setPaused(strings.EqualFold(args[1], "pause"))
	case "send_text":
		if len(args) < 3 {
			ch.log.Error("uuid_audio_stream: send_text requires an argument specifying text to send")
			return fail, nil
		}
		text := strings.Join(args[2:], " ")
		if !utf8.ValidString(text) {
			ch.log.Error("uuid_audio_stream: send_text text contains invalid utf8 characters")
			return fail, nil
		}
		if cur == nil {
			ch.log.Error("uuid_audio_stream: no stream, failed sending text")
			return fail, nil
		}
		cur.writeText(text)
	case "start":
		if cur != nil {
			ch.log.Error("uuid_audio_stream: stream already attached!")
			return fail, nil
		}
		metadata := ""
		if len(args) > 5 {
			metadata = strings.Join(args[5:], " ")
			if !utf8.ValidString(metadata) {
				ch.log.Error("uuid_audio_stream: metadata contains invalid utf8 characters")
				return fail, nil
			}
			if len(metadata) > audioStreamMaxMeta {
				metadata = metadata[:audioStreamMaxMeta]
			}
		}
		rateArg := ""
		if len(args) > 4 {
			rateArg = args[4]
		}
		out, ok := startAudioTap(c, ch, tapAudioStream, args[2], args[3], rateArg, len(args) > 4, metadata)
		if !ok {
			return fail, nil
		}
		return out, nil
	default:
		// graceful-shutdown is in the usage text of the module but not implemented there either
		ch.log.Error("uuid_audio_stream: unsupported command", "cmd", args[1])
		return fail, nil
	}
	return "+OK Success\n", nil
}

// startAudioTap validates a start request and starts the tap.
func startAudioTap(c *Core, ch *Channel, flavor tapFlavor, uri, mix, rateArg string, rateGiven bool, metadata string) (string, bool) {
	name := "uuid_audio_stream"
	if flavor == tapOpenAI {
		name = "uuid_openai_audio_stream"
	}
	mix = strings.ToLower(mix)
	if mix != "mono" && mix != "mixed" && mix != "stereo" {
		ch.log.Error(name+": invalid mix type, must be mono, mixed, or stereo", "mix", mix)
		return "", false
	}
	rate, ok := parseStreamRate(rateArg, rateGiven)
	if !ok {
		ch.log.Error(name+": invalid sample rate", "rate", rateArg)
		return "", false
	}
	if !validStreamURI(uri) {
		ch.log.Error(name+": invalid websocket uri", "uri", uri)
		return "", false
	}
	// the module needs the call to be at least in early media
	if !ch.IsAnswered() {
		if err := ch.AnswerLocal(true); err != nil {
			ch.log.Error(name+": channel must have reached pre-answer status before calling start", "err", err)
			return "", false
		}
	}
	if ch.ep == nil || ch.IsHungUp() {
		ch.log.Error(name + ": the call has no audio")
		return "", false
	}
	t := newAudioTap(c, ch, flavor, uri, mix, rate, metadata)
	if _, loaded := audioTaps.LoadOrStore(tapKey{ch, flavor}, t); loaded {
		ch.log.Error(name + ": stream already attached!")
		return "", false
	}
	t.start()
	return "+OK Success\n", true
}

// openAITapAPI is uuid_openai_audio_stream's hook for the tap (stereo and mixed
// streams). handled=false: the command is not about a tap and the caller goes on.
func openAITapAPI(c *Core, ch *Channel, args []string, cur *openAIStream) (out string, handled bool) {
	const fail = "-ERR Operation Failed\n"
	tap := loadAudioTap(ch, tapOpenAI)
	switch strings.ToLower(args[1]) {
	case "start":
		mix := strings.ToLower(args[3])
		if mix != "stereo" && mix != "mixed" {
			if tap != nil {
				ch.log.Error("uuid_openai_audio_stream: bug already attached!")
				return fail, true
			}
			return "", false
		}
		if tap != nil || cur != nil {
			// one stream per call, as the module has it: a live-transcript start on an AI call fails harmlessly
			ch.log.Error("uuid_openai_audio_stream: bug already attached!")
			return fail, true
		}
		rateArg := ""
		if len(args) > 4 {
			rateArg = args[4]
		}
		res, ok := startAudioTap(c, ch, tapOpenAI, args[2], args[3], rateArg, len(args) > 4, "")
		if !ok {
			return fail, true
		}
		return res, true
	case "stop":
		if tap == nil {
			return "", false
		}
		final := ""
		if len(args) > 2 {
			final = args[2]
		}
		tap.stop(final)
		return "+OK Success\n", true
	case "pause", "resume":
		if tap == nil {
			return "", false
		}
		tap.setPaused(strings.EqualFold(args[1], "pause"))
		return "+OK Success\n", true
	case "send_json":
		if tap == nil {
			return "", false
		}
		if len(args) < 3 || tap.sendBase64JSON(args[2]) != nil {
			return fail, true
		}
		return "+OK Success\n", true
	}
	return "", false
}

// ---------------------------------------------------------------------------

// tapSide holds the few frames of one direction waiting for the next pacing tick.
type tapSide struct {
	mu sync.Mutex
	q  [][]int16
}

func (s *tapSide) offer(f []int16) {
	s.mu.Lock()
	s.q = append(s.q, f)
	if len(s.q) > tapQueueDepth {
		s.q = s.q[len(s.q)-tapQueueDepth:] // keep latency bounded: drop the oldest
	}
	s.mu.Unlock()
}

func (s *tapSide) take() []int16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.q) == 0 {
		return nil
	}
	f := s.q[0]
	s.q = s.q[1:]
	return f
}

func (s *tapSide) clear() {
	s.mu.Lock()
	s.q = nil
	s.mu.Unlock()
}

type audioTap struct {
	core   *Core
	ch     *Channel
	flavor tapFlavor
	url    string
	mix    string
	rate   int
	meta   string

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	hdr         http.Header
	noReconnect bool
	heartbeat   time.Duration
	packets     int // 20 ms packets per websocket message
	quiet       bool
	noFiles     bool // STREAM_DISABLE_AUDIOFILES: streamAudio messages are not turned into files (no play event)

	wmu  sync.Mutex
	conn *websocket.Conn // nil while not connected

	paused atomic.Bool

	left  tapSide // what the leg says
	right tapSide // what the leg hears (the partner's voice)

	// the partner whose voice is the right channel
	rmu       sync.Mutex
	rightCh   *Channel
	rightStop func()

	// audio files written for streamAudio messages, removed at the end
	fmu   sync.Mutex
	files []string
	nfile int
}

func newAudioTap(c *Core, ch *Channel, flavor tapFlavor, url, mix string, rate int, meta string) *audioTap {
	ctx, cancel := context.WithCancel(ch.ctx)
	t := &audioTap{core: c, ch: ch, flavor: flavor, url: url, mix: mix, rate: rate, meta: meta, ctx: ctx, cancel: cancel, packets: 1}
	t.noReconnect = truthy(ch.Var("STREAM_NO_RECONNECT"))
	t.quiet = truthy(ch.Var("STREAM_SUPPRESS_LOG"))
	t.noFiles = truthy(ch.Var("STREAM_DISABLE_AUDIOFILES")) // RingQ's AI dialplans set it
	if n, err := strconv.Atoi(strings.TrimSpace(ch.Var("STREAM_HEART_BEAT"))); err == nil && n > 0 {
		t.heartbeat = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(strings.TrimSpace(ch.Var("STREAM_BUFFER_SIZE"))); err == nil && n >= 20 && n%20 == 0 {
		t.packets = n / 20
	}
	t.hdr = http.Header{}
	extra := func() {
		var m map[string]any
		if json.Unmarshal([]byte(ch.Var("STREAM_EXTRA_HEADERS")), &m) == nil {
			for k, v := range m {
				if sv, ok := v.(string); ok {
					t.hdr.Set(k, sv)
				}
			}
		}
	}
	if flavor == tapOpenAI {
		if key := ch.Var("STREAM_OPENAI_API_KEY"); key != "" {
			t.hdr.Set("Authorization", "Bearer "+key)
		} else {
			extra()
		}
	} else {
		extra()
	}
	return t
}

func (t *audioTap) name() string {
	if t.flavor == tapOpenAI {
		return "uuid_openai_audio_stream"
	}
	return "uuid_audio_stream"
}

func (t *audioTap) eventPrefix() string {
	if t.flavor == tapOpenAI {
		return oaiEventPrefix
	}
	return audioStreamEventPrefix
}

func (t *audioTap) start() {
	t.ch.log.Info(t.name()+": start", "mix", t.mix, "rate", t.rate)
	// the left channel: what the leg says
	if frames, stop, err := t.core.Hear(t.ch.UUID); err == nil {
		go func() {
			defer stop()
			for {
				select {
				case f, ok := <-frames:
					if !ok {
						return
					}
					t.left.offer(f)
				case <-t.ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		<-t.ctx.Done()
		t.stop("")
	}()
	go t.run()
	go t.pump()
}

// stop ends the stream: the final text is sent first (stop command), then the
// websocket is closed.
func (t *audioTap) stop(final string) {
	t.once.Do(func() {
		if final != "" {
			t.writeText(final)
		}
		t.ch.log.Info(t.name() + ": stop")
		t.cancel()
		t.wmu.Lock()
		if t.conn != nil {
			_ = t.conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Normal closure"), time.Now().Add(time.Second))
			_ = t.conn.Close()
			t.conn = nil
		}
		t.wmu.Unlock()
		t.setRight(nil)
		audioTaps.CompareAndDelete(tapKey{t.ch, t.flavor}, t)
		t.deleteFiles()
	})
}

func (t *audioTap) setPaused(p bool) {
	t.paused.Store(p)
	if p {
		t.left.clear()
		t.right.clear()
	}
	t.ch.log.Info(t.name() + ": " + map[bool]string{true: "pause", false: "resume"}[p])
}

func (t *audioTap) fire(sub, body string) {
	e := NewEvent(EvCustom, t.eventPrefix()+sub)
	t.ch.eventHeaders(e, true)
	e.Body = body
	t.core.events.Publish(e)
}

func (t *audioTap) writeText(text string) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.conn == nil {
		return
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = t.conn.WriteMessage(websocket.TextMessage, []byte(text))
}

func (t *audioTap) writeFrame(kind int, data []byte) bool {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.conn == nil {
		return false
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return t.conn.WriteMessage(kind, data) == nil
}

func (t *audioTap) sendBase64JSON(b64 string) error {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(b64)
	}
	if err != nil || len(raw) == 0 || !json.Valid(raw) {
		return fmt.Errorf("invalid json")
	}
	t.writeText(string(raw))
	return nil
}

// ---------------------------------------------------------------------------
// connection

func (t *audioTap) run() {
	defer t.stop("")
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second,
		TLSClientConfig: (&openAIStream{ch: t.ch}).tlsConfig()}
	retries := 0
	for t.ctx.Err() == nil {
		conn, resp, err := dialer.DialContext(t.ctx, t.url, t.hdr)
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.ch.log.Warn(t.name()+": connection failed", "err", err, "http_status", status)
			if t.flavor == tapOpenAI {
				t.fire("error", jsonString(map[string]any{"status": "error", "message": map[string]any{
					"retries": retries, "error": err.Error(), "wait_time": 100, "http_status": status}}))
			} else {
				t.fire("error", jsonString(map[string]any{"status": "error", "message": map[string]any{
					"code": status, "error": err.Error()}}))
			}
			return // the module closes its media bug when the connection fails
		}
		conn.SetReadLimit(32 << 20)
		t.wmu.Lock()
		t.conn = conn
		t.wmu.Unlock()
		if t.heartbeat > 0 {
			go t.pinger(conn)
		}
		if t.meta != "" {
			t.writeText(t.meta) // the first frame: the call's metadata
		}
		t.fire("connect", `{"status":"connected"}`)
		code, reason := t.readLoop(conn)
		t.wmu.Lock()
		if t.conn == conn {
			t.conn = nil
		}
		t.wmu.Unlock()
		_ = conn.Close()
		if t.ctx.Err() != nil {
			return
		}
		t.ch.log.Info(t.name() + ": connection closed")
		t.fire("disconnect", jsonString(map[string]any{"status": "disconnected", "message": map[string]any{"code": code, "reason": reason}}))
		if t.flavor == tapAudioStream {
			// mod_audio_stream does not reconnect: the stream stays until stop or hangup and sends nothing
			<-t.ctx.Done()
			return
		}
		if t.noReconnect || code == websocket.CloseNormalClosure {
			return
		}
		retries++
		select {
		case <-t.ctx.Done():
			return
		case <-time.After(time.Duration(min(retries, 10)) * 500 * time.Millisecond):
		}
	}
}

func (t *audioTap) pinger(conn *websocket.Conn) {
	tk := time.NewTicker(t.heartbeat)
	defer tk.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tk.C:
			t.wmu.Lock()
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			t.wmu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (t *audioTap) readLoop(conn *websocket.Conn) (int, string) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ce, ok := err.(*websocket.CloseError); ok {
				return ce.Code, ce.Text
			}
			return websocket.CloseAbnormalClosure, err.Error()
		}
		t.handle(data)
	}
}

// handle is the module's message callback.
func (t *audioTap) handle(data []byte) {
	msg := string(data)
	if t.flavor == tapAudioStream && !t.noFiles {
		if rewritten, ok := t.processStreamAudio(data); ok {
			t.fire("play", rewritten)
			if !t.quiet {
				t.ch.log.Debug(t.name()+": response", "msg", clipText(rewritten, 300))
			}
			return
		}
	}
	t.fire("json", msg)
	if !t.quiet {
		t.ch.log.Debug(t.name()+": response", "msg", clipText(msg, 300))
	}
}

// processStreamAudio is the module's processMessage: a "streamAudio" message is decoded to a
// file; the data object, with the file's path, is what the play event carries.
func (t *audioTap) processStreamAudio(data []byte) (string, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return "", false
	}
	var typ string
	if json.Unmarshal(root["type"], &typ) != nil || typ != "streamAudio" {
		return "", false
	}
	logErr := func(s string) { t.ch.log.Error(t.name() + ": (" + t.ch.UUID + ") processMessage - " + s) }
	rawData, ok := root["data"]
	if !ok {
		logErr("no data in streamAudio")
		return "", false
	}
	var obj map[string]any
	if json.Unmarshal(rawData, &obj) != nil {
		logErr("no data in streamAudio")
		return "", false
	}
	audio, present := obj["audioData"]
	if !present {
		logErr("streamAudio missing 'audioData' field")
		return "", false
	}
	b64, isStr := audio.(string)
	if !isStr {
		logErr("'audioData' is not a string (expected base64 string)")
		return "", false
	}
	delete(obj, "audioData")
	kind, _ := obj["audioDataType"].(string)
	sampleRate := 0
	if f, ok := obj["sampleRate"].(float64); ok {
		sampleRate = int(f)
	}
	var ext string
	switch kind {
	case "raw":
		switch sampleRate {
		case 8000:
			ext = ".r8"
		case 16000:
			ext = ".r16"
		case 24000:
			ext = ".r24"
		case 32000:
			ext = ".r32"
		case 48000:
			ext = ".r48"
		case 64000:
			ext = ".r64"
		default:
			logErr("unsupported sample rate: " + strconv.Itoa(sampleRate))
			return "", false
		}
	case "wav":
		ext = ".wav"
	case "mp3":
		ext = ".mp3"
	case "ogg":
		ext = ".ogg"
	case "pcmu":
		ext = ".pcmu"
	case "pcma":
		ext = ".pcma"
	default:
		logErr("unsupported audio type: " + kind)
		return "", false
	}
	pcm, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if pcm, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			logErr("base64 decode error: " + err.Error())
			return "", false
		}
	}
	// The file is made by os.CreateTemp in the temp directory (mode 0600, a random name): nothing a websocket peer
	// sends (the audio type only picks one of the constant extensions above) is part of the path. The play event
	// carries the path, which is all RingQ's AI server uses.
	f, err := os.CreateTemp("", "rq-audio-*.tmp"+ext)
	if err != nil {
		logErr("failed creating the audio file: " + err.Error())
		return "", false
	}
	path := f.Name()
	_, werr := f.Write(pcm)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		logErr("failed writing file: " + path)
		return "", false
	}
	t.fmu.Lock()
	t.files = append(t.files, path)
	t.fmu.Unlock()
	obj["file"] = path
	out, err := json.Marshal(obj)
	if err != nil {
		logErr("cJSON_PrintUnformatted failed")
		return "", false
	}
	return string(out), true
}

func (t *audioTap) deleteFiles() {
	t.fmu.Lock()
	files := t.files
	t.files = nil
	t.fmu.Unlock()
	for _, f := range files {
		_ = os.Remove(f)
	}
}

// ---------------------------------------------------------------------------
// the audio

// setRight listens to ch's partner (nil: nobody).
func (t *audioTap) setRight(p *Channel) {
	t.rmu.Lock()
	defer t.rmu.Unlock()
	if t.rightCh == p {
		return
	}
	if t.rightStop != nil {
		t.rightStop()
		t.rightStop, t.rightCh = nil, nil
		t.right.clear()
	}
	if p == nil {
		return
	}
	frames, stop, err := t.core.Hear(p.UUID)
	if err != nil {
		return
	}
	t.rightCh, t.rightStop = p, stop
	go func() {
		for f := range frames {
			t.right.offer(f)
		}
	}()
}

// pump sends one packet per 20 ms tick that has audio: the leg's voice, the partner's, or both.
func (t *audioTap) pump() {
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	var acc []int16
	n := 0
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tk.C:
		}
		if t.mix != "mono" {
			p := t.ch.Partner()
			if p != nil && p.IsHungUp() {
				p = nil
			}
			t.setRight(p)
		}
		a := t.left.take()
		var b []int16
		if t.mix != "mono" {
			b = t.right.take()
		}
		if t.paused.Load() {
			continue // a paused stream reads and drops
		}
		if a == nil && b == nil {
			continue
		}
		t.wmu.Lock()
		up := t.conn != nil
		t.wmu.Unlock()
		if !up {
			acc, n = acc[:0], 0
			continue
		}
		acc = append(acc, t.packet(a, b)...)
		n++
		if n >= t.packets {
			t.send(acc)
			acc, n = acc[:0], 0
		}
	}
}

// packet makes the samples of one tick for the stream's mix and rate.
func (t *audioTap) packet(a, b []int16) []int16 {
	if t.rate != 8000 {
		a, b = media.Resample(a, 8000, t.rate), media.Resample(b, 8000, t.rate)
	}
	switch t.mix {
	case "mono":
		return a
	case "mixed":
		n := max(len(a), len(b))
		out := make([]int16, n)
		for i := range out {
			var s int32
			if i < len(a) {
				s += int32(a[i])
			}
			if i < len(b) {
				s += int32(b[i])
			}
			out[i] = int16(max(-32768, min(32767, s)))
		}
		return out
	}
	n := max(len(a), len(b))
	out := make([]int16, 2*n)
	for i := 0; i < n; i++ {
		if i < len(a) {
			out[2*i] = a[i]
		}
		if i < len(b) {
			out[2*i+1] = b[i]
		}
	}
	return out
}

func (t *audioTap) send(samples []int16) {
	raw := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(raw[i*2:], uint16(v))
	}
	if t.flavor == tapOpenAI {
		t.writeFrame(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"`+base64.StdEncoding.EncodeToString(raw)+`"}`))
		return
	}
	t.writeFrame(websocket.BinaryMessage, raw)
}
