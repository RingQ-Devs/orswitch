package core

import (
	"strings"
	"fmt"
	"sync"
	"time"
)

// FreeSWITCH-style "status" lines. RingQ Core reads them: the capacity test
// (CapacityTest::parseStatus) takes "N session(s) max", "N session(s) - peak ..."
// and "... per Sec out of max N" from the console's status text.

// sessionPeak keeps the highest number of channels per minute, for the
// "peak" and "last 5min" figures of status.
type sessionPeak struct {
	mu      sync.Mutex
	overall int
	minute  [6]int   // peak in each of the last six minutes
	at      [6]int64 // the minute number each slot holds
}

func (p *sessionPeak) note(n int, now time.Time) {
	m := now.Unix() / 60
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > p.overall {
		p.overall = n
	}
	i := m % 6
	if p.at[i] != m {
		p.at[i], p.minute[i] = m, 0
	}
	if n > p.minute[i] {
		p.minute[i] = n
	}
}

// get returns the peak since startup and in the last five minutes.
func (p *sessionPeak) get(now time.Time) (overall, last5 int) {
	m := now.Unix() / 60
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.at {
		if m-p.at[i] < 5 && p.minute[i] > last5 {
			last5 = p.minute[i]
		}
	}
	return p.overall, last5
}

// fsStatusLines are the lines FreeSWITCH's status prints for sessions.
func (c *Core) fsStatusLines() string {
	now := time.Now()
	peak, last5 := c.peak.get(now)
	cur := c.channelCount()
	if cur > peak {
		peak = cur
	}
	if cur > last5 {
		last5 = cur
	}
	max := c.config().Settings.MaxSessions
	return fmt.Sprintf("%d session(s) - peak %d, last 5min %d \n%d session(s) per Sec out of max %d, peak %d, last 5min %d \n%d session(s) max\nmin idle cpu 0.00/100.00\n",
		cur, peak, last5, 0, 30, 0, 0, max)
}

// extraProfiles are the database's SIP profiles, other than the main one,
// whose port is open now ("sofia status" lists them as FreeSWITCH does).
func (c *Core) extraProfiles() []ProfileStatus {
	var out []ProfileStatus
	for _, p := range c.SIPProfiles() {
		if !p.Main && p.Enabled && p.Listening && p.Name != "internal" {
			out = append(out, p)
		}
	}
	return out
}

// fsChannelName is a channel name as FreeSWITCH writes a trunk leg's:
// sofia/gateway/<gateway uuid>/<number>. RingQ Core finds the trunk of a call
// (call cost, trunk reports) in the channel names of its events.
func fsChannelName(name string) string {
	if rest, ok := strings.CutPrefix(name, "ringq/gateway/"); ok {
		return "sofia/gateway/" + rest
	}
	return name
}
