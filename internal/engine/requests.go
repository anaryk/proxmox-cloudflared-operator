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
// what came of them. A run that did not decide leaves them waiting. Of a run
// that decided, an adoption is cleared only when it reached its write, its
// record stored first; one held for any other reason waits on. A
// confirmation is cleared unless the run could not keep it: it confirmed
// nothing and still holds removals by the mass delete guard, or because the
// tombstones could not be saved.
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
		if held := unkeptConfirmation(res); held != "" {
			c.confirmWhy = fmt.Sprintf("the run could not keep the confirmation of the removals it holds (%s); "+
				"it is tried again until it expires", held)
		} else {
			c.e.confirm = nil
			c.adminEvent(levelInfo, "", fmt.Sprintf("deletes confirmed: %d pending removals now pass the mass delete guard", res.Confirmed))
		}
	}
	replaced := make(map[string]bool, len(res.Replaced))
	for _, rec := range res.Replaced {
		replaced[recordName(rec)] = true
	}
	if c.adoptWaits == nil {
		c.adoptWaits = make(map[string]string)
	}
	for _, name := range slices.Sorted(maps.Keys(in.Adopt)) {
		if !c.adopted[name] {
			c.adoptWaits[name] = heldReason(res, name)
			continue
		}
		delete(c.e.adopt, name)
		if replaced[name] {
			c.adminEvent(levelInfo, name, fmt.Sprintf("adoption of %s applied: the record was replaced", name))
			continue
		}
		c.adminEvent(levelWarn, name, fmt.Sprintf("adoption of %s reached its write but the record was not replaced; "+
			"its copy is in the adopted log; see the actions of the cycle", name))
	}
}

// unkeptConfirmation returns why the deletes of a run that confirmed nothing
// are still held, when that is the mass delete guard or tombstones that could
// not be saved, and nothing otherwise.
func unkeptConfirmation(res reconcile.DNSResult) string {
	if res.Confirmed > 0 {
		return ""
	}
	for _, a := range res.Actions {
		if a.Kind == reconcile.DeleteRecord && !a.Applied &&
			(strings.HasPrefix(a.Held, "mass delete guard") || a.Held == "tombstones not saved") {
			return a.Held
		}
	}
	return ""
}

// heldReason says why the run did not take over the record at name: the held
// reason of its action, when it has one.
func heldReason(res reconcile.DNSResult, name string) string {
	for _, a := range res.Actions {
		if !a.Applied && a.Held != "" && strings.EqualFold(strings.TrimSuffix(a.Target, "."), name) {
			return a.Held
		}
	}
	return "the DNS run did not get to its record; see the problems of the cycle"
}

// notePending tells the admin that a request waits and why, again only when
// the reason changes.
func (c *cycleRun) notePending(adoptWaits map[string]string) {
	why := c.waitWhy
	if why == "" {
		why = "the cycle did not reach DNS"
	}
	if r := c.e.confirm; r != nil {
		reason := why
		if c.confirmWhy != "" {
			reason = c.confirmWhy
		}
		if reason != r.why {
			r.why = reason
			c.adminEvent(levelInfo, "", "confirmation waits: "+reason)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.e.adopt)) {
		r := c.e.adopt[name]
		reason, ok := adoptWaits[name]
		if !ok {
			reason = why
		}
		if reason != r.why {
			r.why = reason
			c.adminEvent(levelInfo, name, fmt.Sprintf("adoption of %s waits: %s", name, reason))
		}
	}
}

func (c *cycleRun) adminEvent(level, subject, msg string) {
	c.events = append(c.events, Event{At: c.now, Level: level, Kind: kindAdmin, Subject: subject, Message: msg})
}

// recordName is the name of a record as the requests key it.
func recordName(rec cfapi.Record) string {
	return strings.ToLower(strings.TrimSuffix(rec.Name, "."))
}
