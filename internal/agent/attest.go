package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	// observationQueue is how many observations wait between the goroutine
	// that reads the kernel's notifications and the one that judges them.
	// Nothing is dropped: a full queue holds the reader, which lets the
	// kernel drop notifications and the observer start again from a dump.
	observationQueue = 1024
	// heldEntries bounds the entries of the forwarding tables the agent
	// holds a word of, heldPerMAC those of one MAC, so that frames with a
	// guest's MAC in many VLANs grow nothing; attestationsKept bounds the
	// attestations a status carries, movesKept the moves, which it carries
	// for movesKeptFor.
	heldEntries      = 4096
	heldPerMAC       = 8
	attestationsKept = 1000
	movesKept        = 500
	movesKeptFor     = 10 * time.Minute
	// conflictSaidEvery is the least time between two events of the
	// conflicts of one MAC, whatever ports it shows on.
	conflictSaidEvery = time.Minute
	// observeAgainAfter is how long an observer that failed waits before it
	// starts again.
	observeAgainAfter = time.Minute
)

// ObserveFunc reads the forwarding tables of the node's bridges until ctx
// ends and reports what they show of the expected MACs: egress.ObserveForwarding.
type ObserveFunc func(ctx context.Context, expected func() map[string]egress.Expected, report func(egress.Observation)) error

// observing is what the agent holds of the forwarding tables of its bridges:
// the MACs of the published guests, as the observer expects them, and the
// last word of each entry that still holds. It is guarded by mu; the
// observer reads the expected MACs at every entry.
type observing struct {
	mu       sync.Mutex
	expected map[string]egress.Expected // by MAC
	// cards are the published cards whose guest the snapshot says runs, or
	// may run, by MAC: what is attested is judged against them when the
	// status is made, so that a guest the snapshot says stopped is attested
	// no more, whatever the table still holds of it.
	cards map[string]model.NIC
	kinds map[int]model.GuestKind // by VMID, of every guest the snapshot names
	held  map[entryKey]heldEntry
	count map[string]int           // the entries held, by MAC
	said  map[string]time.Time     // when a conflict of each MAC was last said
	moves []agentproto.Observation // newest last
	// since is when the notifications now followed began, zero while none
	// are: the start of the observer's run, or of its last subscription.
	since time.Time
	run   int // numbers the runs of the observer, so a run that ended says nothing more
	queue chan observed
}

// entryKey is an entry of a forwarding table: a MAC in a VLAN of a bridge.
type entryKey struct {
	bridge string
	vlan   int
	mac    string
}

// heldEntry is the last Learned or Conflict of an entry that still holds.
type heldEntry struct {
	kind  egress.ObservationKind
	port  string
	from  string
	since time.Time
	// guest and nic are the published card the MAC is of.
	guest model.GuestRef
	nic   int
	owner *model.GuestRef // the guest whose port the MAC is on, for a conflict
}

// observed is an observation with the run of the observer that made it.
type observed struct {
	run int
	obs egress.Observation
}

// Expected is what the observer of the forwarding tables asks at every
// entry: the MACs of the published guests, by MAC, as the last pass set them.
func (a *Agent) Expected() map[string]egress.Expected {
	a.obs.mu.Lock()
	defer a.obs.mu.Unlock()
	return a.obs.expected
}

// expect gives the observer the MACs of the guests the snapshot publishes:
// every card of every target's guest, whatever node the snapshot says runs
// it, so that this node attests any of them it finds on its bridges. What is
// held of a MAC that is no longer published, or published for another card,
// is forgotten. An appliance agent observes nothing.
func (p *pass) expect() {
	if p.a.d.Kind != pki.KindHost {
		return
	}
	expected := map[string]egress.Expected{}
	cards := map[string]model.NIC{}
	kinds := map[int]model.GuestKind{}
	for _, t := range p.s.Targets {
		if t.Guest == nil {
			continue
		}
		kinds[t.Guest.Ref.VMID] = t.Guest.Ref.Kind
		for _, n := range t.Guest.NICs {
			mac, err := model.NormalizeMAC(n.MAC)
			if err != nil {
				continue
			}
			if _, dup := expected[mac]; dup {
				continue
			}
			expected[mac] = egress.Expected{MAC: mac, Guest: t.Guest.Ref, NIC: n.Index}
			if t.Guest.Running || t.Guest.StatusUnknown {
				cards[mac] = n
			}
		}
	}
	for _, g := range p.s.Guests {
		kinds[g.Ref.VMID] = g.Ref.Kind
	}
	o := &p.a.obs
	o.mu.Lock()
	defer o.mu.Unlock()
	o.expected, o.cards, o.kinds = expected, cards, kinds
	for key, h := range o.held {
		want, ok := expected[key.mac]
		if !ok || h.kind == egress.Learned && (want.Guest != h.guest || want.NIC != h.nic) {
			o.drop(key)
		}
	}
	for mac := range o.said {
		if _, ok := expected[mac]; !ok {
			delete(o.said, mac)
		}
	}
}

// Observe runs the observer of the forwarding tables of the node's bridges
// until ctx ends, and starts it again after a while when it fails; while
// none runs the node attests nothing. It is called once, by the daemon, on a
// host node: an appliance agent reads no bridge and refuses.
func (a *Agent) Observe(ctx context.Context, observe ObserveFunc) error {
	if a.d.Kind != pki.KindHost {
		return errors.New("an appliance agent reads no forwarding table")
	}
	o := &a.obs
	o.mu.Lock()
	if o.queue == nil {
		o.queue = make(chan observed, observationQueue)
	}
	queue := o.queue
	o.mu.Unlock()
	judged := make(chan struct{})
	go func() {
		defer close(judged)
		a.judge(ctx, queue)
	}()
	defer func() { <-judged }()
	for {
		run := a.observing(true)
		report := func(obs egress.Observation) {
			select {
			case queue <- observed{run: run, obs: obs}:
			case <-ctx.Done():
			}
		}
		err := observe(ctx, a.Expected, report)
		a.observing(false)
		if ctx.Err() != nil {
			return nil
		}
		a.d.Log.Warn().Err(err).Dur("again", observeAgainAfter).Msg("watching the forwarding tables of the bridges failed; " +
			"until it runs again this node attests nothing")
		ch, stop := a.after(observeAgainAfter)
		select {
		case <-ctx.Done():
			stop()
			return nil
		case <-ch:
		}
	}
}

// observing notes that a run of the observer starts, and returns its
// number, or that the run ended. Either way what was held is forgotten and
// the run numbered anew, so that an observation of the run that ended, still
// queued, says nothing; while no run is on, the status says that the tables
// are not watched.
func (a *Agent) observing(on bool) int {
	o := &a.obs
	o.mu.Lock()
	o.run++
	o.forget()
	o.since = time.Time{}
	if on {
		o.since = a.d.Now()
	}
	run := o.run
	o.mu.Unlock()
	a.changed()
	return run
}

// forget drops every entry held.
func (o *observing) forget() {
	clear(o.held)
	clear(o.count)
}

// judge takes the observations off the queue, so that the goroutine that
// reads the kernel's notifications never waits on the agent's locks.
func (a *Agent) judge(ctx context.Context, queue <-chan observed) {
	for {
		select {
		case <-ctx.Done():
			return
		case o := <-queue:
			a.observed(ctx, o)
		}
	}
}

// observed takes in what the forwarding table of a bridge showed of a
// published MAC. A Learned on the guest's own port holds, and is attested
// when the bridge is the card's; a Conflict holds and is reported while it
// does; a Moved is reported for a while; a Gone ends what was held; a Reset
// ends everything held, since a dump follows that shows the tables as they
// are, and starts the watch anew. A move or a new conflict takes every
// address this node serves on that MAC out of the egress filter at once, to
// be verified again. An observation of a run that ended, or made while none
// is on, says nothing.
func (a *Agent) observed(ctx context.Context, in observed) {
	o := &a.obs
	obs := in.obs
	key := entryKey{bridge: obs.Bridge, vlan: obs.VLAN, mac: obs.MAC}
	now := a.d.Now()
	o.mu.Lock()
	if in.run != o.run || o.since.IsZero() {
		o.mu.Unlock()
		return
	}
	want, expected := o.expected[obs.MAC]
	prev, had := o.held[key]
	var changed, block bool
	var say string
	switch obs.Kind {
	case egress.Reset:
		o.forget()
		o.since = now
		changed = true
	case egress.Learned:
		if !expected {
			break
		}
		h := heldEntry{kind: egress.Learned, port: obs.Port, since: obs.At, guest: want.Guest, nic: want.NIC}
		if had && prev.kind == egress.Learned && prev.port == obs.Port {
			h.since = prev.since
		} else {
			changed = true
		}
		if had || o.room(obs.MAC) {
			o.hold(key, h)
		}
	case egress.Conflict:
		if !expected {
			break
		}
		h := heldEntry{kind: egress.Conflict, port: obs.Port, from: obs.From, since: obs.At, owner: o.ownerOf(obs.Owner)}
		if had && prev.kind == egress.Conflict && prev.port == obs.Port {
			h.since = prev.since
		} else {
			changed, block = true, true
			if last, ok := o.said[obs.MAC]; !ok || now.Sub(last) >= conflictSaidEvery {
				if o.said == nil {
					o.said = map[string]time.Time{}
				}
				o.said[obs.MAC] = now
				say = fmt.Sprintf("guest %s: its MAC %s is on port %s of bridge %s, a port of %s; the address is not reached until the controller lifts the block",
					want.Guest, obs.MAC, obs.Port, obs.Bridge, ownerText(h.owner))
			}
		}
		if !had {
			o.makeRoom(obs.MAC)
		}
		o.hold(key, h)
	case egress.Moved:
		if !expected {
			break
		}
		o.drop(key)
		o.moves = append(o.moves, agentproto.Observation{
			Kind: agentproto.ObservationMoved, MAC: obs.MAC, Bridge: obs.Bridge, VLAN: obs.VLAN, From: obs.From, Port: obs.Port, At: obs.At,
		})
		if len(o.moves) > movesKept {
			o.moves = slices.Delete(o.moves, 0, len(o.moves)-movesKept)
		}
		changed, block = true, true
		say = fmt.Sprintf("guest %s: its MAC %s left its own port %s for %s on bridge %s; the address is verified again before it is reached",
			want.Guest, obs.MAC, obs.From, obs.Port, obs.Bridge)
	case egress.Gone:
		if had {
			o.drop(key)
			changed = true
		}
	}
	o.mu.Unlock()
	if block {
		a.blockMAC(ctx, obs.MAC)
	}
	if say != "" {
		a.event(levelWarn, kindEgress, obs.MAC, say)
	}
	if changed {
		a.changed()
	}
}

// hold keeps h for key.
func (o *observing) hold(key entryKey, h heldEntry) {
	if o.held == nil {
		o.held, o.count = map[entryKey]heldEntry{}, map[string]int{}
	}
	if _, had := o.held[key]; !had {
		o.count[key.mac]++
	}
	o.held[key] = h
}

// drop forgets what is held for key.
func (o *observing) drop(key entryKey) {
	if _, had := o.held[key]; !had {
		return
	}
	delete(o.held, key)
	if o.count[key.mac]--; o.count[key.mac] <= 0 {
		delete(o.count, key.mac)
	}
}

// room reports whether a new entry of mac may be held.
func (o *observing) room(mac string) bool {
	return len(o.held) < heldEntries && o.count[mac] < heldPerMAC
}

// makeRoom makes room for a new conflict of mac, which is always held: when
// the MAC has no room the oldest Learned of the MAC goes, or, when it holds
// nothing but conflicts, its oldest conflict; when the whole has no room,
// the oldest Learned of any MAC goes, or else the oldest conflict of any.
func (o *observing) makeRoom(mac string) {
	if o.room(mac) {
		return
	}
	ofMAC := o.count[mac] >= heldPerMAC
	for _, kind := range []egress.ObservationKind{egress.Learned, egress.Conflict} {
		var victim entryKey
		var found bool
		for key, h := range o.held {
			if h.kind != kind || ofMAC && key.mac != mac {
				continue
			}
			if !found || h.since.Before(o.held[victim].since) || h.since.Equal(o.held[victim].since) && key.vlan > victim.vlan {
				victim, found = key, true
			}
		}
		if found {
			o.drop(victim)
			return
		}
	}
}

// onCard reports whether an entry is on the card with mac, of a guest the
// snapshot says runs: on the bridge the card names in the card's VLAN, or
// untagged (the VID is 0 on a bridge that does not filter VLANs and the
// default PVID, 1, on one that does; a bridge with another PVID attests
// nothing, the safe side), or, for a tagged card, untagged on the bridge
// Proxmox makes for its VLAN on a bridge that does not filter VLANs. A
// firewall bridge between the card and its bridge is neither, and the entry
// on it names the tap, not the port the attestation must name.
func (o *observing) onCard(mac, bridge string, vlan int) bool {
	nic, ok := o.cards[mac]
	switch {
	case !ok:
		return false
	case nic.VLAN == 0:
		return bridge == nic.Bridge && (vlan == 0 || vlan == 1)
	}
	return bridge == nic.Bridge && vlan == nic.VLAN || bridge == fmt.Sprintf("%sv%d", nic.Bridge, nic.VLAN) && vlan == 0
}

// ownerOf is the guest whose port a MAC is on, with its kind from the
// snapshot when the port's name does not say it.
func (o *observing) ownerOf(owner *model.GuestRef) *model.GuestRef {
	if owner == nil {
		return nil
	}
	ref := *owner
	if ref.Kind == "" {
		ref.Kind = o.kinds[ref.VMID]
	}
	return &ref
}

func ownerText(ref *model.GuestRef) string {
	switch {
	case ref == nil:
		return "another guest"
	case ref.Kind == "":
		return fmt.Sprintf("the guest with VMID %d", ref.VMID)
	}
	return "guest " + ref.String()
}

// blockMAC takes every address this node serves on mac out of the egress
// filter, as a move of its MAC does, to be verified again.
func (a *Agent) blockMAC(ctx context.Context, mac string) {
	var addrs []netip.Addr
	a.egMu.Lock()
	for addr, p := range a.pins {
		if p.MAC.String() == mac || slices.ContainsFunc(p.Own, func(o egress.OwnMAC) bool { return o.MAC.String() == mac }) {
			addrs = append(addrs, addr)
		}
	}
	a.egMu.Unlock()
	slices.SortFunc(addrs, netip.Addr.Compare)
	for _, addr := range addrs {
		a.Moved(ctx, addr)
	}
}

// report is what the status carries of the forwarding tables: an
// attestation for every card of a running published guest learned on its
// own port of its own bridge, every conflict that holds, and the moves of
// the last movesKeptFor, at most movesKept of them all, the oldest left out,
// and at most attestationsKept attestations, the first by MAC. What holds is
// the last word of the observer on each entry: a move, a conflict, the
// entry's going or a reset ends an attestation at once, and while no run of
// the observer is on nothing is reported at all.
func (o *observing) report(node string, now time.Time) ([]agentproto.Attestation, []agentproto.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.since.IsZero() {
		return nil, nil
	}
	var attestations []agentproto.Attestation
	var observations []agentproto.Observation
	for key, h := range o.held {
		switch {
		case h.kind == egress.Learned && o.onCard(key.mac, key.bridge, key.vlan):
			attestations = append(attestations, agentproto.Attestation{
				Node: node, Guest: h.guest, NIC: h.nic, MAC: key.mac, Bridge: key.bridge, VLAN: key.vlan, Port: h.port, Since: h.since,
			})
		case h.kind == egress.Conflict:
			observations = append(observations, agentproto.Observation{
				Kind: agentproto.ObservationConflict, MAC: key.mac, Bridge: key.bridge, VLAN: key.vlan, From: h.from, Port: h.port, Owner: h.owner, At: h.since,
			})
		}
	}
	for _, m := range o.moves {
		if now.Sub(m.At) <= movesKeptFor {
			observations = append(observations, m)
		}
	}
	slices.SortFunc(attestations, func(a, b agentproto.Attestation) int {
		return cmp.Or(cmp.Compare(a.MAC, b.MAC), cmp.Compare(a.Bridge, b.Bridge), cmp.Compare(a.VLAN, b.VLAN))
	})
	if len(attestations) > attestationsKept {
		attestations = attestations[:attestationsKept]
	}
	slices.SortFunc(observations, func(a, b agentproto.Observation) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.MAC, b.MAC), cmp.Compare(a.Bridge, b.Bridge), cmp.Compare(a.VLAN, b.VLAN), cmp.Compare(a.Kind, b.Kind))
	})
	if len(observations) > movesKept {
		observations = observations[len(observations)-movesKept:]
	}
	return attestations, observations
}
