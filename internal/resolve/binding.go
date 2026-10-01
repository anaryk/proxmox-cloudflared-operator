package resolve

import (
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Binding remembers the address verified for a guest NIC.
type Binding struct {
	Owner        string     `json:"owner"`
	Hostname     string     `json:"hostname"`
	Addr         netip.Addr `json:"addr"`
	MAC          string     `json:"mac"`
	VerifiedAt   time.Time  `json:"verifiedAt"`
	FailingSince *time.Time `json:"failingSince,omitempty"`
}

func newBinding(route model.Route, c Candidate, now time.Time) *Binding {
	return &Binding{Owner: route.Owner(), Hostname: route.Hostname, Addr: c.Addr, MAC: c.NIC.MAC, VerifiedAt: now}
}

// appliesTo reports whether b was made for route. A binding of another owner
// must never hand its address to whoever holds the hostname now.
func (b *Binding) appliesTo(route model.Route) bool {
	return b != nil && b.Owner == route.Owner() && b.Hostname == route.Hostname
}

// clone returns a copy that shares no memory with b.
func (b *Binding) clone() *Binding {
	c := *b
	if b.FailingSince != nil {
		since := *b.FailingSince
		c.FailingSince = &since
	}
	return &c
}

// failing returns a copy of b that has been failing since now, unless it
// already was failing.
func (b *Binding) failing(now time.Time) *Binding {
	c := b.clone()
	if c.FailingSince == nil {
		c.FailingSince = &now
	}
	return c
}

// holds reports whether b, failing at now, is still kept without trying
// other candidates.
func (b *Binding) holds(now time.Time, stickyFor time.Duration) bool {
	since := now
	if b.FailingSince != nil {
		since = *b.FailingSince
	}
	return now.Sub(since) < stickyFor
}
