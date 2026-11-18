package core

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/ringq-devs/orswitch/internal/media"
)

// groupConfirm is FreeSWITCH's group_confirm for an answered leg: the person
// who answered hears group_confirm_file (RingQ's call screening plays the
// caller's recorded name) and has to press group_confirm_key to take the call;
// otherwise the leg is hung up and, with fail_on_single_reject, the call is
// not connected. The variables are read from the dial string, then from the
// calling channel. It returns true when no confirmation is asked or the key was pressed.
func (c *Core) groupConfirm(ctx context.Context, a, b *Channel, globals map[string]string) bool {
	get := func(name string) string {
		v := globals[name]
		if v == "" && b != nil {
			v = b.Var(name) // given in the leg's own [...] variables
		}
		if v == "" && a != nil {
			v = a.Var(name)
		}
		return strings.TrimSpace(v)
	}
	key := get("group_confirm_key")
	if key == "" {
		return true
	}
	if key == "exec" {
		// group_confirm_key=exec: group_confirm_file is an application (lua confirm.lua) run on the
		// leg that answered; the call is connected unless that script hangs the leg up (follow me's
		// "prompt to accept": 1 accepts, 2 rejects).
		return c.groupConfirmExec(ctx, b, get("group_confirm_file"))
	}
	file := get("group_confirm_file")
	if strings.HasPrefix(file, "phrase:") {
		// phrase:screen_confirm:<recording>: the recording is what the callee needs to hear
		if i := strings.LastIndex(file, ":"); i > len("phrase:")-1 && strings.Contains(file[len("phrase:"):], ":") {
			file = file[i+1:]
		} else {
			file = ""
		}
	}
	wait := 10 * time.Second
	if n, err := strconv.Atoi(get("group_confirm_read_timeout")); err == nil && n > 0 {
		wait = time.Duration(n) * time.Millisecond
	}
	b.ep.SetMode(media.ModeLocal) // talk to the callee before connecting the caller
	b.FlushDTMF()
	pressed := func(d rune) bool { return d != 0 && strings.ContainsRune(key, d) }
	if file != "" {
		if d, _ := c.play(ctx, b, file, true); pressed(d) {
			return true
		}
	}
	if d, _ := c.play(ctx, b, "tone_stream://%(200,100,800);%(200,100,1000)", true); pressed(d) {
		return true
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		select {
		case d := <-b.dtmf:
			if pressed(d) {
				return true
			}
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		case <-b.Done():
			return false
		}
	}
}
