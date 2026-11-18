package core

import (
	"strings"
)

// ringq_att_voicemail: RingQ attended transfer, the agent reaches the target's
// voicemail (the Go version of ringq_att_voicemail.lua).
//
// The backend (node/server.js [att-vm]) runs it on the AGENT's consult leg, with
// `uuid_transfer <consult leg> lua:ringq_att_voicemail.lua inline`, when the
// consult call to the target is not answered and the target has voicemail. The
// agent hears the target's voicemail and can leave a message while the customer
// stays parked on hold music. When the voicemail is finished (saved, or ended
// without saving) the agent is NOT hung up: the call goes back to the customer.
//
// Variables set before the transfer:
//
//	voicemail_id        the target's voicemail box
//	rq_att_agent_leg    uuid of the leg to bridge back to (the customer on hold)
//	rq_att_agent_ext    optional: extension to transfer to when that leg is gone

func init() { RegisterScript("ringq_att_voicemail", scriptAttVoicemail) }

func scriptAttVoicemail(s *Script) error {
	if !s.Ready() {
		return nil
	}
	c, ch := s.Core, s.Ch
	domain, box := ch.Var("domain_name"), ch.Var("voicemail_id")
	if box == "" {
		box = ch.Var("dialed_extension")
	}

	// the voicemail ends a call with a hangup; here it must leave the call up
	ch.SetVar("rq_vm_keep_call", "true")
	if err := c.voicemailDeposit(s.Ctx, ch, domain, box); err != nil {
		s.Log("err", "voicemail error", "box", box+"@"+domain, "err", err)
	}
	ch.SetVar("rq_vm_keep_call", "")
	s.Log("info", "voicemail finished, back to the held party", "box", box+"@"+domain)

	if !s.Ready() {
		return nil // the agent hung up
	}
	agentLeg := strings.TrimSpace(ch.Var("rq_att_agent_leg"))
	agentExt := strings.TrimSpace(ch.Var("rq_att_agent_ext"))

	// the agent pressed Retrieve during the voicemail: already connected again
	if ch.Partner() != nil {
		return nil
	}
	// the backend also clears rq_att_agent_leg when the agent retrieved the call
	if agentLeg != "" && c.Channel(agentLeg) != nil {
		r := strings.TrimSpace(c.API("uuid_bridge " + agentLeg + " " + ch.UUID))
		s.Log("info", "bridged back", "to", agentLeg, "result", r)
		if strings.HasPrefix(r, "+OK") {
			return nil
		}
	}
	if agentExt != "" && domain != "" {
		s.Log("info", "held party gone, transferring", "to", agentExt)
		ccTransfer(s, agentExt, domain)
	}
	return nil
}
