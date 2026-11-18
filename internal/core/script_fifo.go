package core

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FIFO queues (FreeSWITCH mod_fifo as FusionPBX uses it) and the scripts around them:
// fifo_member.lua, app/fifo member.lua (app.lua fifo member), extension_queue.lua.
//
//	fifo <name> in [<announce>|undef [<music>|undef]]   a caller waits (music) to be taken by a consumer
//	fifo <name> out [wait|nowait] [<announce>|undef [<music>|undef]]
//	                                                    a consumer takes the next waiting caller; "wait"
//	                                                    stays on and takes the next one after each call
//	fifo_member add <name> <dial string> [simo [timeout [lag]]]   (API, and ${fifo_member(...)})
//	fifo_member del <name> <dial string>
//	fifo count [<name>]                                 name:consumers:callers:members:ringing:idle
//
// A member is a dial string (user/1001@domain): while callers wait, the queue rings its members, up to
// "simo" calls at once, for "timeout" seconds, and a member rests "lag" seconds after a call (wrap-up).
// Queues that FusionPBX's FIFO page defines (v_fifo, v_fifo_members) get their members from the database
// the first time a caller or a consumer comes to them.

type fifoMember struct {
	dial      string
	simo      int
	timeout   time.Duration
	lag       time.Duration
	active    int // calls in progress
	busyUntil time.Time
}

type fifoCaller struct {
	ch     *Channel
	joined time.Time
}

type fifoConsumer struct {
	ch   *Channel
	take chan *fifoCaller
}

type fifoQueue struct {
	name      string
	mu        sync.Mutex
	loaded    bool
	callers   []*fifoCaller
	consumers []*fifoConsumer
	members   map[string]*fifoMember
	ringing   int
}

func fifoKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func (c *Core) fifo(name string, create bool) *fifoQueue {
	c.callfeat.mu.Lock()
	defer c.callfeat.mu.Unlock()
	if c.callfeat.fifos == nil {
		c.callfeat.fifos = map[string]*fifoQueue{}
	}
	q := c.callfeat.fifos[fifoKey(name)]
	if q == nil && create {
		q = &fifoQueue{name: strings.TrimSpace(name), members: map[string]*fifoMember{}}
		c.callfeat.fifos[fifoKey(name)] = q
	}
	return q
}

// fifoLoad adds the members the FIFO page of the portal defines (once per queue).
func (c *Core) fifoLoad(q *fifoQueue) {
	q.mu.Lock()
	if q.loaded {
		q.mu.Unlock()
		return
	}
	q.loaded = true
	q.mu.Unlock()
	rows, err := c.sqlRows(context.Background(), "SELECT m.member_contact, m.member_call_timeout, m.member_simultaneous, m.member_wrap_up_time "+
		"FROM v_fifo_members m JOIN v_fifo f ON f.fifo_uuid = m.fifo_uuid JOIN v_domains d ON d.domain_uuid = f.domain_uuid "+
		"WHERE (f.fifo_extension || '@' || d.domain_name = $1 OR f.fifo_name = $1) AND f.fifo_enabled = 'true' AND m.member_enabled = 'true'", q.name)
	if err != nil {
		return
	}
	for _, r := range rows {
		if dial := strings.TrimSpace(r["member_contact"]); dial != "" {
			c.fifoAddMember(q, dial, optNum(r["member_simultaneous"], 1), optNum(r["member_call_timeout"], 60), optNum(r["member_wrap_up_time"], 10))
		}
	}
}

func (c *Core) fifoAddMember(q *fifoQueue, dial string, simo, timeout, lag int) {
	if simo < 1 {
		simo = 1
	}
	if timeout < 1 {
		timeout = 60
	}
	if lag < 0 {
		lag = 0
	}
	q.mu.Lock()
	if m := q.members[dial]; m != nil {
		m.simo, m.timeout, m.lag = simo, time.Duration(timeout)*time.Second, time.Duration(lag)*time.Second
	} else {
		q.members[dial] = &fifoMember{dial: dial, simo: simo, timeout: time.Duration(timeout) * time.Second, lag: time.Duration(lag) * time.Second}
	}
	q.mu.Unlock()
	c.fifoDispatch(q)
}

// fifoDispatch matches waiting callers with waiting consumers and rings members for the rest.
func (c *Core) fifoDispatch(q *fifoQueue) {
	q.mu.Lock()
	// consumers (fifo out wait) first
	for len(q.callers) > 0 && len(q.consumers) > 0 {
		caller, cons := q.callers[0], q.consumers[0]
		q.callers, q.consumers = q.callers[1:], q.consumers[1:]
		select {
		case cons.take <- caller:
		default:
		}
	}
	var ring []*fifoMember
	free := len(q.callers) - q.ringing
	now := time.Now()
	names := make([]string, 0, len(q.members))
	for k := range q.members {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		m := q.members[k]
		if now.Before(m.busyUntil) {
			continue
		}
		for n := m.simo - m.active; n > 0 && free > 0; n-- {
			m.active++
			q.ringing++
			free--
			ring = append(ring, m)
		}
	}
	q.mu.Unlock()
	for _, m := range ring {
		go c.fifoRing(q, m)
	}
}

// fifoRing rings one member for a waiting caller and connects them when the member answers.
func (c *Core) fifoRing(q *fifoQueue, m *fifoMember) {
	finish := func(connected bool) {
		q.mu.Lock()
		m.active--
		if connected {
			m.busyUntil = time.Now().Add(m.lag)
		}
		q.mu.Unlock()
		if connected && m.lag > 0 {
			time.AfterFunc(m.lag+50*time.Millisecond, func() { c.fifoDispatch(q) })
		}
		c.fifoDispatch(q)
	}
	ringDone := func() {
		q.mu.Lock()
		q.ringing--
		q.mu.Unlock()
	}
	vars := map[string]string{"call_timeout": strconv.Itoa(int(m.timeout.Seconds())), "fifo_name": q.name}
	q.mu.Lock()
	if len(q.callers) > 0 {
		h := q.callers[0].ch
		vars["origination_caller_id_name"] = firstNonEmpty(h.Var("caller_id_name"), h.Var("caller_id_number"))
		vars["origination_caller_id_number"] = h.Var("caller_id_number")
	}
	q.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout+10*time.Second)
	defer cancel()
	leg, cause := c.originate(ctx, nil, m.dial, vars)
	ringDone()
	if leg == nil {
		c.log.Debug("fifo: member did not answer", "fifo", q.name, "member", m.dial, "cause", cause.String())
		finish(false)
		return
	}
	// the answered member takes the longest waiting caller
	q.mu.Lock()
	var caller *fifoCaller
	for len(q.callers) > 0 {
		cand := q.callers[0]
		q.callers = q.callers[1:]
		if !cand.ch.IsHungUp() {
			caller = cand
			break
		}
	}
	q.mu.Unlock()
	if caller == nil {
		leg.Hangup(CauseNormalClearing)
		finish(false)
		return
	}
	c.recordEvent("call", fmt.Sprintf("fifo %s: %s taken by %s", q.name, caller.ch.Var("caller_id_number"), m.dial))
	c.adopt(caller.ch)
	waitIdle(caller.ch)
	caller.ch.SetVar("fifo_bridge_uuid", leg.UUID)
	leg.SetVar("fifo_bridge_uuid", caller.ch.UUID)
	if !caller.ch.IsHungUp() {
		c.bridgeChannels(context.Background(), caller.ch, leg)
		if !caller.ch.IsHungUp() {
			caller.ch.Hangup(CauseNormalClearing)
		}
	} else {
		leg.Hangup(CauseNormalClearing)
	}
	finish(true)
}

func (q *fifoQueue) dropCaller(cl *fifoCaller) {
	q.mu.Lock()
	for i, x := range q.callers {
		if x == cl {
			q.callers = append(q.callers[:i], q.callers[i+1:]...)
			break
		}
	}
	q.mu.Unlock()
}

func fifoArg(v string) string {
	if v == "undef" || v == "_undef_" {
		return ""
	}
	return v
}

// appFifo is the fifo application.
func appFifo(ctx context.Context, c *Core, ch *Channel, data string) error {
	f := strings.Fields(data)
	if len(f) < 2 {
		return fmt.Errorf("fifo: usage: fifo <name> in|out ...")
	}
	q := c.fifo(f[0], true)
	c.fifoLoad(q)
	switch strings.ToLower(f[1]) {
	case "in":
		return c.fifoIn(ctx, q, ch, f[2:])
	case "out":
		return c.fifoOut(ctx, q, ch, f[2:])
	}
	return fmt.Errorf("fifo: %q is not in or out", f[1])
}

func (c *Core) fifoIn(ctx context.Context, q *fifoQueue, ch *Channel, args []string) error {
	if err := startMedia(ch, false); err != nil {
		return err
	}
	var announce, music string
	if len(args) > 0 {
		announce = fifoArg(args[0])
	}
	if len(args) > 1 {
		music = fifoArg(args[1])
	}
	if announce != "" {
		if p := c.resolveSound(ch, announce); p != "" {
			_, _ = c.play(ctx, ch, p, false)
		}
	}
	cl := &fifoCaller{ch: ch, joined: time.Now()}
	q.mu.Lock()
	q.callers = append(q.callers, cl)
	n := len(q.callers)
	q.mu.Unlock()
	ch.SetVar("fifo_name", q.name)
	ch.SetVar("fifo_status", "WAITING")
	ch.log.Info("fifo: caller waiting", "fifo", q.name, "position", n)
	c.recordEvent("call", fmt.Sprintf("%s waits in fifo %s", ch.Var("caller_id_number"), q.name))
	defer q.dropCaller(cl)
	c.fifoDispatch(q)

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exitKey := ch.Var("fifo_caller_exit_key")
	exit := make(chan rune, 1)
	done := c.startMOH(pctx, ch, firstNonEmpty(music, ch.Var("fifo_music"), ch.Var("hold_music")), "", 0, exitKey, exit)
	defer func() { cancel(); <-done }()
	select {
	case <-ctx.Done(): // taken (adopted) or the executor was cancelled
	case <-ch.Done():
	case <-exit:
		ch.SetVar("fifo_status", "ABORTED")
	}
	return nil
}

func (c *Core) fifoOut(ctx context.Context, q *fifoQueue, ch *Channel, args []string) error {
	if err := startMedia(ch, false); err != nil {
		return err
	}
	wait := true
	if len(args) > 0 {
		wait = !strings.EqualFold(args[0], "nowait")
	}
	var music string
	if len(args) > 2 {
		music = fifoArg(args[2])
	}
	cons := &fifoConsumer{ch: ch, take: make(chan *fifoCaller, 1)}
	for {
		if ch.IsHungUp() || ctx.Err() != nil {
			return nil
		}
		var caller *fifoCaller
		q.mu.Lock()
		for len(q.callers) > 0 && caller == nil {
			cand := q.callers[0]
			q.callers = q.callers[1:]
			if !cand.ch.IsHungUp() {
				caller = cand
			}
		}
		if caller == nil && wait {
			q.consumers = append(q.consumers, cons)
		}
		q.mu.Unlock()
		if caller == nil {
			if !wait {
				return nil // nobody is waiting
			}
			pctx, cancel := context.WithCancel(ctx)
			done := c.startMOH(pctx, ch, firstNonEmpty(music, ch.Var("fifo_consumer_music"), ch.Var("hold_music")), "", 0, "", nil)
			select {
			case caller = <-cons.take:
			case <-ctx.Done():
			case <-ch.Done():
			}
			cancel()
			<-done
			if caller == nil {
				q.mu.Lock()
				for i, x := range q.consumers {
					if x == cons {
						q.consumers = append(q.consumers[:i], q.consumers[i+1:]...)
						break
					}
				}
				q.mu.Unlock()
				return nil
			}
		}
		if caller.ch.IsHungUp() {
			continue
		}
		c.adopt(caller.ch)
		waitIdle(caller.ch)
		if caller.ch.IsHungUp() {
			continue
		}
		caller.ch.SetVar("fifo_bridge_uuid", ch.UUID)
		ch.SetVar("fifo_bridge_uuid", caller.ch.UUID)
		c.recordEvent("call", fmt.Sprintf("fifo %s: %s taken by %s", q.name, caller.ch.Var("caller_id_number"), ch.Var("caller_id_number")))
		c.bridgeChannels(ctx, ch, caller.ch)
		if !wait {
			return nil
		}
	}
}

func init() {
	builtinAppsExtra["fifo"] = appFifo
	apiCommands["fifo"] = apiCommand{"fifo count [<name>] | fifo list", "FIFO queues", apiFifo}
	apiCommands["fifo_member"] = apiCommand{"fifo_member add <name> <dial string> [simo [timeout [lag]]] | fifo_member del <name> <dial string>",
		"add or remove a member of a FIFO queue", apiFifoMember}
	RegisterScript("fifo_member", scriptFifoMember)
	RegisterScript("extension_queue", scriptExtensionQueue)
	RegisterScript("app.fifo", scriptAppFifo)
}

func (q *fifoQueue) counts() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	idle, consumers := len(q.consumers), len(q.consumers)
	for _, m := range q.members {
		if m.active > 0 {
			consumers++
		} else if !now.Before(m.busyUntil) {
			idle++
		}
	}
	return fmt.Sprintf("%s:%d:%d:%d:%d:%d", q.name, consumers, len(q.callers), len(q.members), q.ringing, idle)
}

func apiFifo(c *Core, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("usage: fifo count [<name>] | fifo list")
	}
	switch strings.ToLower(args[0]) {
	case "count":
		if len(args) > 1 {
			q := c.fifo(args[1], false)
			if q == nil {
				return args[1] + ":0:0:0:0:0\n", nil
			}
			c.fifoLoad(q)
			return q.counts() + "\n", nil
		}
		fallthrough
	case "list":
		c.callfeat.mu.Lock()
		var qs []*fifoQueue
		for _, q := range c.callfeat.fifos {
			qs = append(qs, q)
		}
		c.callfeat.mu.Unlock()
		sort.Slice(qs, func(i, j int) bool { return qs[i].name < qs[j].name })
		var b strings.Builder
		for _, q := range qs {
			b.WriteString(q.counts() + "\n")
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("usage: fifo count [<name>] | fifo list")
}

func apiFifoMember(c *Core, args []string) (string, error) {
	if len(args) < 3 {
		return "", fmt.Errorf("usage: fifo_member add <name> <dial string> [simo [timeout [lag]]] | fifo_member del <name> <dial string>")
	}
	switch strings.ToLower(args[0]) {
	case "add":
		q := c.fifo(args[1], true)
		c.fifoLoad(q)
		simo, timeout, lag := 1, 60, 10
		if len(args) > 3 {
			simo = optNum(args[3], simo)
		}
		if len(args) > 4 {
			timeout = optNum(args[4], timeout)
		}
		if len(args) > 5 {
			lag = optNum(args[5], lag)
		}
		c.fifoAddMember(q, args[2], simo, timeout, lag)
		return "+OK\n", nil
	case "del":
		q := c.fifo(args[1], false)
		if q == nil {
			return "+OK\n", nil
		}
		q.mu.Lock()
		delete(q.members, args[2])
		q.mu.Unlock()
		return "+OK\n", nil
	}
	return "", fmt.Errorf("usage: fifo_member add|del <name> <dial string>")
}

// ---------------------------------------------------------------------------
// the scripts

// extension_queue.lua: an extension that has a queue ("queue_<ext>@<domain>" in the variable extension_queue):
// when the queue has members, the extension is added as one (fifo_simo, fifo_timeout, fifo_lag), and
// the caller waits in the queue.
func scriptExtensionQueue(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	simo := firstNonEmpty(s.Var("fifo_simo"), "1")
	timeout := firstNonEmpty(s.Var("fifo_timeout"), "10")
	lag := firstNonEmpty(s.Var("fifo_lag"), "10")
	queue := s.Var("extension_queue")
	ext := strings.TrimPrefix(queue, "queue_")
	if len(queue) < len("queue_") {
		ext = ""
	}
	count := strings.Split(strings.TrimSpace(c.API("fifo count "+queue)), ":")
	if len(count) > 3 && count[3] != "0" {
		s.Log("info", "adding member to fifo", "member", ext, "fifo", queue)
		s.SetVar("fifo_member_add_result", strings.TrimSpace(c.API(fmt.Sprintf("fifo_member add %s {fifo_member_wait=nowait}user/%s %s %s %s", queue, ext, simo, timeout, lag))))
	}
	_ = s.Answer()
	return s.Execute("fifo", queue+" in")
}

// fifoMenu is what fifo_member.lua and app.lua fifo member share: "press 1 to log in, 2 to log out".
func fifoMemberLogin(s *Script, queue, user, prompt string, simo, timeout, lag string, onChange func(loggedIn bool)) {
	c := s.Core
	choice := s.GetDigits(1, 1, 3, 3000, prompt, `\d+`)
	s.Log("info", "menu selection", "digit", choice)
	switch choice {
	case "1":
		dial := "{fifo_member_wait=nowait}user/" + user
		if !strings.Contains(user, "@") {
			if _, dom, ok := strings.Cut(queue, "@"); ok {
				dial += "@" + dom // the queue's domain (user/<ext> alone does not say which tenant)
			} else if d := s.Var("domain_name"); d != "" {
				dial += "@" + d
			}
		}
		res := strings.TrimSpace(c.API(fmt.Sprintf("fifo_member add %s %s %s %s %s", queue, dial, simo, timeout, lag)))
		s.SetVar("fifo_member_add_result", res)
		s.Log("info", "fifo_member_add_result", "result", res)
		if onChange != nil {
			onChange(true)
		}
		s.PlaySound("ivr/ivr-you_are_now_logged_in.wav")
	case "2":
		dial := "{fifo_member_wait=nowait}user/" + user
		if !strings.Contains(user, "@") {
			if _, dom, ok := strings.Cut(queue, "@"); ok {
				dial += "@" + dom
			} else if d := s.Var("domain_name"); d != "" {
				dial += "@" + d
			}
		}
		s.SetVar("fifo_member_del_result", strings.TrimSpace(c.API(fmt.Sprintf("fifo_member del %s %s", queue, dial))))
		if onChange != nil {
			onChange(false)
		}
		s.PlaySound("ivr/ivr-you_are_now_logged_out.wav")
	}
}

// fifo_member.lua: an agent logs in to or out of a queue from a phone (queue_name, fifo_simo, fifo_timeout, fifo_lag, pin_number).
func scriptFifoMember(s *Script) error {
	if !s.Ready() {
		return nil
	}
	_ = s.Answer()
	user, pin, queue := s.Var("user_name"), s.Var("pin_number"), s.Var("queue_name")
	simo, timeout, lag := s.Var("fifo_simo"), s.Var("fifo_timeout"), s.Var("fifo_lag")
	if pin != "" {
		s.Sleep(500)
		if _, ok := s.CheckPIN(pin, "phrase:voicemail_enter_pass:#", 0); !ok {
			return nil
		}
	}
	fifoMemberLogin(s, queue, user, "ivr/ivr-enter_destination_telephone_number.wav", firstNonEmpty(simo, "1"), firstNonEmpty(timeout, "10"), firstNonEmpty(lag, "10"), nil)
	s.Hangup("")
	return nil
}

// app.lua fifo member (app/fifo/resources/scripts/member.lua): the same from the FIFO page's feature code; the
// member's wrap up time comes from the FIFO page and the member is switched on or off there.
func scriptAppFifo(s *Script) error {
	if s.Arg(1) != "member" {
		return nil
	}
	if !s.Ready() {
		return nil
	}
	_ = s.Answer()
	domainUUID, fifoUUID, queue := s.Var("domain_uuid"), s.Var("fifo_uuid"), s.Var("fifo_name")
	user, pin := s.Var("user_name"), s.Var("pin_number")
	simo, timeout, lag := firstNonEmpty(s.Var("fifo_simo"), "5"), firstNonEmpty(s.Var("fifo_timeout"), "20"), s.Var("fifo_lag")
	s.Sleep(500)
	if pin != "" {
		if _, ok := s.CheckPIN(pin, "phrase:voicemail_enter_pass:#", 0); !ok {
			return nil
		}
		s.Sleep(500)
	}
	memberUUID := ""
	if r := s.Row("SELECT * FROM v_fifo_members WHERE fifo_uuid = $1::uuid AND member_contact LIKE $2", fifoUUID, "%"+user+"%"); r != nil {
		memberUUID = r["fifo_member_uuid"]
		lag = r["member_wrap_up_time"]
	}
	_ = domainUUID
	fifoMemberLogin(s, queue, user, "phrase:agent_status:#", simo, timeout, firstNonEmpty(lag, "10"), func(in bool) {
		if memberUUID != "" {
			s.Exec("UPDATE v_fifo_members SET member_enabled = $1::text WHERE fifo_member_uuid = $2::uuid", strconv.FormatBool(in), memberUUID)
		}
	})
	return nil
}
