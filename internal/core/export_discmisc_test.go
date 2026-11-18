package core

// SchedCount is the number of scheduled tasks (sched_api, sched_transfer, sched_hangup).
func (c *Core) SchedCount() int { return c.sched.count() }

// ParkedSlots lists the valet park slots in use ("lot/slot").
func (c *Core) ParkedSlots() []string {
	c.park.mu.Lock()
	defer c.park.mu.Unlock()
	var out []string
	for k, ch := range c.park.slots {
		if ch != nil && !ch.IsHungUp() {
			out = append(out, k)
		}
	}
	return out
}

// QueueEmailForTest queues a mail with QueueEmail.
func (c *Core) QueueEmailForTest(m EmailMessage) error { return c.QueueEmail(testCtx(), m) }
