package core_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/sip"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// call_flow_subscribe / call_flow_monitor: the lamp of a call flow key comes from the database.
func TestScriptCallFlowLamps(t *testing.T) {
	h := cfHarness(t, "", nil)
	status := "false"
	db := &cfDB{rows: func(q string, a []any) []map[string]string {
		if strings.Contains(q, "t1.call_flow_uuid") {
			return []map[string]string{{"call_flow_uuid": "cf1", "call_flow_status": status}}
		}
		if strings.Contains(q, "v_call_flows AS f") {
			return []map[string]string{{"domain_name": cfDomain, "call_flow_uuid": "cf1", "call_flow_feature_code": "*88", "call_flow_status": status}}
		}
		return nil
	}}
	db.install(h)

	// a phone's BLF key "flow+*77": the SUBSCRIBE lights it from the database
	watcher := h.phone("1003")
	blf, resp, err := watcher.Subscribe("flow+*77", "dialog", 600)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("subscribe: %v %v", resp, err)
	}
	nextNotify(t, watcher, blf, func(m *sip.Message) bool { return strings.Contains(body(m), "<state>confirmed</state>") })
	if !h.sw.LampOn("flow+*77", cfDomain) {
		t.Fatal("lamp not lit")
	}

	// blf_subscribe flow, directly; an unsubscribe changes nothing
	status = "true"
	h.sw.API("lua blf_subscribe.lua flow x flow+*77@" + cfDomain + " 0")
	if !h.sw.LampOn("flow+*77", cfDomain) {
		t.Fatal("an unsubscribe must not touch the lamp")
	}
	h.sw.API("lua call_flow_subscribe.lua x flow+*77@" + cfDomain + " 3600")
	if h.sw.LampOn("flow+*77", cfDomain) {
		t.Fatal("status true: lamp off")
	}

	// the monitor sets every flow's lamp, and stops
	status = "false"
	h.sw.StartService("call_flow_monitor")
	waitUntil(t, "the monitor to light the lamp", 3*time.Second, func() bool { return h.sw.LampOn("flow+*88", cfDomain) })
	h.sw.StopService("call_flow_monitor")
	time.Sleep(100 * time.Millisecond)
	status = "true"
	h.sw.StartService("call_flow_monitor") // runs again (the first one ended)
	waitUntil(t, "the monitor to clear the lamp", 3*time.Second, func() bool { return !h.sw.LampOn("flow+*88", cfDomain) })
	h.sw.StopService("call_flow_monitor")
}

// park.lua / park_monitor.lua: park a call in a lot, pick it up from another phone, the lamp follows.
func TestScriptPark(t *testing.T) {
	h := cfHarness(t, `<extension name="park-in"><condition field="destination_number" expression="^8601$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="set" data="park_extension=5900"/>
    <action application="set" data="park_direction=in"/><action application="lua" data="park.lua"/>
  </condition></extension>
  <extension name="park-out"><condition field="destination_number" expression="^8602$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="set" data="park_extension=5900"/>
    <action application="set" data="park_direction=out"/><action application="lua" data="park.lua"/>
  </condition></extension>`, nil)
	alice, bob := h.phone("1000"), h.phone("1001")
	acall, err := alice.Dial("8601", []int{0}, 5*time.Second, false)
	if err != nil || acall.Final.StatusCode != 200 {
		t.Fatalf("park call: %v", err)
	}
	waitUntil(t, "the lamp of the lot", 3*time.Second, func() bool { return h.sw.LampOn("5900", cfDomain) })
	bcall, err := bob.Dial("8602", []int{0}, 5*time.Second, false)
	if err != nil || bcall.Final.StatusCode != 200 {
		t.Fatalf("pickup call: %v", err)
	}
	if !rtpFlows(acall.RTP, acall.RemoteRTP, bcall.RTP) {
		t.Fatal("the parked caller and the picker are not connected")
	}
	waitUntil(t, "the lamp to go out", 5*time.Second, func() bool { return !h.sw.LampOn("5900", cfDomain) })
}

// park_monitor: a parked call that nobody picks up goes to park_timeout_destination.
func TestScriptParkTimeout(t *testing.T) {
	h := cfHarness(t, `<extension name="park-in"><condition field="destination_number" expression="^8603$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="set" data="park_extension=5901"/>
    <action application="set" data="park_direction=in"/><action application="set" data="park_timeout_seconds=1"/>
    <action application="set" data="park_timeout_destination=1001"/><action application="lua" data="park.lua"/>
  </condition></extension>`, nil)
	alice, bob := h.phone("1000"), h.phone("1001")
	go func() { _, _ = alice.Dial("8603", []int{0}, 10*time.Second, false) }()
	in, err := bob.WaitInvite(8 * time.Second)
	if err != nil {
		t.Fatalf("the parked call did not return to 1001 after the timeout: %v", err)
	}
	_ = in
}

// page.lua: the paged phones are rung with auto-answer and join a conference with the caller.
func TestScriptPage(t *testing.T) {
	h := cfHarness(t, `<extension name="page"><condition field="destination_number" expression="^8701$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="set" data="destinations=1001,1002-1003"/>
    <action application="set" data="sip_from_user=1000"/><action application="set" data="mute=true"/>
    <action application="lua" data="page.lua"/>
  </condition></extension>`, nil)
	alice := h.phone("1000")
	phones := []*testua.UA{h.phone("1001"), h.phone("1002"), h.phone("1003")}
	done := make(chan *testua.Call, 1)
	go func() { c, _ := alice.Dial("8701", []int{0}, 10*time.Second, false); done <- c }()
	var ins []*testua.Incoming
	for _, p := range phones {
		in, err := p.WaitInvite(6 * time.Second)
		if err != nil {
			t.Fatalf("a paged phone was not rung: %v", err)
		}
		if !strings.Contains(in.Invite.Get("Call-Info"), "answer-after=0") || !strings.Contains(in.Invite.Get("Alert-Info"), "Ring Answer") {
			t.Fatalf("not an auto-answer call: Call-Info %q Alert-Info %q", in.Invite.Get("Call-Info"), in.Invite.Get("Alert-Info"))
		}
		if err := in.Answer(0); err != nil {
			t.Fatal(err)
		}
		ins = append(ins, in)
	}
	acall := <-done
	if acall == nil {
		t.Fatal("the caller was not connected to the page")
	}
	waitUntil(t, "everybody in the page conference", 3*time.Second, func() bool {
		rooms := h.sw.Conferences(cfDomain)
		return len(rooms) == 1 && len(rooms[0].Members) == 4
	})
	for _, m := range h.sw.Conferences(cfDomain)[0].Members {
		if m.CIDNumber != "1000" && !m.Muted {
			t.Fatalf("a paged phone %s is not muted", m.CIDNumber)
		}
	}
	// the caller is heard by the paged phones
	if !hearsFrom(acall, ins[0]) {
		t.Fatal("the paged phone does not hear the caller")
	}
	// when the caller hangs up the page ends
	_, _ = acall.Bye()
	for _, in := range ins {
		if _, err := in.UA.WaitRequest("BYE", 5*time.Second); err != nil {
			t.Fatalf("the page did not end for a paged phone: %v", err)
		}
	}
}

func hearsFrom(from *testua.Call, to *testua.Incoming) bool {
	payload := ulaw(3000, 160)
	for i := 0; i < 60; i++ {
		_ = testua.SendRTP(from.RTP, from.RemoteRTP, 0, uint16(i), uint32(i*160), payload)
		if _, p, err := testua.RecvRTP(to.RTP, 100*time.Millisecond, nil); err == nil && nonSilent(p) {
			return true
		}
	}
	return false
}

// eavesdrop.lua / three_way.lua: a third phone listens to an extension's call (and barges in).
func TestScriptEavesdrop(t *testing.T) {
	for _, script := range []string{"eavesdrop", "three_way"} {
		t.Run(script, func(t *testing.T) {
			h := cfHarness(t, `<extension name="spy"><condition field="destination_number" expression="^8801$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="lua" data="`+script+`.lua 1001"/>
  </condition></extension>`, nil)
			alice, bob, carol := h.phone("1000"), h.phone("1001"), h.phone("1002")
			done := make(chan *testua.Call, 1)
			go func() { c, _ := alice.Dial("1001", []int{0}, 10*time.Second, false); done <- c }()
			in, err := bob.WaitInvite(5 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := in.Answer(0); err != nil {
				t.Fatal(err)
			}
			acall := <-done
			if acall == nil {
				t.Fatal("no call to listen to")
			}
			h.waitFor("channels bridged", func() bool {
				return strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA") == 2
			})
			ccall, err := carol.Dial("8801", []int{0}, 5*time.Second, false)
			if err != nil || ccall.Final.StatusCode != 200 {
				t.Fatalf("the supervisor's call: %v", err)
			}
			// the supervisor hears alice
			heard := false
			payload := ulaw(4000, 160)
			for i := 0; i < 80 && !heard; i++ {
				_ = testua.SendRTP(acall.RTP, acall.RemoteRTP, 0, uint16(i), uint32(i*160), payload)
				if _, p, err := testua.RecvRTP(ccall.RTP, 60*time.Millisecond, nil); err == nil && nonSilent(p) {
					heard = true
				}
			}
			if !heard {
				t.Fatal("the supervisor does not hear the call")
			}
			if script == "three_way" {
				// barge: bob hears carol
				heardCarol := false
				for i := 0; i < 80 && !heardCarol; i++ {
					_ = testua.SendRTP(ccall.RTP, ccall.RemoteRTP, 0, uint16(i), uint32(i*160), ulaw(5000, 160))
					if _, p, err := testua.RecvRTP(in.RTP, 60*time.Millisecond, nil); err == nil && nonSilent(p) && !equalBytes(p, ulaw(0, 160)) {
						heardCarol = true
					}
				}
				if !heardCarol {
					t.Fatal("three way: the called party does not hear the supervisor")
				}
			}
			// the supervisor hangs up: the call goes on directly
			_, _ = ccall.Bye()
			time.Sleep(300 * time.Millisecond)
			if !rtpFlows(acall.RTP, acall.RemoteRTP, in.RTP) {
				t.Fatal("the call is not reconnected after the supervisor left")
			}
		})
	}
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

// eavesdrop.lua with a PIN: a wrong PIN ends the call.
func TestScriptEavesdropPIN(t *testing.T) {
	h := cfHarness(t, `<extension name="spy"><condition field="destination_number" expression="^8802$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="set" data="pin_number=4321"/>
    <action application="lua" data="eavesdrop.lua 1001"/>
  </condition></extension>`, nil)
	call, err := h.phone("1002").Dial("8802", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	for i := 0; i < 3; i++ {
		dtmf(call, "1111#")
		time.Sleep(700 * time.Millisecond)
	}
	waitUntil(t, "the call to be hung up", 8*time.Second, func() bool { return len(h.sw.Channels()) == 0 })
}

// intercept_group.lua: picks up a call ringing a phone of the caller's call group.
func TestScriptInterceptGroup(t *testing.T) {
	h := cfHarness(t, `<extension name="ig"><condition field="destination_number" expression="^8950$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="lua" data="intercept_group.lua"/>
  </condition></extension>`, map[string]string{"1001": "call_group=sales", "1002": "call_group=sales", "1003": "call_group=other"})
	alice, bob, carol := h.phone("1000"), h.phone("1001"), h.phone("1002")
	done := make(chan *testua.Call, 1)
	go func() { c, _ := alice.Dial("1001", []int{0}, 10*time.Second, false); done <- c }()
	in, err := bob.WaitInvite(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = in.Ring()
	pick, err := carol.Dial("8950", []int{0}, 5*time.Second, false)
	if err != nil || pick.Final.StatusCode != 200 {
		t.Fatalf("pickup call: %v", err)
	}
	acall := <-done
	if acall == nil {
		t.Fatal("alice was not connected to the picker")
	}
	if !rtpFlows(acall.RTP, acall.RemoteRTP, pick.RTP) {
		t.Fatal("alice and the picker are not connected")
	}
	// bob's phone stops ringing
	if _, err := bob.WaitRequest("CANCEL", 3*time.Second); err != nil {
		t.Fatalf("the ringing phone was not cancelled: %v", err)
	}
}

// The small scripts, started from the API (as the RingQ backend starts them) or from a dialplan.
func TestScriptsMisc(t *testing.T) {
	dir := t.TempDir()
	h := cfHarness(t, `<extension name="dcid"><condition field="destination_number" expression="^(1\d{10})$">
    <action application="lua" data="destination_caller_id.lua"/><action application="bridge" data="user/1001"/>
  </condition></extension>
  <extension name="bind"><condition field="destination_number" expression="^8940$">
    <action application="answer"/><action application="lua" data="bind_digit_bleg.lua default"/><action application="echo"/>
  </condition></extension>`, nil)
	db := &cfDB{rows: func(q string, a []any) []map[string]string {
		switch {
		case strings.Contains(q, "v_destinations WHERE"):
			return []map[string]string{{"destination_number": "18005550000"}}
		case strings.Contains(q, "v_contacts"):
			return []map[string]string{{"name": "Alice Smith"}}
		}
		return nil
	}}
	db.install(h)
	alice, bob := h.phone("1000"), h.phone("1001")

	// destination_caller_id: the caller id of the call is a number of the tenant in the dialled area code
	go func() { _, _ = alice.Dial("18005551234", []int{0}, 5*time.Second, false) }()
	in, err := bob.WaitInvite(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in.Invite.Get("From"), "18005550000") {
		t.Fatalf("destination_caller_id did not set the caller id: %s", in.Invite.Get("From"))
	}
	uuid := channelOf(h, "1000")
	if uuid == "" {
		t.Fatal("no channel for alice")
	}
	_ = in.Ring()
	_ = in.Answer(0)
	h.waitFor("channels bridged", func() bool { return strings.Count(h.sw.API("show channels"), "CS_EXCHANGE_MEDIA") == 2 })

	// cidlookup <uuid>: the caller's name from the contacts
	h.sw.API("lua cidlookup.lua " + uuid)
	if n := h.sw.Channel(uuid).Var("caller_id_name"); n != "Alice Smith" {
		t.Fatalf("cidlookup: caller_id_name %q", n)
	}
	// dtmf_handler <uuid> <digit>
	h.sw.API("lua dtmf_handler.lua " + uuid + " 7")
	if v := h.sw.Channel(uuid).Var("dtmf_history"); !strings.Contains(v, ":7") {
		t.Fatalf("dtmf_history %q", v)
	}
	// call_divert <uuid>: both parties hear the caller's number
	acall := in.Dialog()
	_ = acall
	h.sw.API("lua call_divert.lua " + uuid)
	if !hearsAnything(in.RTP, 6*time.Second) {
		t.Fatal("call_divert: the called party hears nothing")
	}
	h.sw.API("lua call_back_request.lua " + uuid + " 5551234")

	// mkdir
	d := filepath.Join(dir, "a", "b")
	if out := h.sw.API("lua mkdir.lua " + d); strings.HasPrefix(out, "-ERR") {
		t.Fatal(out)
	}
	if st, err := os.Stat(d); err != nil || !st.IsDir() {
		t.Fatal("mkdir.lua did not make the folder")
	}
	// transfer2 needs its arguments
	if out := h.sw.API("lua transfer2.lua " + uuid); !strings.HasPrefix(out, "-ERR") {
		t.Fatalf("transfer2 without arguments: %q", out)
	}
	// event_notify through luarun / lua
	if out := h.sw.API("luarun app.lua event_notify internal check_sync 1000@" + cfDomain); !strings.HasPrefix(out, "+OK") {
		t.Fatalf("luarun app.lua event_notify: %q", out)
	}
	if out := h.sw.API("lua app.lua event_notify internal check_sync 1000@" + cfDomain + " x"); !strings.Contains(out, "+OK sent to") {
		t.Fatalf("lua app.lua event_notify: %q", out)
	}
	// monitor: stops at once when asked
	if out := h.sw.API("lua monitor.lua stop"); strings.HasPrefix(out, "-ERR") {
		t.Fatal(out)
	}
	// transfer2: KILL the first party
	if out := h.sw.API("lua transfer2.lua " + uuid + " ::KILL:: ::KILL::"); strings.HasPrefix(out, "-ERR") {
		t.Fatal(out)
	}
	waitUntil(t, "transfer2 to end the call", 3*time.Second, func() bool { return len(h.sw.Channels()) == 0 })

	// bind_digit_bleg runs its applications and the call goes on
	call, err := alice.Dial("8940", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("bind_digit_bleg call: %v", err)
	}
}

// dial_string.lua: a phone saves its contact as an extension's dial string (id and voicemail password asked).
func TestScriptDialString(t *testing.T) {
	h := cfHarness(t, `<extension name="ds"><condition field="destination_number" expression="^8910$">
    <action application="set" data="domain_uuid=du1"/><action application="set" data="direction=in"/>
    <action application="set" data="extension=true"/><action application="set" data="sip_from_user=1000"/>
    <action application="set" data="sip_from_host=127.0.0.1"/><action application="lua" data="dial_string.lua"/>
  </condition></extension>`, nil)
	db := &cfDB{rows: func(q string, a []any) []map[string]string {
		switch {
		case strings.Contains(q, "FROM v_extensions AS e"):
			return []map[string]string{{"extension_uuid": "eu1", "domain_uuid": "du1", "extension": "1002", "dial_string": ""}}
		case strings.Contains(q, "v_voicemails"):
			return []map[string]string{{"voicemail_password": "9999"}}
		}
		return nil
	}}
	db.install(h)
	call, err := h.phone("1000").Dial("8910", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("call: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	dtmf(call, "1002#")
	time.Sleep(900 * time.Millisecond)
	dtmf(call, "9999#")
	waitUntil(t, "the dial string saved", 4*time.Second, func() bool {
		return db.has("UPDATE v_extensions SET dial_string", "sofia_contact(1000@127.0.0.1)", "eu1")
	})
}

// fifo: callers wait, members are rung, consumers take callers; extension_queue and fifo_member scripts.
func TestFifo(t *testing.T) {
	h := cfHarness(t, `<extension name="fin"><condition field="destination_number" expression="^8901$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="fifo" data="q1@127.0.0.1 in"/>
  </condition></extension>
  <extension name="fout"><condition field="destination_number" expression="^8902$">
    <action application="set" data="domain_name=127.0.0.1"/><action application="fifo" data="q2@127.0.0.1 out wait"/>
  </condition></extension>
  <extension name="fin2"><condition field="destination_number" expression="^8903$">
    <action application="fifo" data="q2@127.0.0.1 in"/>
  </condition></extension>
  <extension name="eq"><condition field="destination_number" expression="^8904$">
    <action application="set" data="extension_queue=queue_1003@127.0.0.1"/><action application="lua" data="extension_queue.lua"/>
  </condition></extension>
  <extension name="fm"><condition field="destination_number" expression="^8905$">
    <action application="set" data="user_name=1002"/><action application="set" data="queue_name=q3@127.0.0.1"/>
    <action application="lua" data="fifo_member.lua"/>
  </condition></extension>`, nil)
	alice, bob, carol := h.phone("1000"), h.phone("1001"), h.phone("1002")

	// a member is rung for the waiting caller
	if out := h.sw.API("fifo_member add q1@127.0.0.1 {fifo_member_wait=nowait}user/1001@127.0.0.1 1 10 0"); !strings.HasPrefix(out, "+OK") {
		t.Fatal(out)
	}
	if out := strings.TrimSpace(h.sw.API("fifo count q1@127.0.0.1")); out != "q1@127.0.0.1:0:0:1:0:1" {
		t.Fatalf("fifo count: %q", out)
	}
	done := make(chan *testua.Call, 1)
	go func() { c, _ := alice.Dial("8901", []int{0}, 15*time.Second, false); done <- c }()
	in, err := bob.WaitInvite(6 * time.Second)
	if err != nil {
		t.Fatalf("the member was not rung: %v", err)
	}
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	acall := <-done
	if acall == nil || acall.Final.StatusCode != 200 {
		t.Fatal("the caller was not connected to the member")
	}
	if !rtpFlows(acall.RTP, acall.RemoteRTP, in.RTP) {
		t.Fatal("no audio between the caller and the member")
	}
	h.sw.API("fifo_member del q1@127.0.0.1 {fifo_member_wait=nowait}user/1001@127.0.0.1")
	if out := strings.TrimSpace(h.sw.API("fifo count q1@127.0.0.1")); !strings.Contains(out, ":0:0:0:") {
		t.Fatalf("after the member left: %q", out)
	}

	// a consumer (fifo out wait) takes the caller who comes later
	ccall, err := carol.Dial("8902", []int{0}, 5*time.Second, false)
	if err != nil || ccall.Final.StatusCode != 200 {
		t.Fatalf("consumer: %v", err)
	}
	waitUntil(t, "the consumer to wait", 3*time.Second, func() bool {
		return strings.HasPrefix(strings.TrimSpace(h.sw.API("fifo count q2@127.0.0.1")), "q2@127.0.0.1:1:")
	})
	dave := h.phone("1003")
	dcall, err := dave.Dial("8903", []int{0}, 5*time.Second, false)
	if err != nil || dcall.Final.StatusCode != 200 {
		t.Fatalf("fifo in: %v", err)
	}
	if !rtpFlows(dcall.RTP, dcall.RemoteRTP, ccall.RTP) {
		t.Fatal("the caller and the consumer are not connected")
	}
}

func TestFifoScripts(t *testing.T) {
	h := cfHarness(t, `<extension name="eq"><condition field="destination_number" expression="^8904$">
    <action application="set" data="extension_queue=queue_1003@127.0.0.1"/><action application="lua" data="extension_queue.lua"/>
  </condition></extension>
  <extension name="fm"><condition field="destination_number" expression="^8905$">
    <action application="set" data="user_name=1002"/><action application="set" data="queue_name=q3@127.0.0.1"/>
    <action application="lua" data="fifo_member.lua"/>
  </condition></extension>`, nil)
	alice := h.phone("1000")
	// extension_queue: the extension becomes a member when the queue has members; the caller waits
	h.sw.API("fifo_member add queue_1003@127.0.0.1 {fifo_member_wait=nowait}user/1002@127.0.0.1 1 10 10")
	call, err := alice.Dial("8904", []int{0}, 5*time.Second, false)
	if err != nil || call.Final.StatusCode != 200 {
		t.Fatalf("extension_queue call: %v", err)
	}
	waitUntil(t, "the caller to wait and the extension to be a member", 3*time.Second, func() bool {
		return strings.TrimSpace(h.sw.API("fifo count queue_1003@127.0.0.1")) == "queue_1003@127.0.0.1:0:1:2:0:2" ||
			strings.Contains(h.sw.API("fifo count queue_1003@127.0.0.1"), ":1:2:")
	})
	_, _ = call.Bye()

	// fifo_member.lua: 1 logs in, 2 logs out
	login := func(digit string) {
		c, err := alice.Dial("8905", []int{0}, 5*time.Second, false)
		if err != nil || c.Final.StatusCode != 200 {
			t.Fatalf("fifo_member call: %v", err)
		}
		time.Sleep(900 * time.Millisecond)
		dtmf(c, digit)
		time.Sleep(1500 * time.Millisecond)
	}
	login("1")
	if out := strings.TrimSpace(h.sw.API("fifo count q3@127.0.0.1")); !strings.Contains(out, ":0:0:1:") {
		t.Fatalf("after logging in: %q", out)
	}
	login("2")
	if out := strings.TrimSpace(h.sw.API("fifo count q3@127.0.0.1")); !strings.Contains(out, ":0:0:0:") {
		t.Fatalf("after logging out: %q", out)
	}
}
