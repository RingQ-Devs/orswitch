package core_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// fakeStream is a websocket server standing in for the AI bridge / transcript_live.js.
type fakeStream struct {
	srv   *httptest.Server
	url   string // ws://127.0.0.1:port
	msgs  chan wsMsg
	reqs  chan *http.Request
	send  chan wsMsg
	close chan struct{}
	mu    sync.Mutex
}

type wsMsg struct {
	binary bool
	data   []byte
}

func newFakeStream(t *testing.T) *fakeStream {
	t.Helper()
	f := &fakeStream{msgs: make(chan wsMsg, 4096), reqs: make(chan *http.Request, 8),
		send: make(chan wsMsg, 16), close: make(chan struct{}, 1)}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		f.reqs <- r
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			for {
				select {
				case m := <-f.send:
					kind := websocket.TextMessage
					if m.binary {
						kind = websocket.BinaryMessage
					}
					_ = c.WriteMessage(kind, m.data)
				case <-f.close:
					_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1001, "going away"), time.Now().Add(time.Second))
					return
				case <-stop:
					return
				}
			}
		}()
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			f.msgs <- wsMsg{binary: mt == websocket.BinaryMessage, data: data}
		}
	}))
	t.Cleanup(f.srv.Close)
	f.url = "ws" + strings.TrimPrefix(f.srv.URL, "http")
	return f
}

func (f *fakeStream) next(t *testing.T, what string, d time.Duration) wsMsg {
	t.Helper()
	select {
	case m := <-f.msgs:
		return m
	case <-time.After(d):
		t.Fatalf("no %s from the stream", what)
		return wsMsg{}
	}
}

// drain throws away what is queued.
func (f *fakeStream) drain() {
	for {
		select {
		case <-f.msgs:
		default:
			return
		}
	}
}

func waitSub(t *testing.T, sub *core.Subscription, subclass string, d time.Duration) *core.Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case e := <-sub.C:
			if e.Subclass() == subclass {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s event", subclass)
		}
	}
}

func pcmOf(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

func avg(s []int16) int {
	if len(s) == 0 {
		return 0
	}
	sum := 0
	for _, v := range s {
		sum += int(v)
	}
	return sum / len(s)
}

func aiDialplan(extra string) string {
	return strings.Replace(testDialplan, `</context></include>`, extra+`
</context></include>`, 1)
}

func twoUsers() string {
	users := "<include>"
	for _, u := range []string{"1000", "1001"} {
		users += `<user id="` + u + `"><params><param name="password" value="` + testPassword + `"/></params></user>`
	}
	return users + "</include>"
}

// uuid_audio_stream: argument checks, the metadata frame, binary L16 frames up, streamAudio and
// other JSON down (play / json events), pause, resume, send_text, stop with a final text,
// a refused connection and a closed one.
func TestAudioStreamCommand(t *testing.T) {
	dp := aiDialplan(`
  <extension name="ai"><condition field="destination_number" expression="^9300$">
    <action application="answer"/>
    <action application="playback" data="silence_stream://-1"/>
  </condition></extension>`)
	h := newHarnessWith(t, "", twoUsers(), dp, nil)
	sub := h.sw.Events().Subscribe(256, func(e *core.Event) bool { return strings.HasPrefix(e.Subclass(), "mod_audio_stream::") })
	defer sub.Close()
	fs := newFakeStream(t)

	alice := h.phone("1000")
	call, err := alice.Dial("9300", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", err)
	}
	var uuid string
	h.waitFor("the call's channel", func() bool {
		for _, ch := range h.sw.Channels() {
			if ch.Var("destination_number") == "9300" {
				uuid = ch.UUID
			}
		}
		return uuid != ""
	})
	api := func(cmd string) string { return h.sw.API("uuid_audio_stream " + cmd) }
	want := func(got, prefix string) {
		t.Helper()
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("expected %q, got %q", prefix, got)
		}
	}

	// argument checks, as the module has them
	want(h.sw.API("uuid_audio_stream"), "-USAGE: <uuid> [start | stop | send_text")
	want(api(uuid), "-USAGE:")
	want(api(uuid+" start ws://127.0.0.1:1"), "-USAGE:")
	want(api("no-such-uuid start ws://127.0.0.1:1 mono 8k"), "-ERR Operation Failed")
	want(api(uuid+" start http://127.0.0.1:1 mono 8k"), "-ERR Operation Failed")
	want(api(uuid+" start ws://bad_host:1 mono 8k"), "-ERR Operation Failed")
	want(api(uuid+" start ws://127.0.0.1:x mono 8k"), "-ERR Operation Failed")
	want(api(uuid+" start ws://127.0.0.1:1 quad 8k"), "-ERR Operation Failed")
	want(api(uuid+" start ws://127.0.0.1:1 mono 7000"), "-ERR Operation Failed")
	want(api(uuid+" graceful-shutdown"), "-ERR Operation Failed")
	want(api(uuid+" stop"), "-ERR Operation Failed")
	want(api(uuid+" pause"), "-ERR Operation Failed")
	want(api(uuid+" send_text hi"), "-ERR Operation Failed")

	// start: the metadata is the first frame, then the connect event
	meta := `{"uuid":"` + uuid + `","callerNumber":"1000"}`
	want(api(uuid+" start "+fs.url+"/rt mono 16k "+meta), "+OK Success")
	want(api(uuid+" start "+fs.url+"/rt mono 16k"), "-ERR Operation Failed") // one stream per call
	if m := fs.next(t, "metadata", 5*time.Second); m.binary || string(m.data) != meta {
		t.Fatalf("the first frame should be the metadata, got binary=%v %q", m.binary, m.data)
	}
	ev := waitSub(t, sub, "mod_audio_stream::connect", 3*time.Second)
	if ev.Body != `{"status":"connected"}` || ev.Get("Unique-ID") != uuid {
		t.Fatalf("connect event: %q %q", ev.Body, ev.Get("Unique-ID"))
	}
	if r := <-fs.reqs; r.URL.Path != "/rt" {
		t.Fatalf("path %q", r.URL.Path)
	}

	// caller -> websocket: 20 ms at 16 kHz is 640 bytes, binary, and the call is not taken over
	payload := ulaw(3000, 160)
	var frame *wsMsg
	for i := 0; i < 50 && frame == nil; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i), uint32(i*160), payload)
		select {
		case m := <-fs.msgs:
			frame = &m
		case <-time.After(30 * time.Millisecond):
		}
	}
	if frame == nil {
		t.Fatal("caller audio never reached the websocket")
	}
	if !frame.binary || len(frame.data) != 640 {
		t.Fatalf("expected a 640 byte binary frame, got binary=%v len=%d", frame.binary, len(frame.data))
	}
	if a := avg(pcmOf(frame.data)); a < 2000 || a > 4000 {
		t.Fatalf("the audio is not the caller's (avg %d)", a)
	}

	// websocket -> events: streamAudio is written to a file and announced; anything else is json
	raw := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	fs.send <- wsMsg{data: []byte(`{"type":"streamAudio","data":{"audioDataType":"raw","sampleRate":16000,"audioData":"` + base64.StdEncoding.EncodeToString(raw) + `"}}`)}
	play := waitSub(t, sub, "mod_audio_stream::play", 3*time.Second)
	var file string
	if i := strings.Index(play.Body, `"file":"`); i >= 0 {
		file = play.Body[i+8:]
		file = file[:strings.Index(file, `"`)]
	}
	if file == "" || !strings.HasSuffix(file, ".tmp.r16") || !strings.HasPrefix(filepath.Base(file), "rq-audio-") ||
		filepath.Dir(file) != filepath.Clean(os.TempDir()) ||
		!strings.Contains(play.Body, `"audioDataType":"raw"`) || strings.Contains(play.Body, "audioData\"") {
		t.Fatalf("play event: %s", play.Body)
	}
	if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("the audio file: %v %v", got, err)
	}
	fs.send <- wsMsg{data: []byte(`{"type":"transcript","text":"hello"}`)}
	if j := waitSub(t, sub, "mod_audio_stream::json", 3*time.Second); j.Body != `{"type":"transcript","text":"hello"}` {
		t.Fatalf("json event: %q", j.Body)
	}
	fs.send <- wsMsg{data: []byte(`{"type":"streamAudio","data":{"audioDataType":"raw","sampleRate":1234,"audioData":"AAAA"}}`)}
	if j := waitSub(t, sub, "mod_audio_stream::json", 3*time.Second); !strings.Contains(j.Body, "streamAudio") {
		t.Fatalf("an unusable streamAudio should come as json: %q", j.Body)
	}

	// pause drops audio, resume brings it back
	want(api(uuid+" pause"), "+OK Success")
	time.Sleep(100 * time.Millisecond)
	fs.drain()
	for i := 0; i < 15; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(100+i), uint32((100+i)*160), payload)
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case m := <-fs.msgs:
		t.Fatalf("a paused stream sent %d bytes", len(m.data))
	case <-time.After(150 * time.Millisecond):
	}
	want(api(uuid+" resume"), "+OK Success")
	got := false
	for i := 0; i < 30 && !got; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(200+i), uint32((200+i)*160), payload)
		select {
		case m := <-fs.msgs:
			got = m.binary
		case <-time.After(30 * time.Millisecond):
		}
	}
	if !got {
		t.Fatal("no audio after resume")
	}

	// send_text, and stop with a final text, which closes the websocket
	want(api(uuid+" send_text hello-bridge"), "+OK Success")
	for deadline := time.Now().Add(3 * time.Second); ; {
		m := fs.next(t, "send_text", time.Until(deadline))
		if !m.binary && string(m.data) == "hello-bridge" {
			break
		}
	}
	want(api(uuid+" stop bye"), "+OK Success")
	for deadline := time.Now().Add(3 * time.Second); ; {
		m := fs.next(t, "the final text", time.Until(deadline))
		if !m.binary && string(m.data) == "bye" {
			break
		}
	}
	want(api(uuid+" stop"), "-ERR Operation Failed")
	if _, err := os.Stat(file); err == nil {
		t.Fatal("the audio file should be removed when the stream ends")
	}

	// a refused connection: the error event, and the stream is gone
	want(api(uuid+" start ws://127.0.0.1:1/x mono 8k"), "+OK Success")
	if e := waitSub(t, sub, "mod_audio_stream::error", 5*time.Second); !strings.Contains(e.Body, `"status":"error"`) {
		t.Fatalf("error event: %q", e.Body)
	}
	h.waitFor("the failed stream to end", func() bool { return strings.HasPrefix(api(uuid+" stop"), "-ERR") })

	// a connection the server closes: the disconnect event, the stream stays until stop
	want(api(uuid+" start "+fs.url+"/rt mono 8k"), "+OK Success")
	waitSub(t, sub, "mod_audio_stream::connect", 3*time.Second)
	fs.close <- struct{}{}
	if e := waitSub(t, sub, "mod_audio_stream::disconnect", 3*time.Second); !strings.Contains(e.Body, `"code":1001`) {
		t.Fatalf("disconnect event: %q", e.Body)
	}
	want(api(uuid+" stop"), "+OK Success")

	// the call goes on: the dialplan's playback is still running
	if ch := h.sw.Channel(uuid); ch == nil || ch.IsHungUp() {
		t.Fatal("the tap must not end the call")
	}
	// and a stream is gone with its call
	want(api(uuid+" start "+fs.url+"/rt mono 8k"), "+OK Success")
	waitSub(t, sub, "mod_audio_stream::connect", 3*time.Second)
	_, _ = call.Bye()
	h.waitChannels(0)
	want(api(uuid+" stop"), "-ERR Operation Failed")
}

// Live transcription on an ordinary call, the way the account's "global-live-transcript" dialplan
// rule does it (api_on_answer, sched_api, uuid_openai_audio_stream ... stereo 24k to
// node/transcript_live.js): both people are on the two channels and the call itself is
// not touched (the bridge keeps relaying).
func TestLiveTranscriptStereoOnBridgedCall(t *testing.T) {
	fs := newFakeStream(t)
	dp := strings.Replace(testDialplan, `<context name="default">`, `<context name="default">
  <extension name="global-live-transcript" continue="true">
    <condition field="destination_number" expression="^(.+)$" break="never">
      <action application="set" data="STREAM_OPENAI_API_KEY=local-transcript" inline="true"/>
      <action application="set" data="STREAM_DISABLE_AUDIOFILES=true" inline="true"/>
      <action application="set" data="STREAM_BUFFER_SIZE=20" inline="true"/>
      <action application="set" data="STREAM_NO_RECONNECT=true" inline="true"/>
      <action application="set" data="api_on_answer=sched_api +1 none uuid_openai_audio_stream ${uuid} start `+fs.url+`/?uuid=${uuid}&amp;from=${caller_id_number}&amp;to=${destination_number}&amp;domain=switch.example stereo 24k" inline="true"/>
    </condition>
  </extension>`, 1)
	h := newHarnessWith(t, "", twoUsers(), dp, nil)
	alice, bob := h.phone("1000"), h.phone("1001")

	type result struct {
		call *testua.Call
		err  error
	}
	done := make(chan result, 1)
	go func() {
		c, err := alice.Dial("1001", []int{0}, 10*time.Second, false)
		done <- result{c, err}
	}()
	in, err := bob.WaitInvite(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = in.Ring()
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", r.err)
	}
	call := r.call
	h.waitFor("channels bridged", func() bool {
		return strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA") == 2
	})

	// both people talk: alice a steady 3000, bob a steady -3000
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i), uint32(i*160), ulaw(3000, 160))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			_ = testua.SendRTP(in.RTP, in.Remote, 0, uint16(i), uint32(i*160), ulaw(-3000, 160))
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	// the websocket opens about a second after the answer, with the call's details in its address
	select {
	case rq := <-fs.reqs:
		q := rq.URL.Query()
		reqURI := rq.URL.RequestURI()
		if q.Get("from") != "1000" || q.Get("to") != "1001" || q.Get("domain") != "switch.example" || len(q.Get("uuid")) != 36 {
			t.Fatalf("unexpected connect url %s", reqURI)
		}
		if rq.Header.Get("Authorization") != "Bearer local-transcript" {
			t.Fatalf("authorization %q", rq.Header.Get("Authorization"))
		}
	case <-time.After(6 * time.Second):
		t.Fatal("the live transcript stream never connected")
	}

	// stereo, 24 kHz: 20 ms is 480 samples a channel, 1920 bytes, as JSON text frames of base64 (the
	// mod_openai_audio_stream framing node/transcript_frames.js decodes). left: alice, right: bob
	var left, right int
	deadline := time.Now().Add(5 * time.Second)
	for left < 2000 || right > -2000 {
		m := fs.next(t, "stereo audio", time.Until(deadline))
		if m.binary {
			t.Fatal("the openai framing is text")
		}
		if !bytes.Contains(m.data, []byte(`"type":"input_audio_buffer.append"`)) {
			continue
		}
		i := bytes.Index(m.data, []byte(`"audio":"`))
		b64 := string(m.data[i+9:])
		b64 = b64[:strings.Index(b64, `"`)]
		pcm, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(pcm) != 1920 {
			t.Fatalf("a packet is %d bytes (%v), want 1920", len(pcm), err)
		}
		s := pcmOf(pcm)
		var l, rr []int16
		for k := 0; k+1 < len(s); k += 2 {
			l, rr = append(l, s[k]), append(rr, s[k+1])
		}
		left, right = avg(l), avg(rr)
		if time.Now().After(deadline) {
			t.Fatalf("left %d right %d: alice should be on the left and bob on the right", left, right)
		}
	}

	// a second live-transcript start on the call fails (one stream per call), like the module
	var uuid string
	for _, ch := range h.sw.Channels() {
		if ch.Var("destination_number") == "1001" && ch.Direction == core.DirectionInbound {
			uuid = ch.UUID
		}
	}
	if out := h.sw.API("uuid_openai_audio_stream " + uuid + " start " + fs.url + "/x stereo 24k"); !strings.HasPrefix(out, "-ERR") {
		t.Fatalf("second start: %q", out)
	}

	// the call itself was left alone: alice's voice still reaches bob, untouched
	probe := ulaw(7000, 160)
	var heard bool
	for i := 0; i < 20 && !heard; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(5000+i), uint32((5000+i)*160), probe)
		_, _, err := testua.RecvRTP(in.RTP, 100*time.Millisecond, func(pt uint8, p []byte) bool { return bytes.Equal(p, probe) })
		heard = err == nil
	}
	if !heard {
		t.Fatal("the transcript stream broke the call's audio")
	}
	if got := strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA"); got != 2 {
		t.Fatalf("the call should still be bridged (%d legs)", got)
	}

	// hang up: the stream goes with the call
	if _, err := call.Bye(); err != nil {
		t.Fatal(err)
	}
	if bye, err := bob.WaitRequest("BYE", 5*time.Second); err == nil {
		_ = bob.Respond(bye, 200, "", nil, "")
	}
	h.waitChannels(0)
	if out := h.sw.API("uuid_openai_audio_stream " + uuid + " stop"); !strings.HasPrefix(out, "-ERR") {
		t.Fatalf("stop after hangup: %q", out)
	}
}

// live_transcribe.lua (the older route of the same rule): luarun'd with the call's uuid, from and
// to, it starts uuid_audio_stream ... stereo 24000 and the bridge gets raw stereo binary frames.
func TestLiveTranscribeScript(t *testing.T) {
	fs := newFakeStream(t)
	defer core.SetLiveTranscribeBridge(fs.url)()
	h := newHarnessWith(t, "", twoUsers(), testDialplan, nil)
	alice, bob := h.phone("1000"), h.phone("1001")
	done := make(chan *testua.Call, 1)
	go func() {
		c, _ := alice.Dial("1001", []int{0}, 10*time.Second, false)
		done <- c
	}()
	in, err := bob.WaitInvite(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = in.Ring()
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	call := <-done
	if call == nil || call.Final.StatusCode != 200 {
		t.Fatal("call failed")
	}
	h.waitFor("channels bridged", func() bool {
		return strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA") == 2
	})
	var uuid string
	for _, ch := range h.sw.Channels() {
		if ch.Direction == core.DirectionInbound && ch.Var("destination_number") == "1001" {
			uuid = ch.UUID
		}
	}
	if out := h.sw.API("luarun live_transcribe.lua " + uuid + " 1000 1001"); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("luarun: %q", out)
	}
	rq := <-fs.reqs
	q := rq.URL.Query()
	if rq.URL.Path != "/" || q.Get("uuid") != uuid || q.Get("from") != "1000" || q.Get("to") != "1001" || q.Get("domain") == "" {
		t.Fatalf("unexpected connect url %s", rq.URL.RequestURI())
	}
	if ch := h.sw.Channel(uuid); ch.Var("X_UUID") != uuid || ch.Var("X_FROM") != "1000" || ch.Var("X_TO") != "1001" || ch.Var("X_DOMAIN") == "" {
		t.Fatalf("channel variables not set: %v", ch.Vars())
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i), uint32(i*160), ulaw(3000, 160))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			_ = testua.SendRTP(in.RTP, in.Remote, 0, uint16(i), uint32(i*160), ulaw(-3000, 160))
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	var left, right int
	deadline := time.Now().Add(5 * time.Second)
	for left < 2000 || right > -2000 {
		m := fs.next(t, "stereo audio", time.Until(deadline))
		if !m.binary || len(m.data) != 1920 {
			t.Fatalf("expected 1920 byte binary frames (20 ms stereo at 24 kHz), got binary=%v len=%d", m.binary, len(m.data))
		}
		s := pcmOf(m.data)
		var l, rr []int16
		for k := 0; k+1 < len(s); k += 2 {
			l, rr = append(l, s[k]), append(rr, s[k+1])
		}
		left, right = avg(l), avg(rr)
		if time.Now().After(deadline) {
			t.Fatalf("left %d right %d", left, right)
		}
	}
	// the script ends with the call
	_, _ = call.Bye()
	if bye, err := bob.WaitRequest("BYE", 5*time.Second); err == nil {
		_ = bob.Respond(bye, 200, "", nil, "")
	}
	h.waitChannels(0)
	if out := h.sw.API("luarun live_transcribe.lua no-such-uuid a b"); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("luarun of a missing call: %q", out)
	}
}

// The AI IVR scripts fire the custom events node/server.js answers; the same server
// then runs uuid_audio_stream ... mono 16k <uuid> and parks the call.
func TestAIIVRScripts(t *testing.T) {
	dp := aiDialplan(`
  <extension name="ai-ivr1"><condition field="destination_number" expression="^7771$">
    <action application="answer"/>
    <action application="lua" data="ivr_ai.lua ${uuid} sub.example"/>
    <action application="sleep" data="3000"/>
  </condition></extension>
  <extension name="ai-ivr3"><condition field="destination_number" expression="^7773$">
    <action application="answer"/>
    <action application="lua" data="ivr_ai3.lua ${uuid} sub.example"/>
    <action application="sleep" data="3000"/>
  </condition></extension>
  <extension name="ai-ivr2"><condition field="destination_number" expression="^7772$">
    <action application="answer"/>
    <action application="lua" data="ivr_ai2.lua ${uuid} sub.example"/>
    <action application="sleep" data="3000"/>
  </condition></extension>`)
	h := newHarnessWith(t, "", twoUsers(), dp, nil)
	sub := h.sw.Events().Subscribe(64, func(e *core.Event) bool { return strings.HasPrefix(e.Subclass(), "ai_ivr_recording") })
	defer sub.Close()
	alice := h.phone("1000")
	for _, tc := range []struct{ dest, subclass string }{
		{"7771", "ai_ivr_recording"}, {"7772", "ai_ivr_recording2"}, {"7773", "ai_ivr_recording3"},
	} {
		call, err := alice.Dial(tc.dest, []int{0}, 5*time.Second, false)
		if err != nil || call.Final.StatusCode != 200 {
			t.Fatalf("%s: call failed: %v", tc.dest, err)
		}
		e := waitSub(t, sub, tc.subclass, 4*time.Second)
		uuid := e.Get("Caller-uuid")
		if len(uuid) != 36 || h.sw.Channel(uuid) == nil || e.Get("Caller-subdomain") != "sub.example" {
			t.Fatalf("%s: unexpected event headers uuid=%q sub=%q", tc.dest, uuid, e.Get("Caller-subdomain"))
		}
		hasRec := false
		for _, kv := range e.Headers {
			hasRec = hasRec || kv[0] == "Recording-Filename"
		}
		if !hasRec {
			t.Fatalf("%s: Recording-Filename header missing", tc.dest)
		}
		// what node/server.js does with it: a tap towards the AI service, then park
		fs := newFakeStream(t)
		if out := h.sw.API("uuid_audio_stream " + uuid + " start " + fs.url + " mono 16k " + uuid); !strings.HasPrefix(out, "+OK") {
			t.Fatalf("%s: start: %q", tc.dest, out)
		}
		if out := h.sw.API("uuid_park " + uuid); !strings.HasPrefix(out, "+OK") {
			t.Fatalf("%s: park: %q", tc.dest, out)
		}
		if m := fs.next(t, "metadata", 5*time.Second); m.binary || string(m.data) != uuid {
			t.Fatalf("%s: metadata frame %q", tc.dest, m.data)
		}
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, 1, 160, ulaw(2000, 160))
		got := false
		for i := 0; i < 40 && !got; i++ {
			_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(2+i), uint32((2+i)*160), ulaw(2000, 160))
			select {
			case m := <-fs.msgs:
				got = m.binary && len(m.data) == 640
			case <-time.After(30 * time.Millisecond):
			}
		}
		if !got {
			t.Fatalf("%s: no audio from the parked call", tc.dest)
		}
		_, _ = call.Bye()
		h.waitChannels(0)
	}
}

func wavBytes(rate int, pcm []int16) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)*2))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(1))
	w(uint32(rate))
	w(uint32(rate * 2))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm) * 2))
	for _, v := range pcm {
		w(v)
	}
	return b.Bytes()
}

// The legacy AI receptionist route (AiController, AI_AUDIO_TRANSPORT = 'legacy', node/cluade_server.js on port 8081): the
// dialplan starts uuid_audio_stream with the call's metadata and parks the call; the bridge plays its voice
// with uuid_broadcast of WAV files, stops it with uuid_break, and ends the call with uuid_transfer / uuid_kill.
func TestAudioStreamLegacyAIBridgeRoute(t *testing.T) {
	fs := newFakeStream(t)
	dp := aiDialplan(`
  <extension name="legacy"><condition field="destination_number" expression="^9400$">
    <action application="answer"/>
    <action application="set" data="ai_meta=${uuid}|${caller_id_number}|${destination_number}|${domain_name}"/>
    <action application="set" data="api_result=${uuid_audio_stream ${uuid} start ` + fs.url + ` mono 8k ${ai_meta}}"/>
    <action application="log" data="INFO start -> ${api_result}"/>
    <action application="sleep" data="500"/>
    <action application="park"/>
    <action application="set" data="api_result=${uuid_audio_stream ${uuid} stop}"/>
  </condition></extension>`)
	h := newHarnessWith(t, "", twoUsers(), dp, nil)
	alice := h.phone("1000")
	call, err := alice.Dial("9400", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", err)
	}
	var uuid string
	h.waitFor("the call's channel", func() bool {
		for _, ch := range h.sw.Channels() {
			if ch.Var("destination_number") == "9400" {
				uuid = ch.UUID
			}
		}
		return uuid != ""
	})
	// metadata: uuid|caller|extension|domain, the first frame
	m := fs.next(t, "metadata", 5*time.Second)
	parts := strings.Split(string(m.data), "|")
	if m.binary || len(parts) != 4 || parts[0] != uuid || parts[1] != "1000" || parts[2] != "9400" {
		t.Fatalf("metadata frame %q", m.data)
	}
	// the caller's audio, mono 8 kHz: 320 bytes per 20 ms, while the call is parked
	got := false
	for i := 0; i < 60 && !got; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i), uint32(i*160), ulaw(2500, 160))
		select {
		case m := <-fs.msgs:
			got = m.binary && len(m.data) == 320 && avg(pcmOf(m.data)) > 1500
		case <-time.After(30 * time.Millisecond):
		}
	}
	if !got {
		t.Fatal("no caller audio from the parked call")
	}

	// the bridge's voice: a 24 kHz WAV written to a file and uuid_broadcast to the caller
	tone := make([]int16, 24000/2)
	for i := range tone {
		tone[i] = 9000
		if (i/20)%2 == 1 {
			tone[i] = -9000
		}
	}
	file := h.dir + "/tts_0.wav"
	if err := os.WriteFile(file, wavBytes(24000, tone), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := h.sw.API("uuid_broadcast " + uuid + " " + file + " aleg"); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("uuid_broadcast: %q", out)
	}
	heard := false
	for i := 0; i < 40 && !heard; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(100+i), uint32((100+i)*160), ulaw(2500, 160))
		_, _, err := testua.RecvRTP(call.RTP, 100*time.Millisecond, func(pt uint8, p []byte) bool {
			return pt == 0 && len(bytes.Trim(p, "\xff\x7f")) > 0
		})
		heard = err == nil
	}
	if !heard {
		t.Fatal("the caller did not hear the broadcast file")
	}
	// uuid_break (INTERRUPT_SILENCE_FILE is silence_stream://100) and the stream keeps running
	if out := h.sw.API("uuid_broadcast " + uuid + " silence_stream://100 aleg"); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("uuid_broadcast silence: %q", out)
	}
	if out := h.sw.API("uuid_break " + uuid); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("uuid_break: %q", out)
	}
	if got := h.sw.API("uuid_audio_stream " + uuid + " pause"); !strings.HasPrefix(got, "+OK") {
		t.Fatalf("the stream should still be running after the broadcast: %q", got)
	}
	_ = h.sw.API("uuid_audio_stream " + uuid + " resume")

	// the bridge ends the call itself
	if out := h.sw.API("uuid_kill " + uuid); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("uuid_kill: %q", out)
	}
	h.waitChannels(0)
}
