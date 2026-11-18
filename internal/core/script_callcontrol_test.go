package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/config"
)

// Call control: resolve reads the extension's behaviors (here a fake database),
// and when nobody answers the failover step rings the Call Relay destination.
func TestCallControlRelay(t *testing.T) {
	dp := strings.Replace(testDialplan, `</context></include>`, `
  <extension name="cc"><condition field="destination_number" expression="^8001$">
    <action application="set" data="extension_uuid=ext-8001" inline="true"/>
    <action application="set" data="call_direction=local" inline="true"/>
    <action application="set" data="rq_cc=${lua(ringq_call_control.lua resolve ${uuid})}" inline="true"/>
    <action application="set" data="continue_on_fail=true"/>
    <action application="bridge" data="[leg_timeout=3]user/1003"/>
    <action application="lua" data="ringq_call_control.lua failover"/>
  </condition></extension>
</context></include>`, 1)
	users := "<include>"
	for _, u := range []string{"1000", "1002"} { // 1003 is not registered
		users += `<user id="` + u + `"><params><param name="password" value="` + testPassword + `"/></params></user>`
	}
	users += `<user id="1003"><params><param name="password" value="x"/></params></user></include>`
	h := newHarnessWith(t, "", users, dp, func(c *config.Config) {})
	var asked string
	h.sw.SetSQL(func(_ context.Context, q string, args ...any) ([]map[string]string, error) {
		asked = q
		return []map[string]string{{
			"forward_no_answer_timeout": "10,10,10,10,10",
			"follow_me_enabled":         "true,true,true,true,true",
			"follow_me_destinations":    "1002|1002,1002,1002,1002,1002",
			"forward_busy_enabled":      "false", "forward_no_answer_enabled": "false",
			"call_screening": "false",
		}}, nil
	})
	alice, bob := h.phone("1000"), h.phone("1002")
	go func() { _, _ = alice.Dial("8001", []int{0}, 10*time.Second, false) }()
	in, err := bob.WaitInvite(8 * time.Second)
	if err != nil {
		t.Fatalf("the call relay did not ring 1002: %v", err)
	}
	_ = in
	if !strings.Contains(asked, "q_call_behaviors") {
		t.Fatalf("behaviors were not read: %q", asked)
	}
}
