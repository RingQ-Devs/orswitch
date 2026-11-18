package core_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// With ai-log-file set, an AI call's errors land in a FreeSWITCH-shaped log line the portal's
// AiVoice::summarize understands: "<uuid> <date> ... processMessage - error: {json}".
func TestAILogFile(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "freeswitch.log")
	dp := aiDialplan(`
  <extension name="ai"><condition field="destination_number" expression="^9300$">
    <action application="answer"/>
    <action application="set" data="STREAM_OPENAI_API_KEY=sk-test"/>
    <action application="playback" data="silence_stream://-1"/>
  </condition></extension>`)
	h := newHarnessWith(t, `<param name="ai-log-file" value="`+logPath+`"/>`, twoUsers(), dp, nil)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"insufficient_quota","code":"credit_balance_exhausted","message":"no credits"}}`))
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	alice := h.phone("1000")
	call, err := alice.Dial("9300", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call failed: %v", err)
	}
	var uuid string
	h.waitFor("channel", func() bool {
		for _, ch := range h.sw.Channels() {
			if ch.Var("destination_number") == "9300" {
				uuid = ch.UUID
			}
		}
		return uuid != ""
	})
	if out := h.sw.API("uuid_openai_audio_stream " + uuid + " start ws" + strings.TrimPrefix(srv.URL, "http") + " mono 24k"); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("start: %q", out)
	}
	re := regexp.MustCompile(`(?m)^` + uuid + ` \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d+ [\d.]+% \[ERR\] \S+ \(` + uuid + `\) processMessage - error:\s*(\{.*\})\s*$`)
	h.waitFor("the error line", func() bool {
		b, _ := os.ReadFile(logPath)
		return re.Match(b)
	})
	b, _ := os.ReadFile(logPath)
	if !strings.Contains(string(b), "insufficient_quota") || !strings.Contains(string(b), uuid+" ") {
		t.Fatalf("log:\n%s", b)
	}
}
