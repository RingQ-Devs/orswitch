package core

import (
	"strings"
)

// intercept_group.lua [direction]: answer a call that is ringing a phone of the caller's call group(s)
// (FusionPBX intercept.lua without an extension).
//
// The members are the extensions whose call group contains one of the caller's groups (a
// call_group like %group%; a caller without a group picks up for extensions without a group, as
// the Lua query did). direction: all (the default), inbound (calls ringing the members) or
// outbound (calls the members are making that still ring). pin_number (the dialplan's) is asked
// first. The native rq_pickup application (pbx.go) does the same for the plain case; this script
// also keeps FusionPBX's group semantics, the PIN and the queue variables of a picked up queue call.
//
// Not supported: outbound (calls the members are placing) and the other-box proxy of intercept.lua
// (X-intercept_uuid): the switch runs on one box.

func init() { RegisterScript("intercept_group", scriptInterceptGroup) }

// callGroupMembers are the extensions of the domain that share a call group with the extension.
func (c *Core) callGroupMembers(domain, ext string) []string {
	dom := c.config().Directory.Find(domain)
	if dom == nil {
		return nil
	}
	groups := strings.Split(c.extData(ext, domain, "call_group"), ",")
	var out []string
	for _, u := range dom.Users {
		cg := u.Vars["call_group"]
		if o, ok := c.userOverride(u.ID, domain, "call_group"); ok {
			cg = o
		}
		for _, g := range groups {
			g = strings.TrimSpace(g)
			if (g == "" && cg == "") || (g != "" && strings.Contains(cg, g)) {
				out = append(out, u.ID)
				break
			}
		}
	}
	return out
}

func scriptInterceptGroup(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c := s.Core
	direction := firstNonEmpty(s.Arg(1), "all")
	_ = s.Answer()
	domain, callerExt := s.Var("domain_name"), s.Var("caller_id_number")
	if pin := s.Var("pin_number"); pin != "" {
		s.Sleep(500)
		if _, ok := s.CheckPIN(pin, "phrase:voicemail_enter_pass:#", 0); !ok {
			return nil
		}
	}
	if !s.Ready() {
		return nil
	}
	if !strings.Contains(direction, "all") && !strings.Contains(direction, "inbound") {
		s.Log("info", "intercept group: only calls ringing the group can be picked up here", "direction", direction)
		return nil
	}
	members := map[string]bool{}
	for _, m := range c.callGroupMembers(domain, callerExt) {
		members[m] = true
	}
	own := s.UUID()
	for _, leg := range c.Channels() {
		v := leg.Vars()
		if leg.Direction != DirectionOutbound || leg.IsHungUp() || leg.IsAnswered() || !strings.EqualFold(v["dialed_domain"], domain) || !members[v["dialed_user"]] {
			continue
		}
		// a queue's ring goes to the queued caller; any other leg to the call that rings it
		target := chAlive(c, v["cc_member_session_uuid"])
		if target == nil {
			target = chAlive(c, v["originator"])
		}
		if target == nil || target.UUID == own || target.IsAnswered() {
			continue
		}
		s.Log("info", "intercept group", "ringing", v["dialed_user"], "takes", target.UUID)
		if q := target.Var("cc_queue"); q != "" {
			// a picked up queue call stays a queue call (the call center's reports)
			for _, k := range []string{"call_center_queue_uuid", "cc_side", "cc_queue", "cc_queue_joined_epoch"} {
				if val := target.Var(k); val != "" {
					s.SetVar(k, val)
				}
			}
			s.SetVar("cc_cause", "answered")
		}
		return c.takeOver(s.Ctx, s.Ch, target)
	}
	s.Log("info", "intercept group: nothing is ringing", "caller", callerExt, "members", len(members))
	return nil
}
