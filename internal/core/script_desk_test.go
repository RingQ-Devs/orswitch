package core_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// Desk park: a call transferred to *591 is parked in the shared slot (Redis list),
// and a phone dialling the slot picks it up.
func TestDeskPark(t *testing.T) {
	if _, err := exec.LookPath("redis-cli"); err != nil {
		t.Skip("needs redis")
	}
	defer exec.Command("redis-cli", "DEL", "127.0.0.1:parked").Run()
	dp := strings.Replace(testDialplan, `</context></include>`, `
  <extension name="park"><condition field="destination_number" expression="^8501$">
    <action application="set" data="domain_name=127.0.0.1"/>
    <action application="set" data="last_bridge_to=x"/>
    <action application="lua" data="ringq_desk_park.lua 1"/>
  </condition></extension>
  <extension name="unpark"><condition field="destination_number" expression="^8502$">
    <action application="set" data="domain_name=127.0.0.1"/>
    <action application="lua" data="ringq_desk_park.lua 1"/>
  </condition></extension>
</context></include>`, 1)
	users := "<include>"
	for _, u := range []string{"1000", "1001"} {
		users += `<user id="` + u + `"><params><param name="password" value="` + testPassword + `"/></params></user>`
	}
	users += "</include>"
	h := newHarnessWith(t, "", users, dp, func(c *config.Config) {})
	alice, bob := h.phone("1000"), h.phone("1001")

	acall, err := alice.Dial("8501", []int{0}, 5*time.Second, false)
	if err != nil || acall.Final.StatusCode != 200 {
		t.Fatalf("the parking call failed: %v", err)
	}
	h.waitFor("the call to be parked", func() bool {
		out, _ := exec.Command("redis-cli", "LINDEX", "127.0.0.1:parked", "0").Output()
		return len(bytes.TrimSpace(out)) > 30
	})
	bcall, err := bob.Dial("8502", []int{0}, 5*time.Second, false)
	if err != nil || bcall.Final.StatusCode != 200 {
		t.Fatalf("the pickup call failed: %v", err)
	}
	// the picker and the parked caller now talk to each other
	payload := ulaw(2000, 160)
	ok := false
	for i := 0; i < 30 && !ok; i++ {
		_ = testua.SendRTP(acall.RTP, acall.RemoteRTP, 0, uint16(i), uint32(i*160), payload)
		_, p, err := testua.RecvRTP(bcall.RTP, 100*time.Millisecond, func(pt uint8, p []byte) bool { return pt == 0 && bytes.Equal(p, payload) })
		ok = err == nil && bytes.Equal(p, payload)
	}
	if !ok {
		t.Fatal("the parked caller and the picker are not connected")
	}
	out, _ := exec.Command("redis-cli", "LINDEX", "127.0.0.1:parked", "0").Output()
	if len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("the slot should be free after the pickup: %q", out)
	}
}
