package db_test

// End-to-end check of what the Reports page needs from the switch.
//
// A copy of the switch (configuration from a private copy of the RingQ database) takes real SIP calls from test
// phones. RingQ Core's own node service (server.js, unmodified, test/reports_e2e/harness.js) listens on the switch's
// event socket and queues the call records; Core's own PHP import (MainTask calldetailrecord) writes them into
// q_ringq_cdr; Core's own report classes (ReportGenerator / HubReports) are then run on them.
//
// Needs the switch server (node, php with phalcon, postgres binaries, Core in /var/www/ringq) and root:
//
//	OR_REPORTS_E2E=1 go test ./internal/db -run TestReportsE2E -count=1 -v
//
// The private PostgreSQL and Redis are created by test/reports_e2e/sandbox.sh (done here when missing). The live
// database, Redis, switch and Core are only read.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/db"
	"github.com/ringq-devs/orswitch/internal/esl"
	"github.com/ringq-devs/orswitch/internal/sip"
	"github.com/ringq-devs/orswitch/internal/testua"
)

const e2eDomain = "switch.example.com"

type e2e struct {
	t       *testing.T
	sbx     string
	out     string
	sw      *core.Core
	store   *db.Store
	addr    netip.AddrPort
	eslAddr string
	pw      map[string]string // SIP user (alias) -> password
	phones  map[string]*testua.UA
	mu      sync.Mutex
	events  []map[string]any
	log     []string
}

func (e *e2e) note(format string, args ...any) {
	s := fmt.Sprintf("%s  %s", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
	e.mu.Lock()
	e.log = append(e.log, s)
	e.mu.Unlock()
	e.t.Log(s)
}

func sh(t *testing.T, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", script, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// startE2E prepares the sandbox, the switch and the phones.
func startE2E(t *testing.T, cdrTables string) *e2e {
	t.Helper()
	if os.Getenv("OR_REPORTS_E2E") == "" {
		t.Skip("OR_REPORTS_E2E not set")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root (runs node and psql against the private sandbox)")
	}
	e := &e2e{t: t, sbx: os.Getenv("OR_SBX"), out: os.Getenv("OR_E2E_OUT"), pw: map[string]string{}, phones: map[string]*testua.UA{}}
	if e.sbx == "" {
		e.sbx = "/tmp/rq_reports_sbx"
	}
	if e.out == "" {
		e.out = "/tmp/rq_reports_out"
	}
	_ = os.RemoveAll(e.out)
	_ = os.MkdirAll(e.out, 0o755)
	repo, _ := filepath.Abs("../..")
	helpers := filepath.Join(repo, "test", "reports_e2e")
	const work = "/tmp/rq_reports_e2e"
	_ = os.MkdirAll(work, 0o755)
	for _, f := range []string{"harness.js", "sandbox.sh", "prepare_core.sh", "run_reports.php"} {
		b, err := os.ReadFile(filepath.Join(helpers, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = exec.Command("chmod", "-R", "a+rX", work).Run()
	// A fresh copy of the database and Redis every run.
	_ = exec.Command("bash", filepath.Join(work, "sandbox.sh"), "down").Run()
	t.Log(sh(t, filepath.Join(work, "sandbox.sh"), "up"))
	t.Cleanup(func() {
		if os.Getenv("OR_E2E_KEEP") == "" {
			_ = exec.Command("bash", filepath.Join(work, "sandbox.sh"), "down").Run()
		}
	})
	t.Log(sh(t, filepath.Join(work, "prepare_core.sh"), "-f"))

	dir := t.TempDir()
	conf := fmt.Sprintf(`<configuration>
  <vars><var name="domain" value="127.0.0.1"/></vars>
  <settings>
    <param name="sip-ip" value="127.0.0.1"/>
    <param name="sip-port" value="0"/>
    <param name="enable-tcp" value="false"/>
    <param name="rtp-ip" value="127.0.0.1"/>
    <param name="rtp-start-port" value="52000"/>
    <param name="rtp-end-port" value="52400"/>
    <param name="event-socket-listen" value=""/>
    <param name="cdr-csv" value=""/>
    <param name="sounds-dir" value="/var/lib/orswitch/sounds"/>
    <param name="recordings-dir" value="%s/rec"/>
    <param name="voicemail-dir" value="%s/vm"/>
    <param name="codec-prefs" value="PCMU,PCMA"/>
    <param name="auth-calls" value="true"/>
    <param name="call-timeout" value="20"/>
    <param name="redis-addr" value="unix:%s/redis.sock"/>
  </settings>
  <database>
    <param name="dsn" value="host=%s user=postgres dbname=ringq"/>
    <param name="cdr-tables" value="%s"/>
    <param name="write-registrations" value="true"/>
    <param name="write-channels" value="true"/>
    <param name="poll-seconds" value="0"/>
    <param name="default-domain" value="%s"/>
    <param name="manage-schema" value="true"/>
  </database>
  <gateways/>
</configuration>`, dir, dir, e.sbx, e.sbx, cdrTables, e2eDomain)
	if err := os.WriteFile(filepath.Join(dir, "orswitch.xml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	level := slog.LevelError
	if os.Getenv("OR_TEST_DEBUG") != "" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx := context.Background()
	store, err := db.Open(ctx, cfg.Database, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Overlay(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Nothing of the real world: no carrier registration, no extra SIP ports.
	cfg.Gateways = nil
	cfg.Profiles = nil
	sw, err := core.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sw.Stop)
	w := store.StartWriter(sw)
	t.Cleanup(w.Close)
	srv, err := esl.Listen("127.0.0.1:0", "e2e-pass", sw, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	e.sw, e.store, e.eslAddr = sw, store, srv.Addr()
	e.addr = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(sw.SIPPort()))

	// every event the switch publishes, for the record
	sub := sw.Events().Subscribe(1<<16, func(ev *core.Event) bool { return true })
	go func() {
		for ev := range sub.C {
			h := map[string]string{}
			for _, kv := range ev.Headers {
				h[kv[0]] = kv[1]
			}
			e.mu.Lock()
			e.events = append(e.events, map[string]any{"name": ev.Name, "headers": h})
			e.mu.Unlock()
		}
	}()

	// phones: the extensions' SIP users and passwords come from the (private) database
	conn, err := pgx.Connect(ctx, "host="+e.sbx+" user=postgres dbname=ringq")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `select number_alias, password from v_extensions e join v_domains d on d.domain_uuid = e.domain_uuid
		where d.domain_name = $1`, e2eDomain)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var alias, pass string
		if err := rows.Scan(&alias, &pass); err != nil {
			t.Fatal(err)
		}
		e.pw[alias] = pass
	}
	rows.Close()
	return e
}

// phone registers (once) the extension's phone, as the apps do: with the SIP alias.
func (e *e2e) phone(ext string) *testua.UA {
	e.t.Helper()
	if ua := e.phones[ext]; ua != nil {
		return ua
	}
	var alias string
	conn, err := pgx.Connect(context.Background(), "host="+e.sbx+" user=postgres dbname=ringq")
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(context.Background(), `select number_alias from v_extensions e join v_domains d on d.domain_uuid = e.domain_uuid
		where d.domain_name = $1 and e.extension = $2`, e2eDomain, ext).Scan(&alias); err != nil {
		e.t.Fatalf("extension %s: %v", ext, err)
	}
	var resp *sip.Message
	var ua *testua.UA
	for _, user := range []string{alias, ext} {
		ua, err = testua.New(user, e.pw[alias], e.addr)
		if err != nil {
			e.t.Fatal(err)
		}
		ua.Domain = e2eDomain
		e.t.Cleanup(ua.Close)
		if resp, err = ua.Register(300); err == nil && resp.StatusCode == 200 {
			break
		}
	}
	if err != nil || resp.StatusCode != 200 {
		e.t.Fatalf("register %s (%s): %v %v", ext, alias, resp, err)
	}
	e.phones[ext] = ua
	return ua
}

func (e *e2e) api(cmd string) string { return e.sw.API(cmd) }

// ---------------------------------------------------------------------------
// scenarios

type scenario struct {
	Name, From, To string
	Start, End     int64
}

func (e *e2e) mark(s *scenario, f func()) {
	s.Start = time.Now().Unix()
	e.note("scenario %s: %s -> %s", s.Name, s.From, s.To)
	f()
	s.End = time.Now().Unix()
}

// talk answers an incoming call on `callee`, lets it run, and hangs up from the chosen side.
func (e *e2e) answered(from, to, callee string, talk time.Duration, calleeHangs bool) {
	e.t.Helper()
	a, b := e.phone(from), e.phone(callee)
	type res struct {
		c   *testua.Call
		err error
	}
	done := make(chan res, 1)
	go func() {
		c, err := a.Dial(to, []int{0}, 30*time.Second, false)
		done <- res{c, err}
	}()
	in, err := b.WaitInvite(10 * time.Second)
	if err != nil {
		e.t.Fatalf("%s did not ring for %s->%s: %v", callee, from, to, err)
	}
	_ = in.Ring()
	time.Sleep(600 * time.Millisecond)
	if err := in.Answer(0); err != nil {
		e.t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.c.Final.StatusCode != 200 {
		e.t.Fatalf("call not answered: %v %+v", r.err, r.c)
	}
	time.Sleep(talk)
	if calleeHangs {
		_, _ = in.Dialog().Bye()
		_, _ = a.WaitRequest("BYE", 5*time.Second)
	} else {
		_, _ = r.c.Bye()
		if bye, err := b.WaitRequest("BYE", 5*time.Second); err == nil {
			_ = b.Respond(bye, 200, "", nil, "")
		}
	}
	time.Sleep(1500 * time.Millisecond)
}

// unanswered: the callee rings and the caller gives up (or the callee rejects).
func (e *e2e) unanswered(from, to, callee string, ring time.Duration, reject int) {
	e.t.Helper()
	a, b := e.phone(from), e.phone(callee)
	var c *testua.Call
	var dialed chan *testua.Call
	if reject == 0 {
		var err error
		if c, err = a.Dial(to, []int{0}, 30*time.Second, true); err != nil {
			e.t.Fatal(err)
		}
	} else {
		dialed = make(chan *testua.Call, 1)
		go func() { c, _ := a.Dial(to, []int{0}, 30*time.Second, false); dialed <- c }()
	}
	in, err := b.WaitInvite(10 * time.Second)
	if err != nil {
		e.t.Fatalf("%s did not ring: %v", callee, err)
	}
	_ = in.Ring()
	if reject != 0 {
		_ = in.Reject(reject)
		// the dialplan sends the caller on (voicemail): the caller listens a moment, then hangs up
		if dc := <-dialed; dc != nil && dc.Final != nil && dc.Final.StatusCode == 200 {
			time.Sleep(2 * time.Second)
			_, _ = dc.Bye()
		}
		time.Sleep(2 * time.Second)
		return
	}
	time.Sleep(ring)
	_, _ = c.Cancel()
	_, _ = b.WaitRequest("CANCEL", 3*time.Second)
	time.Sleep(2 * time.Second)
}

// queueCall dials a queue; the first of the agent phones that rings answers after `wait` and talks.
func (e *e2e) queueCall(from, queue string, agents []string, wait, talk time.Duration, callerHangs bool, abandon bool) {
	e.t.Helper()
	a := e.phone(from)
	var uas []*testua.UA
	for _, ag := range agents {
		uas = append(uas, e.phone(ag))
	}
	type res struct {
		c   *testua.Call
		err error
	}
	done := make(chan res, 1)
	go func() {
		c, err := a.Dial(queue, []int{0}, 60*time.Second, false)
		done <- res{c, err}
	}()
	if abandon {
		// the caller hangs up while the queue still rings the agents
		// the queue has answered the caller (music): the caller hangs up while the agents ring
		r := <-done
		time.Sleep(wait)
		if r.err == nil && r.c != nil {
			_, _ = r.c.Bye()
		}
		time.Sleep(3 * time.Second)
		return
	}
	got := make(chan *testua.Incoming, len(uas))
	for _, u := range uas {
		go func(u *testua.UA) {
			if in, err := u.WaitInvite(20 * time.Second); err == nil {
				got <- in
			}
		}(u)
	}
	var in *testua.Incoming
	select {
	case in = <-got:
	case <-time.After(20 * time.Second):
		e.t.Fatalf("no agent rang for %s", queue)
	}
	_ = in.Ring()
	time.Sleep(wait)
	if err := in.Answer(0); err != nil {
		e.t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.c.Final.StatusCode != 200 {
		e.t.Fatalf("queue call not answered: %v", r.err)
	}
	time.Sleep(talk)
	if callerHangs {
		_, _ = r.c.Bye()
		if bye, err := in.UA.WaitRequest("BYE", 5*time.Second); err == nil {
			_ = in.UA.Respond(bye, 200, "", nil, "")
		}
	} else {
		_, _ = in.Dialog().Bye()
		_, _ = a.WaitRequest("BYE", 5*time.Second)
	}
	time.Sleep(2 * time.Second)
}

// ---------------------------------------------------------------------------

func (e *e2e) startNode(targets string) (*exec.Cmd, io.WriteCloser, *bytes.Buffer) {
	e.t.Helper()
	outFile := filepath.Join(e.out, "harness.json")
	script := fmt.Sprintf("cd /tmp && env OR_ESL_PORT=%s OR_ESL_PASS=e2e-pass OR_SBX=%s OR_OUT=%s OR_TRACE_KEYS='%s' OR_TARGETS=%s node /tmp/rq_reports_e2e/harness.js",
		e.eslAddr[strings.LastIndex(e.eslAddr, ":")+1:], e.sbx, outFile, os.Getenv("OR_TRACE_KEYS"), targets)
	cmd := exec.Command("sh", "-c", script)
	stdin, _ := cmd.StdinPipe()
	logf, err := os.Create(filepath.Join(e.out, "harness.log"))
	if err != nil {
		e.t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	cmd.Stdout = io.MultiWriter(logf, buf)
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "HARNESS READY") {
			return cmd, stdin, buf
		}
		time.Sleep(200 * time.Millisecond)
	}
	e.t.Fatalf("node harness did not start:\n%s", buf.String())
	return nil, nil, nil
}

func (e *e2e) writeJSON(name string, v any) {
	b, _ := json.MarshalIndent(v, "", " ")
	_ = os.WriteFile(filepath.Join(e.out, name), b, 0o644)
}

func (e *e2e) psql(q string) string {
	cmd := exec.Command("psql", "-h", e.sbx, "-U", "postgres", "-d", "ringq", "-At", "-c", q)
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Logf("psql: %v %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *e2e) coreCLI(args ...string) string {
	cmd := exec.Command("php", append([]string{"app/cli.php"}, args...)...)
	cmd.Dir = "/tmp/rq_reports_core"
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// scenarios every test places: the calls a Reports page is made of.
func (e *e2e) scenarios() []*scenario {
	var sc []*scenario
	run := func(name, from, to string, f func()) {
		s := &scenario{Name: name, From: from, To: to}
		sc = append(sc, s)
		e.mark(s, f)
	}
	run("local-answered", "1002", "1003", func() { e.answered("1002", "1003", "1003", 3*time.Second, false) })
	run("local-no-answer", "1002", "1003", func() { e.unanswered("1002", "1003", "1003", 3*time.Second, 0) })
	run("local-rejected", "1002", "1003", func() { e.unanswered("1002", "1003", "1003", 0, 603) })
	run("queue-answered", "1004", "9000", func() { e.queueCall("1004", "9000", []string{"1000"}, 3*time.Second, 4*time.Second, true, false) })
	run("queue-agent-hangs", "1004", "9000", func() { e.queueCall("1004", "9000", []string{"1000"}, 2*time.Second, 3*time.Second, false, false) })
	run("queue-abandoned", "1004", "9000", func() { e.queueCall("1004", "9000", []string{"1000"}, 3*time.Second, 0, false, true) })
	return sc
}

// cdrRows reads the sandbox's q_ringq_cdr rows of the test, oldest first.
func (e *e2e) cdrRows(since int64) []map[string]any {
	conn, err := pgx.Connect(context.Background(), "host="+e.sbx+" user=postgres dbname=ringq")
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(), "select to_jsonb(t) - 'json' - 'xml' from q_ringq_cdr t where start_epoch >= $1 order by start_epoch, end_epoch", since)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var m map[string]any
		if err := rows.Scan(&m); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return fmt.Sprintf("%d", int64(v))
	case bool:
		return fmt.Sprintf("%t", v)
	}
	return fmt.Sprint(m[k])
}

func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

// runReports runs Core's report classes on the sandbox database.
func (e *e2e) runReports(since int64) map[string]json.RawMessage {
	spec := fmt.Sprintf(`{"domain":"2c757b32-5497-4e28-aee2-d94b92216bd7","tz":"UTC","start":%d,"end":0,"fqdn":%q}`, since, e2eDomain)
	cmd := exec.Command("php", "app/runner.php", spec)
	cmd.Dir = "/tmp/rq_reports_core"
	out, _ := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(e.out, "reports.json"), out, 0o644)
	var reports map[string]json.RawMessage
	if err := json.Unmarshal(out, &reports); err != nil {
		e.t.Fatalf("report runner output: %v\n%.600s", err, out)
	}
	return reports
}

// checkReports asserts what the Reports page shows for the six scenarios.
func (e *e2e) checkReports(reports map[string]json.RawMessage) {
	t := e.t
	for name, raw := range reports {
		var probe struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &probe)
		if probe.Error != "" {
			t.Errorf("report %s failed: %s", name, probe.Error)
		}
	}
	type table struct {
		Rows []map[string]any `json:"rows"`
	}
	get := func(name string) table {
		var tb table
		_ = json.Unmarshal(reports[name], &tb)
		return tb
	}
	// Overall (Queue Performance Overview): three queue calls, two answered by the agent, one abandoned
	ov := get("overall")
	if len(ov.Rows) != 3 {
		t.Errorf("overall: %d queue calls, want 3", len(ov.Rows))
	}
	answered, abandoned := 0, 0
	for _, r := range ov.Rows {
		switch str(r, "call_type") {
		case "Answered":
			answered++
			if str(r, "callee_id_number") != "1000" || str(r, "callee_id_name") == "--" {
				t.Errorf("overall: answered call without its agent: %v", r)
			}
			if str(r, "call_talk_time") == "--" || str(r, "call_time_to_answer") == "--" {
				t.Errorf("overall: answered call without times: %v", r)
			}
		case "Abandoned":
			abandoned++
		}
	}
	if answered != 2 || abandoned != 1 {
		t.Errorf("overall: %d answered, %d abandoned; want 2 and 1", answered, abandoned)
	}
	// Detailed queue statistics
	dq := get("detail_queue")
	if len(dq.Rows) != 1 || num(dq.Rows[0], "total_call") != 3 || num(dq.Rows[0], "answered_call") != 2 || num(dq.Rows[0], "abandoned_call") != 1 {
		t.Errorf("detail queue: %v", dq.Rows)
	}
	// the agent's talk time is the time with the caller (4 s and 3 s), not the call's total
	if len(dq.Rows) == 1 && num(dq.Rows[0], "talking_time") < 5 {
		t.Errorf("detail queue talk time %d s, want at least 5 (4 + 3)", num(dq.Rows[0], "talking_time"))
	}
	var sla struct {
		Rows []map[string]any `json:"rows"`
	}
	_ = json.Unmarshal(reports["sla"], &sla)
	if len(sla.Rows) != 1 || num(sla.Rows[0], "calls") != 3 || num(sla.Rows[0], "answered") != 2 || num(sla.Rows[0], "abandoned") != 1 {
		t.Errorf("SLA statistics: %v", sla.Rows)
	}
	if ab := get("abandoned"); len(ab.Rows) != 1 {
		t.Errorf("abandoned queue calls: %d, want 1", len(ab.Rows))
	}
	if wt := get("wait_time"); len(wt.Rows) != 1 || num(wt.Rows[0], "answered") != 2 {
		t.Errorf("answered by waiting time: %v", wt.Rows)
	}
	// Team queue and agent in queue statistics
	tq := get("hub:team_queue")
	found := false
	for _, r := range tq.Rows {
		if str(r, "queue") == "9000" {
			found = true
			if num(r, "calls") != 3 || num(r, "answered") != 2 || str(r, "top_agent") != "tyrone montgomery" {
				t.Errorf("team queue 9000: %v", r)
			}
		}
	}
	if !found {
		t.Errorf("team queue statistics: no row for queue 9000: %v", tq.Rows)
	}
	aq := get("hub:agent_queue")
	if len(aq.Rows) != 1 || str(aq.Rows[0], "extension") != "1000" || num(aq.Rows[0], "answered") != 2 {
		t.Errorf("agent in queue statistics: %v", aq.Rows)
	}
	// Extension statistics / user activity: 1002 and 1004 called, 1003 and 1000 were called
	exts := map[string]bool{}
	for _, r := range get("hub:ext_stats").Rows {
		exts[str(r, "extension")] = true
	}
	for _, ext := range []string{"1002", "1003", "1004", "1000"} {
		if !exts[ext] {
			t.Errorf("extension statistics: no row for %s: %v", ext, exts)
		}
	}
	if ua := get("hub:user_activity"); len(ua.Rows) == 0 {
		t.Errorf("user activity is empty")
	}
	if cd := get("hub:call_distribution"); len(cd.Rows) == 0 {
		t.Errorf("call distribution is empty")
	}
	if hm := get("hub:heatmap"); len(hm.Rows) == 0 {
		t.Errorf("call heat map is empty")
	}
}

func TestReportsE2E(t *testing.T) {
	e := startE2E(t, "v_xml_cdr")
	started := time.Now().Add(-2 * time.Second).Unix()
	targets := os.Getenv("OR_E2E_TARGETS") // e.g. server.js,queue.js
	if targets == "" {
		targets = "server.js"
	}
	cmd, stdin, _ := e.startNode(targets)
	sc := e.scenarios()
	e.api("hupall NORMAL_CLEARING")
	// let Core's node finish (journey rows are saved 4 s after a call ends)
	time.Sleep(8 * time.Second)
	_, _ = io.WriteString(stdin, "dump\n")
	_ = cmd.Wait()

	e.writeJSON("scenarios.json", sc)
	e.mu.Lock()
	e.writeJSON("events.json", e.events)
	e.mu.Unlock()
	_ = os.WriteFile(filepath.Join(e.out, "steps.log"), []byte(strings.Join(e.log, "\n")), 0o644)

	// what node queued, then Core's own import
	out, _ := exec.Command("redis-cli", "-s", e.sbx+"/redis.sock", "lrange", "callLogs-"+e2eDomain, "0", "-1").CombinedOutput()
	_ = os.WriteFile(filepath.Join(e.out, "callLogs.redis.txt"), out, 0o644)
	_ = os.WriteFile(filepath.Join(e.out, "import.log"), []byte(e.coreCLI("main", "calldetailrecord")), 0o644)
	_ = os.WriteFile(filepath.Join(e.out, "q_ringq_cdr.jsonl"), []byte(e.psql(fmt.Sprintf("select to_jsonb(t) - 'json' - 'xml' from q_ringq_cdr t where start_epoch >= %d order by start_epoch", started))), 0o644)
	_ = os.WriteFile(filepath.Join(e.out, "journeys.jsonl"), []byte(e.psql(fmt.Sprintf("select to_jsonb(t) from q_call_journeys t where start_epoch >= %d order by start_epoch", started))), 0o644)

	// Core's call records: one row per outcome, in Core's wording, with the agent, the waits and the talk time
	rows := e.cdrRows(started)
	byArg := map[string][]map[string]any{}
	for _, r := range rows {
		byArg[str(r, "last_arg")] = append(byArg[str(r, "last_arg")], r)
	}
	for arg, n := range map[string]int{"Inbound Answered": 1, "Inbound Caller Abandoned": 1, "Inbound Callee Busy": 1, "9000 - Queue Answered": 2, "Queue Caller Abandoned": 1} {
		if len(byArg[arg]) != n {
			t.Errorf("q_ringq_cdr: %d row(s) %q, want %d (rows: %d)", len(byArg[arg]), arg, n, len(rows))
		}
	}
	for _, r := range byArg["9000 - Queue Answered"] {
		if str(r, "cc_agent") != "4840ce81-4325-4770-9fa9-5c03236564a4" || str(r, "cc_queue") != "9000@"+e2eDomain || str(r, "caller_destination") != "1000" {
			t.Errorf("queue answered row without its agent/queue: %v", r)
		}
		if num(r, "billsec") < 3 || num(r, "duration") <= num(r, "billsec") || r["waitsec"] == nil {
			t.Errorf("queue answered row times (duration %d billsec %d waitsec %v): the call's total duration should exceed the agent's talk time",
				num(r, "duration"), num(r, "billsec"), r["waitsec"])
		}
	}
	for _, r := range byArg["Queue Caller Abandoned"] {
		if str(r, "cc_cause") != "cancel" || str(r, "cc_queue") != "9000@"+e2eDomain || str(r, "missed_call") != "true" {
			t.Errorf("abandoned queue row: %v", r)
		}
	}
	for _, r := range byArg["Inbound Answered"] {
		if str(r, "caller_id_number") != "1002" || str(r, "caller_destination") != "1003" || str(r, "missed_call") != "false" || num(r, "billsec") < 2 {
			t.Errorf("answered call row: %v", r)
		}
	}
	e.checkReports(e.runReports(started))
	t.Logf("output in %s", e.out)
}

// The same calls with no Core jobs (a test server): the switch's writer makes the q_ringq_cdr rows itself.
func TestReportsE2EDirect(t *testing.T) {
	e := startE2E(t, "v_xml_cdr,q_ringq_cdr")
	started := time.Now().Add(-2 * time.Second).Unix()
	e.scenarios()
	e.api("hupall NORMAL_CLEARING")
	time.Sleep(3 * time.Second)
	rows := e.cdrRows(started)
	e.writeJSON("direct-rows.json", rows)
	byArg := map[string]int{}
	for _, r := range rows {
		byArg[str(r, "last_arg")]++
	}
	for arg, n := range map[string]int{"Inbound Answered": 1, "Inbound Caller Abandoned": 1, "Inbound Callee Busy": 1, "9000 - Queue Answered": 2, "Queue Caller Abandoned": 1} {
		if byArg[arg] != n {
			t.Errorf("q_ringq_cdr: %d row(s) %q, want %d (all: %v)", byArg[arg], arg, n, byArg)
		}
	}
	e.checkReports(e.runReports(started))
}
