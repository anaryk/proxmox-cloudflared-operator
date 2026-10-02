package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	refreshTimeout     = 60 * time.Second
	resolveTimeout     = 15 * time.Second
	resolveConcurrency = 8

	problemNotSetUp       = "pco is not set up on this node; run pco setup"
	problemNotMounted     = "cluster filesystem is not mounted"
	problemNoWriter       = "no writer identity; run pco setup"
	problemNotRegistered  = "node %s is not registered; run pco setup"
	problemPolicyInvalid  = "settings contain an invalid allow or deny pattern; nothing is changed until it is fixed"
	problemIncomplete     = "the inventory is incomplete; claims, bindings, tunnels, DNS and connectors are left as they are"
	problemNoCredential   = "no Cloudflare credential; add one with pco credential add"
	problemStoppedResolve = "the cycle ended while addresses were resolved (%v); nothing is changed"
)

// cycleRun is the state of one cycle. It runs under the cycle lock.
type cycleRun struct {
	e      *Engine
	ctx    context.Context
	now    time.Time
	st     State   // the state being built
	events []Event // reported by the steps; the changes against the last state come on top

	settings  store.Settings
	install   store.Install
	snap      inventory.Snapshot
	manual    []model.Route
	approvals map[string]string // nil unless admission is approve
	stored    map[string]planner.Claim
	bindings  map[string]resolve.Binding
	deny      resolve.Denylist
	col       planner.Collected
	claims    planner.ClaimResult
	settled   bool                      // the claims were settled and saved
	listing   listing                   // what this cycle saw of the guests; empty unless it saw all of them
	results   map[string]resolve.Result // of the winners, by hostname, with what the identity minimum holds back
	credIDs   []string                  // of the stored credentials, sorted
	zones     zoneSet
	plan      planner.Plan

	// cfHold says that nothing is changed at Cloudflare or on the
	// connectors in this cycle; the problems say why, and holdWhy is the
	// reason of the step that first held the cycle or ended it before its DNS
	// run looked. checked says that the cycle got through to its DNS run and
	// that the run looked at the records as the writer. storeHold says that
	// it held because the store could not be read or written.
	cfHold        bool
	storeHold     bool
	holdWhy       string
	checked       bool
	recheck       recheck
	tunnelVerdict reconcile.WriterVerdict
	// tunnels are the states the tunnel run returned, which DNS must be given.
	tunnels []reconcile.TunnelState
	// offer is what this cycle's state shows waiting for a confirmation.
	offer confirmable

	// waitWhy says why the admin's requests wait, when the DNS step was
	// reached; confirmWhy why a confirmation the DNS run could not keep
	// waits, and adoptWaits why each adoption waits. adopted holds the names
	// whose adoption reached its write in this cycle.
	waitWhy    string
	confirmWhy string
	adoptWaits map[string]string
	adopted    map[string]bool
}

func (e *Engine) newCycle(ctx context.Context) *cycleRun {
	now := e.d.Now()
	return &cycleRun{e: e, ctx: ctx, now: now, st: e.State().carried(now)}
}

// run goes through the steps of a cycle in order. A step that cannot go on
// safely ends the cycle; what it found so far is the state.
func (c *cycleRun) run() State {
	c.expireRequests()
	if c.prepare() && c.inspect() {
		c.build()
		if why, saved := c.saveMemory(); !saved {
			c.hold(why)
		}
		c.reconcile()
	}
	c.saveMemory()
	if !c.checked {
		c.markUnchecked()
	}
	c.notePending(c.adoptWaits)
	c.st.Waiting = c.offer.waiting(c.st.Routes)
	c.st.Offer = offerOf(c.st.Waiting)
	c.st.FinishedAt = c.e.d.Now()
	return c.st.normalized()
}

// problem adds a problem line to the state and returns it.
func (c *cycleRun) problem(format string, args ...any) string {
	line := fmt.Sprintf(format, args...)
	c.st.Problems = append(c.st.Problems, line)
	return line
}

// storeProblem reports a store error and returns the line. The cases that need
// the admin are named as such, never taken for "nothing stored".
func (c *cycleRun) storeProblem(doing string, err error) string {
	c.storeHold = true
	switch {
	case errors.Is(err, store.ErrNotMounted):
		return c.problem(problemNotMounted)
	case errors.Is(err, store.ErrNoRoot):
		return c.problem(problemNotSetUp)
	}
	return c.problem("%s: %v", doing, err)
}

// hold leaves Cloudflare and the connectors as they are for the rest of the
// cycle, which is then not checked. why says so, as a problem line does: the
// first reason given is the one the state names the hold by.
func (c *cycleRun) hold(why string) {
	c.cfHold = true
	if c.holdWhy == "" {
		c.holdWhy = why
	}
}

// prepare reads the settings, the install, the node registry and the writer
// identity. Without the first three nothing else is done.
func (c *cycleRun) prepare() bool {
	s, err := c.e.d.Store.Settings()
	if err != nil {
		c.hold(c.storeProblem("reading the settings", err))
		return false
	}
	c.settings = s
	c.st.Mode = modeName(s.ObserveOnly)
	c.e.interval.Store(int64(s.PollInterval))
	if c.e.d.StartOnly != nil {
		if names := c.e.d.StartOnly(s); len(names) > 0 {
			c.problem("settings %s changed since pco started and are read only at start; "+
				"restart pco (systemctl restart pco) for them to take effect", strings.Join(names, ", "))
		}
	}

	inst, found, err := c.e.d.Store.Install()
	switch {
	case err != nil:
		c.hold(c.storeProblem("reading the install identity", err))
		return false
	case !found:
		c.hold(c.problem(problemNotSetUp))
		return false
	}
	c.install = inst
	c.st.Profile = inst.ProfileName()
	if !c.registered() {
		return false
	}
	c.readWriter()
	switch note, err := c.e.recall(c.install.ID); {
	case err != nil:
		c.storeHold = true
		c.hold(c.problem("reading what the engine remembered: %v; nothing is changed at Cloudflare until it can be read: "+
			"fix the file or remove it; removing it forgets the connectors kept for tunnels no credential sees, "+
			"the zones that left their listing and the guests confirmed gone", err))
	case note != "":
		c.problem("%s", note)
	}
	return true
}

// registered checks that the node registry names this node and no other:
// until the cluster milestone exactly one node may run pco.
func (c *cycleRun) registered() bool {
	nodes, err := c.e.d.Store.Nodes()
	if err != nil {
		c.hold(c.storeProblem("reading the node registry", err))
		return false
	}
	self := false
	var others []string
	for _, n := range nodes {
		if n.Name == c.e.d.Node {
			self = true
		} else {
			others = append(others, n.Name)
		}
	}
	switch {
	case !self:
		c.hold(c.problem(problemNotRegistered, c.e.d.Node))
		return false
	case len(others) > 0:
		slices.Sort(others)
		c.hold(c.problem("the node registry names %s besides %s; pco runs on one registered node only",
			strings.Join(others, ", "), c.e.d.Node))
		return false
	}
	return true
}

// readWriter reads leader.json once; that identity is ours for the whole
// cycle. Without a valid one of this install, Cloudflare is left alone, but
// the inventory and the routes are still worked out for display.
func (c *cycleRun) readWriter() {
	c.e.us = planner.Writer{}
	w, found, err := c.e.d.Store.Writer()
	var why string
	switch {
	case err != nil:
		why = c.storeProblem("reading the writer identity", err)
	case !found:
		why = c.problem(problemNoWriter)
	case w.Validate() != nil:
		why = c.problem("the writer identity in leader.json is not valid: %v", w.Validate())
	case w.InstallID != c.install.ID:
		why = c.problem("leader.json names install %s, but this is install %s; run pco setup --recover", w.InstallID, c.install.ID)
	default:
		c.e.us = w
		return
	}
	c.hold(why)
	c.st.WriterVerdict = VerdictUnknown
}

// inspect refreshes the inventory, collects the routes, settles the claims and
// resolves the addresses of the winners. It returns false when the cycle has
// to hold. Everything it needs from the store is read before anything is
// saved, so that a hold changes nothing on disk.
func (c *cycleRun) inspect() bool {
	if !c.refresh() || !c.collect() || !c.load() || !c.guardVanished() {
		return false
	}
	c.settleClaims()
	return c.resolveTargets()
}

func (c *cycleRun) refresh() bool {
	ctx, cancel := c.e.timeout(c.ctx, refreshTimeout)
	defer cancel()
	c.snap = c.e.d.Inventory.Refresh(ctx)
	c.st.Complete = c.snap.Complete
	c.st.Problems = append(c.st.Problems, c.snap.Problems...)
	if !c.learnNodeAddrs() {
		return false
	}
	if !c.snap.Complete {
		c.hold(c.problem(problemIncomplete))
		return false
	}
	c.listing = listingOf(c.snap)
	return true
}

// load reads the claims and the bindings and builds the denylist.
func (c *cycleRun) load() bool {
	claims, err := c.e.d.Store.Claims()
	if err != nil {
		c.hold(c.storeProblem("reading the claims", err))
		return false
	}
	bindings, err := c.e.d.Store.Bindings()
	if err != nil {
		c.storeHold = true
		c.hold(c.problem("reading the bindings: %v", err))
		return false
	}
	deny, err := resolve.NewDenylist(c.e.addrs.list, nil)
	if err != nil {
		c.hold(c.problem("building the denylist: %v", err))
		return false
	}
	c.stored, c.bindings, c.deny = claims, bindings, deny
	return true
}

// settleClaims decides which owner serves each hostname and saves the claims.
// Claims that cannot be saved keep Cloudflare as it is: a restart would decide
// from the old ones and might hand a published hostname to someone else.
func (c *cycleRun) settleClaims() {
	identity := make(map[string]string, len(c.snap.Guests))
	for _, g := range c.snap.Guests {
		identity[g.Ref.String()] = g.Identity
	}
	c.claims = planner.ResolveClaims(planner.ClaimInput{
		Routes:   c.col.Routes,
		Held:     c.col.Held,
		Claims:   c.stored,
		Identity: identity,
		Now:      c.now,
		Grace:    time.Duration(c.settings.Grace),
	})
	if err := c.e.d.Store.SaveClaims(c.claims.Claims); err != nil {
		// The same changes are made again in the next cycle: they become
		// events once they are saved.
		c.storeHold = true
		c.hold(c.problem("saving the claims: %v; nothing is changed at Cloudflare until they are saved", err))
		return
	}
	c.settled = true
	c.events = append(c.events, claimEvents(c.now, c.claims.Events)...)
}

// served maps every hostname to the owner that won it, when the cycle
// settled the claims; nil otherwise.
func (c *cycleRun) served() map[string]string {
	if !c.settled {
		return nil
	}
	out := make(map[string]string, len(c.claims.Winners))
	for _, rt := range c.claims.Winners {
		out[rt.Hostname] = rt.Owner()
	}
	return out
}

// build plans the Cloudflare state: the credentials and their zones first,
// then the plan itself. The plan is made even when Cloudflare is held, for
// the route states, unless the credentials could not be read: then the routes
// stay as the last cycle showed them.
func (c *cycleRun) build() {
	if !c.syncCredentials() {
		return
	}
	writer := c.e.us
	if writer.InstallID == "" {
		writer = planner.Writer{InstallID: c.install.ID}
	}
	c.plan = planner.Build(planner.BuildInput{
		Winners:   c.claims.Winners,
		Conflicts: c.claims.Conflicts,
		Claims:    c.claims.Claims,
		Targets:   c.targets(),
		Zones:     c.zones.planned,
		Writer:    writer,
	})
	c.st.Routes = c.routeViews()
}

// targets are the targets of the winners, with the level each was proven at.
func (c *cycleRun) targets() map[string]planner.ResolvedTarget {
	out := make(map[string]planner.ResolvedTarget, len(c.results))
	for host, res := range c.results {
		t := res.Target
		t.Level = string(res.Level)
		out[host] = t
	}
	return out
}

// reconcile brings Cloudflare and the connectors in line with the plan,
// unless the cycle holds them.
func (c *cycleRun) reconcile() {
	if c.cfHold {
		return
	}
	if !c.goOn("before the tunnels") {
		return
	}
	if !c.reconcileTunnels() || !c.goOn("before the connectors") {
		return
	}
	c.reconcileConnectors()
	if !c.goOn("before DNS") {
		return
	}
	c.reconcileDNS()
}

// goOn reports whether the cycle's context still lets it go on.
func (c *cycleRun) goOn(where string) bool {
	if err := c.ctx.Err(); err != nil {
		c.hold(c.problem("the cycle ended %s (%v); the rest is left as it is", where, err))
		return false
	}
	return true
}
