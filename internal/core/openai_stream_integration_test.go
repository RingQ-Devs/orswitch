package core_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// uuid_openai_audio_stream against a fake OpenAI Realtime server: the session
// settings and greeting, caller audio up, AI audio down, and the keyword transfer.
func TestOpenAIAudioStream(t *testing.T) {
	dp := strings.Replace(testDialplan, `</context></include>`, `
  <extension name="ai"><condition field="destination_number" expression="^9300$">
    <action application="answer"/>
    <action application="set" data="STREAM_OPENAI_API_KEY=sk-test"/>
    <action application="set" data="STREAM_OPENAI_VOICE=marin"/>
    <action application="set" data="STREAM_OPENAI_INSTRUCTIONS=Be brief."/>
    <action application="set" data="STREAM_OPENAI_TRANSFER_MAP=human:1001"/>
    <action application="playback" data="silence_stream://-1"/>
  </condition></extension>
</context></include>`, 1)
	users := "<include>"
	for _, u := range []string{"1000", "1001"} {
		users += `<user id="` + u + `"><params><param name="password" value="` + testPassword + `"/></params></user>`
	}
	users += "</include>"
	h := newHarnessWith(t, "", users, dp, func(c *config.Config) {})

	var auth string
	msgs := make(chan map[string]any, 64)
	send := make(chan string, 8)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		go func() {
			for s := range send {
				_ = c.WriteMessage(websocket.TextMessage, []byte(s))
			}
		}()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) == nil {
				msgs <- m
			}
		}
	}))
	defer srv.Close()
	defer close(send)

	alice, bob := h.phone("1000"), h.phone("1001")
	call, err := alice.Dial("9300", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", err)
	}
	var uuid string
	h.waitFor("the AI call's channel", func() bool {
		for _, ch := range h.sw.Channels() {
			// the dialplan has set its STREAM_OPENAI_* variables once the transfer map is there
			if ch.Var("destination_number") == "9300" && ch.Var("STREAM_OPENAI_TRANSFER_MAP") != "" {
				uuid = ch.UUID
			}
		}
		return uuid != ""
	})
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/realtime?model=gpt-realtime"
	if out := h.sw.API("uuid_openai_audio_stream " + uuid + " start " + url + " mono 24k"); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("start: %q", out)
	}
	if out := h.sw.API("uuid_openai_audio_stream " + uuid + " start http://nope mono 24k"); !strings.HasPrefix(out, "-ERR") {
		t.Fatalf("a bad uri should fail: %q", out)
	}
	next := func(what string) map[string]any {
		t.Helper()
		select {
		case m := <-msgs:
			return m
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s from the stream", what)
			return nil
		}
	}
	su := next("session.update")
	sess, _ := su["session"].(map[string]any)
	out, _ := sess["audio"].(map[string]any)["output"].(map[string]any)
	if su["type"] != "session.update" || sess["instructions"] != "Be brief." || out["voice"] != "marin" || auth != "Bearer sk-test" {
		t.Fatalf("unexpected session.update %v auth %q", su, auth)
	}
	if rc := next("response.create"); rc["type"] != "response.create" {
		t.Fatalf("unexpected %v", rc)
	}

	// caller -> OpenAI: 20 ms at 24 kHz is 960 bytes
	payload := ulaw(3000, 160)
	var appended map[string]any
	for i := 0; i < 40 && appended == nil; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i), uint32(i*160), payload)
		select {
		case m := <-msgs:
			if m["type"] == "input_audio_buffer.append" {
				appended = m
			}
		case <-time.After(30 * time.Millisecond):
		}
	}
	if appended == nil {
		t.Fatal("caller audio never reached the websocket")
	}
	if raw, _ := base64.StdEncoding.DecodeString(appended["audio"].(string)); len(raw) != 960 {
		t.Fatalf("expected 960 bytes per 20 ms at 24 kHz, got %d", len(raw))
	}

	// OpenAI -> caller: 200 ms of a tone arrives as non-silent RTP
	tone := make([]byte, 24000/5*2)
	for i := 0; i < len(tone)/2; i++ {
		v := int16(8000)
		if (i/12)%2 == 1 {
			v = -8000
		}
		binary.LittleEndian.PutUint16(tone[i*2:], uint16(v))
	}
	send <- `{"type":"response.output_audio.delta","delta":"` + base64.StdEncoding.EncodeToString(tone) + `"}`
	heard := false
	for i := 0; i < 30 && !heard; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(100+i), uint32((100+i)*160), payload)
		_, _, err := testua.RecvRTP(call.RTP, 100*time.Millisecond, func(pt uint8, p []byte) bool {
			return pt == 0 && len(bytes.Trim(p, "\xff\x7f")) > 0
		})
		heard = err == nil
	}
	if !heard {
		t.Fatal("the caller did not hear the AI")
	}

	// the caller asks for a human: the call goes to 1001 once the AI has spoken
	send <- `{"type":"conversation.item.input_audio_transcription.completed","transcript":"I want a Human please"}`
	in, err := bob.WaitInvite(9 * time.Second)
	if err != nil {
		t.Fatalf("the keyword did not transfer the call: %v", err)
	}
	_ = in
	if got := h.sw.API("uuid_openai_audio_stream " + uuid + " stop"); !strings.HasPrefix(got, "-ERR") {
		t.Fatalf("the stream should already be stopped by the transfer: %q", got)
	}
	_ = core.Version
}
