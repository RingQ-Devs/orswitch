package core_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/testua"
)

// ---------------------------------------------------------------------------
// helpers

type dbExec struct {
	Query string
	Args  []any
}

// fakeDB stands in for the RingQ database: sel answers the SELECTs of a script by what the query
// contains, exec logs the writes.
type fakeDB struct {
	mu    sync.Mutex
	sel   func(q string, args []any) []map[string]string
	execs []dbExec
}

func (f *fakeDB) install(sw *core.Core) {
	sw.SetSQL(func(_ context.Context, q string, args ...any) ([]map[string]string, error) {
		if f.sel == nil {
			return nil, nil
		}
		return f.sel(q, args), nil
	})
	sw.SetSQLExec(func(_ context.Context, q string, args ...any) (int64, error) {
		f.mu.Lock()
		f.execs = append(f.execs, dbExec{q, args})
		f.mu.Unlock()
		return 1, nil
	})
}

// find returns the logged writes whose query contains all of the parts.
func (f *fakeDB) find(parts ...string) []dbExec {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []dbExec
next:
	for _, e := range f.execs {
		for _, p := range parts {
			if !strings.Contains(e.Query, p) {
				continue next
			}
		}
		out = append(out, e)
	}
	return out
}

func (f *fakeDB) waitExec(t *testing.T, what string, parts ...string) dbExec {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.find(parts...); len(got) > 0 {
			return got[len(got)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no database write for %s (%v)", what, parts)
	return dbExec{}
}

func hasArg(e dbExec, want string) bool {
	for _, a := range e.Args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}

// dtmf presses digits with SIP INFO.
func dmDTMF(t *testing.T, c *testua.Call, digits string) {
	t.Helper()
	for _, d := range digits {
		if resp, err := c.Request("INFO", "application/dtmf-relay", []byte(fmt.Sprintf("Signal=%c\r\nDuration=160\r\n", d))); err != nil || resp.StatusCode != 200 {
			t.Fatalf("INFO %c: %v %v", d, resp, err)
		}
		time.Sleep(120 * time.Millisecond)
	}
}

func dcDial(t *testing.T, ua *testua.UA, ext string) *testua.Call {
	t.Helper()
	c, err := ua.Dial(ext, []int{0}, 6*time.Second, false)
	if err != nil {
		t.Fatalf("dial %s: %v", ext, err)
	}
	return c
}

func dcOK(t *testing.T, ua *testua.UA, ext string) *testua.Call {
	t.Helper()
	c := dcDial(t, ua, ext)
	if c.Final.StatusCode != 200 {
		t.Fatalf("dial %s: %d", ext, c.Final.StatusCode)
	}
	return c
}

func dext(name, number, actions string) string {
	return fmt.Sprintf(`<extension name="%s"><condition field="destination_number" expression="^%s$">%s</condition></extension>`, name, number, actions)
}

func dact(app, data string) string { return fmt.Sprintf(`<action application="%s" data="%s"/>`, app, data) }

func discDialplan(extra ...string) string {
	return strings.Replace(testDialplan, `</context></include>`, strings.Join(extra, "\n")+`</context></include>`, 1)
}

func discHarness(t *testing.T, db *fakeDB, mutate func(*config.Config), extra ...string) *harness {
	t.Helper()
	users := "<include>"
	for _, u := range []string{"1000", "1001", "1002", "1003"} {
		users += fmt.Sprintf(`<user id="%s"><params><param name="password" value="%s"/></params><variables>
		<variable name="effective_caller_id_number" value="%s"/><variable name="effective_caller_id_name" value="Ext %s"/></variables></user>`, u, testPassword, u, u)
	}
	users += "</include>"
	h := newHarnessWith(t, "", users, discDialplan(extra...), mutate)
	if db != nil {
		db.install(h.sw)
	}
	return h
}

// answerNext waits for an INVITE to the phone and answers it.
func answerNext(t *testing.T, ua *testua.UA) *testua.Call {
	t.Helper()
	in, err := ua.WaitInvite(8 * time.Second)
	if err != nil {
		t.Fatalf("%s was not called: %v", ua.User, err)
	}
	_ = in.Ring()
	if err := in.Answer(0); err != nil {
		t.Fatal(err)
	}
	return in.Dialog()
}

// ---------------------------------------------------------------------------
// DISA

func TestDISAPin(t *testing.T) {
	h := discHarness(t, nil, nil, dext("disa", "8601",
		dact("set", "pin_number=1234")+dact("set", "digit_min_length=3")+dact("set", "digit_max_length=4")+dact("lua", "disa.lua")))
	alice, bob := h.phone("1000"), h.phone("1002")
	call := dcOK(t, alice, "8601")
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, call, "1234#")
	time.Sleep(1500 * time.Millisecond) // the script waits a second before asking for the number
	dmDTMF(t, call, "1002#")
	in, err := bob.WaitInvite(6 * time.Second)
	if err != nil {
		t.Fatalf("DISA did not dial 1002: %v", err)
	}
	if !strings.Contains(in.Invite.Get("From"), "1000") {
		t.Fatalf("caller id %s", in.Invite.Get("From"))
	}
	_ = in.Reject(486)
}

func TestDISAWrongPin(t *testing.T) {
	h := discHarness(t, nil, nil, dext("disa", "8601", dact("set", "pin_number=1234")+dact("lua", "disa.lua")))
	alice := h.phone("1000")
	call := dcOK(t, alice, "8601")
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, call, "9999#")
	h.waitChannels(0) // refused callers are hung up on
}

func TestDISAPlusCallingCard(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "'authentication'") && len(args) == 2 && args[0] == "555" && args[1] == "4321" {
			return []map[string]string{{"domain_uuid": "dom-1", "contact_uuid": "c-1"}}
		}
		return nil
	}}
	h := discHarness(t, db, nil, dext("disaplus", "8625",
		dact("set", "reference_number=555")+dact("set", "predefined_destination=1002")+dact("set", "context=127.0.0.1")+dact("lua", "disa.plus.lua")))
	alice, bob := h.phone("1000"), h.phone("1002")
	call := dcOK(t, alice, "8625")
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, call, "4321#")
	in, err := bob.WaitInvite(6 * time.Second)
	if err != nil {
		t.Fatalf("the card holder was not connected: %v", err)
	}
	_ = in.Reject(486)

	// a wrong PIN: refused
	call2 := dcOK(t, alice, "8625")
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, call2, "1111#")
	h.waitFor("the refused caller to be hung up", func() bool { return len(h.sw.Channels()) == 0 })
}

func TestDISACallbackLoopback(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "v_domains") {
			return []map[string]string{{"u": "dom-1"}}
		}
		return nil
	}}
	h := discHarness(t, db, nil)
	alice, bob := h.phone("1000"), h.phone("1001")
	done := make(chan string, 1)
	go func() { done <- h.sw.API("lua disa.callback.loopback.lua 1000 1001 default acct") }()
	a := answerNext(t, alice)
	b := answerNext(t, bob)
	h.waitFor("both legs up", func() bool { return len(h.sw.Channels()) >= 2 })
	_, _ = a.Bye()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("the callback script did not end with the call")
	}
	_, _ = b.Bye()
}

// ---------------------------------------------------------------------------
// wake-up calls

func TestWakeupSchedulesAndCancels(t *testing.T) {
	h := discHarness(t, nil, nil, dext("wakeup", "8624", dact("set", "wakeup_destination=1001")+dact("lua", "wakeup.lua")))
	alice := h.phone("1000")
	call := dcOK(t, alice, "8624")
	time.Sleep(1700 * time.Millisecond) // answer, 2 x 500 ms, the greeting
	dmDTMF(t, call, "08")
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, call, "30")
	h.waitFor("the wake-up call to be scheduled", func() bool { return h.sw.SchedCount() == 1 })
	// 2 at the confirmation cancels it (keys sent until the script is listening)
	deadline := time.Now().Add(15 * time.Second)
	for h.sw.SchedCount() != 0 && time.Now().Before(deadline) {
		dmDTMF(t, call, "2")
		time.Sleep(300 * time.Millisecond)
	}
	if h.sw.SchedCount() != 0 {
		t.Fatal("the wake-up call was not cancelled")
	}
}

func TestWakeupCallSnooze(t *testing.T) {
	h := discHarness(t, nil, nil)
	bob := h.phone("1001")
	// the scheduled call: originate ... &lua('wakeup.lua domain extension sound')
	outc := make(chan string, 1)
	go func() { outc <- h.sw.API("originate {snooze_time=60}user/1001 &lua('wakeup.lua 127.0.0.1 1001 silence_stream://100')") }()
	call := answerNext(t, bob)
	if out := <-outc; !strings.HasPrefix(out, "+OK") {
		t.Fatal(out)
	}
	time.Sleep(1200 * time.Millisecond)
	dmDTMF(t, call, "2") // snooze
	h.waitFor("the snooze to be scheduled", func() bool { return h.sw.SchedCount() == 1 })
	if out := h.sw.API("sched_del wakeup-call-1001"); !strings.Contains(out, "Deleted: 1") {
		t.Fatal(out)
	}
}

// ---------------------------------------------------------------------------
// recordings

func TestRecordingsStarCode(t *testing.T) {
	dir := t.TempDir()
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "v_domains") {
			return []map[string]string{{"u": "dom-1"}}
		}
		return nil
	}}
	h := discHarness(t, db, func(c *config.Config) { c.Settings.RecordingsDir = dir }, dext("rec", "8602",
		dact("set", "domain_name=127.0.0.1")+dact("lua", "recordings.lua")))
	alice := h.phone("1000")
	call := dcOK(t, alice, "8602")
	time.Sleep(1400 * time.Millisecond) // answer, then a second before the id is asked
	dmDTMF(t, call, "7#")
	time.Sleep(700 * time.Millisecond)
	for i := 0; i < 25; i++ { // speech
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i), uint32(i*160), ulaw(3000, 160))
		time.Sleep(20 * time.Millisecond)
	}
	dmDTMF(t, call, "#") // ends the recording
	e := db.waitExec(t, "the recording row", "INSERT INTO v_recordings")
	if !hasArg(e, "7.wav") {
		t.Fatalf("row %v", e.Args)
	}
	if got := db.find("DELETE FROM v_recordings"); len(got) == 0 {
		t.Fatal("the old row was not removed")
	}
	if st, err := os.Stat(filepath.Join(dir, "127.0.0.1", "7.wav")); err != nil || st.Size() < 44 {
		t.Fatalf("recording file: %v", err)
	}
	// 1 keeps it; the call ends
	deadline := time.Now().Add(10 * time.Second)
	for len(h.sw.Channels()) != 0 && time.Now().Before(deadline) {
		dmDTMF(t, call, "1")
	}
	if len(h.sw.Channels()) != 0 {
		t.Fatal("the call did not end after saving")
	}
}

// ---------------------------------------------------------------------------
// call centre announcements

func TestCallcenterAnnouncePosition(t *testing.T) {
	var h *harness
	h = tenantHarness(t, nil, queueConfig("longest-idle-agent", func(q *config.Queue, agents []*config.Agent) {
		for _, a := range agents {
			a.Status = config.AgentOnBreak
		}
	}))
	snd := h.dir
	for _, f := range []string{"ivr/ivr-you_are_number.wav", "digits/1.wav", "ivr/ivr-in-the-queue.wav"} {
		p := filepath.Join(snd, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		tone := make([]int16, 4000)
		for i := range tone {
			tone[i] = int16(8000 * ((i / 8) % 2 * 2 - 1))
		}
		if err := writeWAV(p, tone); err != nil {
			t.Fatal(err)
		}
	}
	caller := h.tenantPhone("1000", "a.example")
	call := dcOK(t, caller, "3000")
	var uuid string
	h.waitFor("the caller in the queue", func() bool {
		for _, ch := range h.sw.Channels() {
			if ch.Var("caller_id_number") == "1000" {
				uuid = ch.UUID
			}
		}
		return uuid != ""
	})
	if out := h.sw.API("luarun callcenter-announce-position.lua " + uuid + " 3000@a.example 300"); !strings.HasPrefix(out, "+OK") {
		t.Fatal(out)
	}
	heard := false
	for i := 0; i < 150 && !heard; i++ {
		_, p, err := testua.RecvRTP(call.RTP, 40*time.Millisecond, nil)
		if err == nil && len(p) > 0 && p[0] != 0xFF && p[0] != 0x7F {
			heard = true
		}
	}
	if !heard {
		t.Fatal("the caller did not hear the position announcement")
	}
	_, _ = call.Bye()
	h.waitChannels(0)
}

// ---------------------------------------------------------------------------
// email, roku, ha_monitor, handler

func TestEmailScriptQueuesMail(t *testing.T) {
	db := &fakeDB{}
	h := discHarness(t, db, nil)
	out := h.sw.API(`lua email.lua boss@example.com noreply@example.com hdr "The subject" "It&#39;s here"`)
	if strings.HasPrefix(out, "-ERR") {
		t.Fatal(out)
	}
	e := db.waitExec(t, "the mail", "INSERT INTO v_email_queue (")
	if !hasArg(e, "boss@example.com") || !hasArg(e, "The subject") || !hasArg(e, "It's here") || !hasArg(e, "noreply@example.com") {
		t.Fatalf("mail %v", e.Args)
	}
}

func TestRokuKeys(t *testing.T) {
	keys := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Method + " " + r.URL.Path
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	h := discHarness(t, nil, nil, dext("roku", "8622", dact("set", "host="+host)+dact("set", "roku_port="+port)+dact("lua", "roku.lua")))
	alice := h.phone("1000")
	call := dcOK(t, alice, "8622")
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, call, "2")
	select {
	case k := <-keys:
		if k != "POST /keypress/Up" {
			t.Fatalf("key %s", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no key reached the Roku")
	}
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, call, "*") // ends the call
	h.waitChannels(0)
}

func TestHAMonitorPassesCacheFlush(t *testing.T) {
	type req struct{ path, query, user, pass string }
	got := make(chan req, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		got <- req{r.URL.Path, r.URL.RawQuery, u, p}
		_, _ = io.WriteString(w, "+OK")
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	h := discHarness(t, nil, nil)
	h.sw.API(fmt.Sprintf(`global_setvar ha_monitor_servers=[{"method":"curl","username":"fs","password":"pw","hostname":"%s","port":"%s"}]`, host, port))
	if out := h.sw.API("luarun ha_monitor.lua"); !strings.HasPrefix(out, "+OK") {
		t.Fatal(out)
	}
	time.Sleep(300 * time.Millisecond)
	h.sw.API("memcache flush")
	select {
	case r := <-got:
		if r.path != "/webapi/memcache" || r.query != "flush" || r.user != "fs" || r.pass != "pw" {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the flush did not reach the other server")
	}
	h.sw.API("memcache delete dialplan:x")
	select {
	case r := <-got:
		if r.query != "delete%20dialplan%3Ax" {
			t.Fatalf("%+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delete did not reach the other server")
	}
}

func TestHandlerHoldsCall(t *testing.T) {
	h := discHarness(t, nil, nil, dext("handler", "8623", dact("lua", "handler.lua")))
	alice := h.phone("1000")
	call := dcOK(t, alice, "8623")
	time.Sleep(1500 * time.Millisecond)
	if n := len(h.sw.Channels()); n != 1 {
		t.Fatalf("the call should be held open, %d channels", n)
	}
	if v := h.sw.Channels()[0].Var("STREAM_META_CALLER"); v != "1000" {
		t.Fatalf("STREAM_META_CALLER %q", v)
	}
	_, _ = call.Bye()
	h.waitChannels(0)
}

// ---------------------------------------------------------------------------
// app/ scripts

func TestAppCallBlock(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "v_call_block") {
			return []map[string]string{{"u": "blk-1", "app": "busy", "data": "", "cnt": "4"}}
		}
		return nil
	}}
	h := discHarness(t, db, nil, dext("blk", "8610", dact("set", "call_direction=inbound")+dact("lua", "app.lua call_block")+dact("answer", "")+dact("echo", "")))
	alice := h.phone("1000")
	call := dcDial(t, alice, "8610")
	if call.Final.StatusCode != 486 {
		t.Fatalf("a blocked caller should get busy, got %d", call.Final.StatusCode)
	}
	e := db.waitExec(t, "the block count", "UPDATE v_call_block")
	if e.Args[0] != 5 {
		t.Fatalf("count %v", e.Args)
	}
}

func TestAppSpeedDial(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "phone_speed_dial") && len(args) >= 2 && args[1] == "5" {
			return []map[string]string{{"n": "1001"}}
		}
		return nil
	}}
	h := discHarness(t, db, nil, dext("sd", "8611", dact("set", "domain_uuid=dom-1")+dact("lua", "app.lua speed_dial 5")))
	alice, bob := h.phone("1000"), h.phone("1001")
	go func() { _, _ = alice.Dial("8611", []int{0}, 8*time.Second, false) }()
	in, err := bob.WaitInvite(6 * time.Second)
	if err != nil {
		t.Fatalf("speed dial 5 did not call 1001: %v", err)
	}
	_ = in.Reject(486)
}

func TestAppTollAllow(t *testing.T) {
	actions := func(allow string) string {
		return dact("set", "default_country=IT") + dact("set", "toll_allow="+allow) + dact("lua", "app.lua toll_allow ${uuid}") + dact("answer", "") + dact("echo", "")
	}
	h := discHarness(t, nil, nil, dext("toll1", "8612", actions("mobile,landline")), dext("toll2", "8613", actions("unknown")),
		dext("toll3", "8614", dact("set", "default_country=IT")+dact("lua", "app.lua toll_allow ${uuid}")+dact("answer", "")))
	alice := h.phone("1000")
	if c := dcDial(t, alice, "8612"); c.Final.StatusCode == 200 {
		t.Fatal("an unknown toll type should be barred")
	}
	c := dcOK(t, alice, "8613") // unknown is allowed here
	_, _ = c.Bye()
	if c := dcDial(t, alice, "8614"); c.Final.StatusCode == 200 {
		t.Fatal("an extension without toll_allow is barred")
	}
}

func TestAppIsLocalAndCallerID(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		switch {
		case strings.Contains(q, "v_destinations"):
			return []map[string]string{{"n": "1001", "ctx": "default"}}
		case strings.Contains(q, "v_contact_phones"):
			return []map[string]string{{"org": "ACME Ltd", "fam": "", "giv": ""}}
		}
		return nil
	}}
	h := discHarness(t, db, nil,
		dext("local", "8615", dact("lua", "app.lua is_local")),
		dext("cid", "8616", dact("set", "domain_uuid=dom-1")+dact("lua", "app.lua caller_id")+dact("bridge", "user/1001")))
	alice, bob := h.phone("1000"), h.phone("1001")
	go func() { _, _ = alice.Dial("8615", []int{0}, 8*time.Second, false) }()
	in, err := bob.WaitInvite(6 * time.Second)
	if err != nil {
		t.Fatalf("is_local did not route to 1001: %v", err)
	}
	_ = in.Reject(486)
	time.Sleep(300 * time.Millisecond)
	go func() { _, _ = alice.Dial("8616", []int{0}, 8*time.Second, false) }()
	in, err = bob.WaitInvite(6 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in.Invite.Get("From"), "ACME Ltd") {
		t.Fatalf("the contact's name is not the caller id: %s", in.Invite.Get("From"))
	}
	_ = in.Reject(486)
}

func TestAppEmergencyAndMissedMail(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		switch {
		case strings.Contains(q, "v_default_settings") && strings.Contains(q, "'emergency'"):
			return []map[string]string{{"v": "security@example.com"}}
		case strings.Contains(q, "v_default_settings") && strings.Contains(q, "smtp_from"):
			return []map[string]string{{"v": "pbx@example.com"}}
		}
		return nil
	}}
	h := discHarness(t, db, nil,
		dext("em", "8617", dact("set", "domain_uuid=dom-1")+dact("set", "destination_number=911")+dact("lua", "app.lua emergency")+dact("answer", "")+dact("echo", "")),
		dext("miss", "8618", dact("set", "domain_uuid=dom-1")+dact("set", "originate_disposition=ORIGINATOR_CANCEL")+dact("set", "missed_call_app=email")+
			dact("set", "missed_call_data=boss@example.com")+dact("set", "api_hangup_hook=lua app.lua hangup")+dact("answer", "")+dact("echo", "")))
	alice := h.phone("1000")
	c := dcOK(t, alice, "8617")
	e := db.waitExec(t, "the emergency mail", "INSERT INTO v_email_queue (")
	if !hasArg(e, "security@example.com") || !hasArg(e, "911 Emergency Call") {
		t.Fatalf("mail %v", e.Args)
	}
	l := db.waitExec(t, "the emergency log", "INSERT INTO v_emergency_logs")
	if !hasArg(l, "911 Emergency Call") {
		t.Fatalf("log %v", l.Args)
	}
	_, _ = c.Bye()
	h.waitChannels(0)

	db.mu.Lock()
	db.execs = nil
	db.mu.Unlock()
	c = dcOK(t, alice, "8618")
	_, _ = c.Bye() // the hangup hook mails the missed call
	e = db.waitExec(t, "the missed call mail", "INSERT INTO v_email_queue (")
	if !hasArg(e, "boss@example.com") {
		t.Fatalf("mail %v", e.Args)
	}
}

func TestAppFaxHangupHook(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "FROM v_fax ") {
			return []map[string]string{{"e": "fax@example.com", "x": "9000", "f": "", "p": "", "n": "", "num": ""}}
		}
		return nil
	}}
	h := discHarness(t, db, nil, dext("fax", "8619", dact("set", "domain_uuid=dom-1")+dact("set", "fax_uuid=11111111-2222-3333-4444-555555555555")+
		dact("set", "fax_success=1")+dact("set", "fax_result_text=OK")+dact("set", "fax_file=/tmp/f1.tif")+dact("set", "fax_result_code=0")+
		dact("set", "api_hangup_hook=lua hangup_rx.lua")+dact("answer", "")+dact("echo", "")))
	alice := h.phone("1000")
	c := dcOK(t, alice, "8619")
	_, _ = c.Bye()
	db.waitExec(t, "the fax log", "INSERT INTO v_fax_logs")
	f := db.waitExec(t, "the fax file", "INSERT INTO v_fax_files")
	if !hasArg(f, "rx") || !hasArg(f, "/tmp/f1.tif") {
		t.Fatalf("file row %v", f.Args)
	}
	if m := db.waitExec(t, "the fax mail", "INSERT INTO v_email_queue ("); !hasArg(m, "fax@example.com") {
		t.Fatalf("mail %v", m.Args)
	}
}

func TestAppCallControlAndRaiseHand(t *testing.T) {
	db := &fakeDB{}
	h := discHarness(t, db, nil,
		dext("cc", "8620", dact("set", "target_group=support")+dact("set", "domain_name=x.example")+dact("set", "domain_uuid=dom-1")+dact("lua", "app.lua call_control")),
		dext("hand", "8621", dact("answer", "")+dact("lua", "app.lua dialplan")+dact("lua", "raise_hand.lua toggle")+dact("park", "")))
	alice := h.phone("1000")
	c := dcOK(t, alice, "8620")
	time.Sleep(300 * time.Millisecond)
	dmDTMF(t, c, "2")
	e := db.waitExec(t, "the extensions' context", "UPDATE v_extensions")
	if !hasArg(e, "limit") || !hasArg(e, "support") {
		t.Fatalf("update %v", e.Args)
	}
	_, _ = c.Bye()
	h.waitChannels(0)
	c = dcOK(t, alice, "8621")
	var uuid string
	h.waitFor("the call", func() bool {
		for _, ch := range h.sw.Channels() {
			if ch.Var("hand_raised") == "true" && ch.Var("call_direction") == "local" {
				uuid = ch.UUID
			}
		}
		return uuid != ""
	})
	_, _ = c.Bye()
}

func TestAppToolsEventNotifyAndAVMD(t *testing.T) {
	h := discHarness(t, nil, nil, dext("park", "8630", dact("answer", "")+dact("park", "")))
	bob := h.phone("1001")
	if out := h.sw.API("luarun app.lua event_notify internal reboot 1001@127.0.0.1 yealink"); !strings.HasPrefix(out, "+OK") {
		t.Fatal(out)
	}
	n, err := bob.WaitRequest("NOTIFY", 5*time.Second)
	if err != nil {
		t.Fatal("no NOTIFY:", err)
	}
	if n.Get("Event") != "check-sync;reboot=true" {
		t.Fatalf("Event %q", n.Get("Event"))
	}
	h.sw.API("luarun app.lua event_notify internal check_sync 1001@127.0.0.1 grandstream")
	n, err = bob.WaitRequest("NOTIFY", 5*time.Second)
	if err != nil || n.Get("Event") != "resync" {
		t.Fatalf("%v %v", n, err)
	}

	// avmd: a beep hangs the call up
	alice := h.phone("1000")
	call := dcOK(t, alice, "8630")
	h.waitChannels(1)
	if out := h.sw.API("luarun app.lua avmd"); !strings.HasPrefix(out, "+OK") {
		t.Fatal(out)
	}
	time.Sleep(200 * time.Millisecond)
	e := core.NewEvent(core.EvCustom, "avmd::beep")
	e.Add("Unique-ID", h.sw.Channels()[0].UUID)
	h.sw.Events().Publish(e)
	h.waitChannels(0)
	_ = call
}

func TestAppValetParkSlots(t *testing.T) {
	h := discHarness(t, nil, nil, dext("valet", "8631", dact("set", "valet_parking_direction=in")+dact("lua", "app.lua valet_park")))
	a, b := h.phone("1000"), h.phone("1001")
	dcOK(t, a, "8631")
	h.waitFor("slot 1", func() bool { return len(h.sw.ParkedSlots()) == 1 })
	dcOK(t, b, "8631")
	h.waitFor("slot 2", func() bool { return len(h.sw.ParkedSlots()) == 2 })
	got := strings.Join(h.sw.ParkedSlots(), " ")
	if !strings.Contains(got, "*5901") || !strings.Contains(got, "*5902") {
		t.Fatalf("slots %s", got)
	}
}

func TestAppAgentStatusToggle(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "v_call_center_agents") {
			return []map[string]string{{"u": "agent-1001", "agent_name": "1001", "agent_id": "1001", "user_uuid": ""}}
		}
		return nil
	}}
	dp := "<include>" + strings.Replace(portalContext("a.example"), `<extension name="local_extension" continue="true">`,
		dext("agent", "8660", dact("set", "agent_id=1001")+dact("set", "agent_authorized=true")+dact("set", "domain_uuid=dom-a")+dact("lua", "app.lua agent_status"))+
			`<extension name="local_extension" continue="true">`, 1) + "</include>"
	h := newHarnessWith(t, "", tenantUsers(nil), dp, queueConfig("longest-idle-agent", nil))
	db.install(h.sw)
	phone := h.tenantPhone("1002", "a.example")
	call := dcOK(t, phone, "8660")
	// five seconds later, as the Lua did
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := agentStatus(h, "agent-1001"); st == config.AgentLoggedOut {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st, _ := agentStatus(h, "agent-1001"); st != config.AgentLoggedOut {
		t.Fatalf("the agent is %q", st)
	}
	_, _ = call.Bye()
}

func TestConferenceFlagMuteAndCenter(t *testing.T) {
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		switch {
		case strings.Contains(q, "v_conference_centers"):
			return []map[string]string{{"u": "cc-1", "g": ""}}
		case strings.Contains(q, "v_conference_rooms") && len(args) > 1 && args[1] == "9999":
			return []map[string]string{{"u": "room-1", "name": "board", "record": "false", "profile": "", "max_members": "", "wait_mod": "",
				"endconf": "", "mpin": "1111", "ppin": "9999", "announce_name": "", "announce_count": "", "announce_recording": "", "mute": "true", "sounds": ""}}
		}
		return nil
	}}
	h := discHarness(t, db, nil, dext("conf", "8640", dact("conference", "r1+flags{|mute}")),
		dext("center", "8641", dact("set", "domain_uuid=dom-1")+dact("lua", "app.lua conference_center")))
	a, b := h.phone("1000"), h.phone("1001")
	dcOK(t, a, "8640")
	h.waitFor("the muted member", func() bool {
		r := h.sw.Conferences("")
		return len(r) == 1 && len(r[0].Members) == 1 && r[0].Members[0].Muted
	})
	c := dcOK(t, b, "8641")
	time.Sleep(1500 * time.Millisecond)
	dmDTMF(t, c, "9999#")
	h.waitFor("the center's room with its participant muted", func() bool {
		for _, r := range h.sw.Conferences("") {
			if strings.Contains(r.Name, "room-1") && len(r.Members) == 1 && r.Members[0].Muted {
				return true
			}
		}
		return false
	})
}

func TestVoicemailEmailAndDestinations(t *testing.T) {
	vmDir := ""
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		switch {
		case strings.Contains(q, "v_voicemail_destinations"):
			return []map[string]string{{"u": "vm-1002"}}
		case strings.Contains(q, "voicemail_mail_to"):
			return []map[string]string{{"mail_to": "owner@example.com", "file": "attach", "keep": "false", "descr": "", "transcribe": "", "domain_uuid": "dom-a"}}
		}
		return nil
	}}
	h := tenantHarness(t, nil, func(cfg *config.Config) {
		vmDir = filepath.Join(cfg.Dir, "vm")
		cfg.Settings.VoicemailDir = vmDir
		cfg.Features.Voicemails["1001@a.example"] = &config.Voicemail{UUID: "vm-1001", Domain: "a.example", ID: "1001", Enabled: true}
		cfg.Features.Voicemails["1002@a.example"] = &config.Voicemail{UUID: "vm-1002", Domain: "a.example", ID: "1002", Enabled: true}
	})
	db.install(h.sw)
	caller := h.tenantPhone("1000", "a.example")
	call := dcOK(t, caller, "1001")
	for i := 0; i < 450; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 0, uint16(i), uint32(i*160), ulaw(2000, 160))
		time.Sleep(20 * time.Millisecond)
	}
	_, _ = call.Bye()
	m := db.waitExec(t, "the mail", "INSERT INTO v_email_queue (")
	if !hasArg(m, "owner@example.com") || m.Args[8] != "delete" {
		t.Fatalf("mail %v", m.Args)
	}
	if got := db.waitExec(t, "the attachment", "INSERT INTO v_email_queue_attachments"); !hasArg(got, "audio/x-wav") {
		t.Fatalf("attachment %v", got.Args)
	}
	db.waitExec(t, "the copy", "INSERT INTO v_voicemail_messages")
	files, _ := filepath.Glob(filepath.Join(vmDir, "a.example", "1002", "msg_*.wav"))
	if len(files) != 1 {
		t.Fatalf("the copy for 1002: %v", files)
	}
}

func TestVoicemailPasswordForOtherBoxes(t *testing.T) {
	vmDir := ""
	dp := "<include>" + strings.Replace(portalContext("a.example"), `<extension name="local_extension" continue="true">`,
		dext("vmcheck", "8650", dact("answer", "")+dact("voicemail", "check default a.example 1001"))+`<extension name="local_extension" continue="true">`, 1) + "</include>"
	h := newHarnessWith(t, "", tenantUsers(nil), dp, func(cfg *config.Config) {
		vmDir = filepath.Join(cfg.Dir, "vm")
		cfg.Settings.VoicemailDir = vmDir
		cfg.Features.Voicemails["1001@a.example"] = &config.Voicemail{UUID: "vm-1001", Domain: "a.example", ID: "1001", Password: "2468", Enabled: true}
	})
	box := filepath.Join(vmDir, "a.example", "1001")
	_ = os.MkdirAll(box, 0o755)
	tone := make([]int16, 8000)
	for i := range tone {
		tone[i] = int16(4000 * ((i / 10) % 2))
	}
	msg := filepath.Join(box, "msg_m1.wav")
	if err := writeWAV(msg, tone); err != nil {
		t.Fatal(err)
	}
	p0 := h.tenantPhone("1000", "a.example")

	// the wrong password three times: the call is ended and the message is untouched
	call := dcOK(t, p0, "8650")
	for i := 0; i < 3; i++ {
		time.Sleep(500 * time.Millisecond)
		dmDTMF(t, call, "1111#")
	}
	h.waitChannels(0)
	if _, err := os.Stat(msg); err != nil {
		t.Fatal("the message went")
	}

	// the right one: the mailbox is listened to (7 deletes)
	call = dcOK(t, p0, "8650")
	time.Sleep(500 * time.Millisecond)
	dmDTMF(t, call, "2468#")
	time.Sleep(700 * time.Millisecond)
	for i := 0; i < 3; i++ {
		_ = testua.SendRTP(call.RTP, call.RemoteRTP, 101, uint16(10+i), 8000, []byte{7, 0x80, 0, 160})
	}
	h.waitFor("the message to be deleted", func() bool { _, err := os.Stat(msg); return os.IsNotExist(err) })
	_, _ = call.Bye()
}

func TestClearCacheScript(t *testing.T) {
	cache := t.TempDir()
	_ = os.WriteFile(filepath.Join(cache, "directory.1000@x"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(cache, "keep.me"), []byte("x"), 0o644)
	db := &fakeDB{sel: func(q string, args []any) []map[string]string {
		if strings.Contains(q, "v_default_settings") && len(args) >= 2 && args[0] == "cache" {
			return []map[string]string{{"v": cache}}
		}
		return nil
	}}
	h := discHarness(t, db, nil)
	h.sw.API("lua clear_cache.lua delete directory.1000@x")
	if _, err := os.Stat(filepath.Join(cache, "directory.1000@x")); !os.IsNotExist(err) {
		t.Fatal("the cached file stays")
	}
	h.sw.API("lua clear_cache.lua flush")
	if _, err := os.Stat(filepath.Join(cache, "keep.me")); !os.IsNotExist(err) {
		t.Fatal("flush left a file")
	}
}
