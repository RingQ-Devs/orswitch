package core

import "github.com/ringq-devs/orswitch/internal/config"

// gatewayEvent fires the CUSTOM event FreeSWITCH gives when a gateway is added or deleted
// (sofia::gateway_add / sofia::gateway_delete); the HA monitor passes it to the other servers.
func (c *Core) gatewayEvent(subclass string, g config.Gateway) {
	e := NewEvent(EvCustom, subclass)
	e.Add("profile-name", g.Profile)
	e.Add("Gateway", g.Name)
	c.events.Publish(e)
}
