package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// requestTTL is how long a one-shot request of the admin waits for a DNS run
// that decides on it. An older one would land on removals the admin did not
// see.
const requestTTL = 5 * time.Minute

// expireRequests drops the requests that waited longer than requestTTL.
func (c *cycleRun) expireRequests() {
	if r := c.e.confirm; r != nil && c.now.Sub(r.at) >= requestTTL {
		c.e.confirm = nil
		c.adminEvent(levelWarn, "", "confirmation expired before it could be applied")
	}
	for _, name := range slices.Sorted(maps.Keys(c.e.adopt)) {
		if c.now.Sub(c.e.adopt[name].at) >= requestTTL {
			delete(c.e.adopt, name)
			c.adminEvent(levelWarn, name, fmt.Sprintf("adoption of %s expired before it could be applied", name))
		}
	}
}

// adoptable returns the requested adoptions whose hostname is published
// through a tunnel whose configuration is verified and whose connector is
// ready, so that the name answers once its record points at the tunnel. The
// others wait, with the reason in waits.
func (c *cycleRun) adoptable(records map[string]reconcile.TunnelState) (ready []string, waits map[string]string) {
	waits = make(map[string]string)
	readyConns := make(map[string]bool, len(c.st.Connectors))
	for _, st := range c.st.Connectors {
		readyConns[st.TunnelID] = st.Ready
	}
	for _, name := range slices.Sorted(maps.Keys(c.e.adopt)) {
		t, published := records[name]
		switch {
		case !published:
			waits[name] = "pco does not publish the hostname in this cycle"
		case !t.Exists || !t.Verified || !readyConns[t.ID]:
			waits[name] = "its tunnel is not verified or its connector is not ready"
		default:
			ready = append(ready, name)
		}
	}
	return ready, waits
}

// settleRequests clears the requests an enforcing DNS run decided on and says
// what came of them. A run that did not decide leaves them waiting.
func (c *cycleRun) settleRequests(in reconcile.DNSInput, res reconcile.DNSResult, mode reconcile.Mode) {
	switch {
	case mode != reconcile.Enforce:
		c.waitWhy = "pco is in observe-only mode"
		return
	case !res.Decided:
		c.waitWhy = "the DNS run stopped before it decided"
		if len(res.Problems) > 0 {
			c.waitWhy += ": " + res.Problems[0]
		}
		return
	}
	if in.ConfirmDeletes {
		c.e.confirm = nil
		c.adminEvent(levelInfo, "", fmt.Sprintf("deletes confirmed: %d pending removals now pass the mass delete guard", res.Confirmed))
	}
	replaced := make(map[string]bool, len(res.Replaced))
	for _, rec := range res.Replaced {
		replaced[recordName(rec)] = true
	}
	for _, name := range slices.Sorted(maps.Keys(in.Adopt)) {
		delete(c.e.adopt, name)
		if replaced[name] {
			c.adminEvent(levelInfo, name, fmt.Sprintf("adoption of %s applied: the record was replaced", name))
			continue
		}
		c.adminEvent(levelWarn, name, fmt.Sprintf("adoption of %s was tried but replaced nothing; see the actions of the cycle", name))
	}
}

// notePending tells the admin, once per request, that it waits and why.
func (c *cycleRun) notePending(adoptWaits map[string]string) {
	why := c.waitWhy
	if why == "" {
		why = "the cycle did not reach DNS"
		if len(c.st.Problems) > 0 {
			why += ": " + c.st.Problems[0]
		}
	}
	if r := c.e.confirm; r != nil && !r.told {
		r.told = true
		c.adminEvent(levelInfo, "", "confirmation waits: "+why)
	}
	for _, name := range slices.Sorted(maps.Keys(c.e.adopt)) {
		r := c.e.adopt[name]
		if r.told {
			continue
		}
		r.told = true
		w, ok := adoptWaits[name]
		if !ok {
			w = why
		}
		c.adminEvent(levelInfo, name, fmt.Sprintf("adoption of %s waits: %s", name, w))
	}
}

func (c *cycleRun) adminEvent(level, subject, msg string) {
	c.events = append(c.events, Event{At: c.now, Level: level, Kind: kindAdmin, Subject: subject, Message: msg})
}

// recordName is the name of a record as the requests key it.
func recordName(rec cfapi.Record) string {
	return strings.ToLower(strings.TrimSuffix(rec.Name, "."))
}
