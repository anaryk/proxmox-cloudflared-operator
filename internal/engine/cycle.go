package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
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
	resolveConcurrency = 32

	problemNotSetUp   = "pco is not set up on this node; run pco setup"
	problemNotMounted = "cluster filesystem is not mounted"
	problemNoWriter   = "no writer identity; run pco setup"
	// problemNoWriterAppliance is the line of an appliance, whose recovery
	// asks Cloudflare.
	problemNoWriterAppliance = "no writer identity; pco appliance recover draws one, with a Cloudflare credential stored"
	problemNotRegistered     = "node %s is not registered; run pco setup"
	problemPolicyInvalid     = "settings contain an invalid allow or deny pattern; nothing is changed until it is fixed"
	problemIncomplete        = "the inventory is incomplete; claims, bindings, tunnels, DNS and connectors are left as they are"
	problemNoCredential      = "no Cloudflare credential; add one with pco credential add"
	problemStoppedResolve    = "the cycle ended while addresses were resolved (%v); nothing is changed"
	problemAgentRefused      = "the agent of this node does not take the desired state of this cycle: %v; " +
		"nothing is changed at Cloudflare, and the egress set, the gateway and the connectors are left as they are"
	problemAgentRefusedLate = "the agent of this node did not take the desired state of this cycle after its tunnel run: %v; " +
		"DNS and further changes are held, and the connectors are left as they run"
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
	approvals map[string]store.Approval
	stored    map[string]planner.Claim
	bindings  map[string]resolve.Binding
	segments  map[string]store.Segment // acknowledged, by id
	access    accessView               // the access control as this cycle goes by it
	deny      resolve.Denylist
	// soft is the soft deny list as this cycle saves it, once it goes on;
	// softSaved says whether the store has it so already.
	soft      store.SoftDeny
	softSaved bool
	col       planner.Collected
	claims    planner.ClaimResult
	refused   []planner.RouteStatus     // the routes the hostname policy took out before the claims
	unzoned   []planner.RouteStatus     // the routes in no zone ever served, taken out before the claims
	settled   bool                      // the claims were settled and saved
	listing   listing                   // what this cycle saw of the guests; empty unless it saw all of them
	results   map[string]resolve.Result // of the winners, by hostname, with what the identity minimum holds back
	guestIDs  map[string]string         // the identity of each guest of the snapshot, by reference; built on first use
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
	// unreported are the tunnels with an id the last state showed in the
	// accounts the tunnel run did not report on, as those of frozen accounts:
	// the connector step looks them up again.
	unreported []TunnelView
	// connected says that the connector step ran: existing are the tunnels it
	// found to exist and shown all whose connectors it shows. before are the
	// statuses of the connectors as the last cycle read them.
	connected bool
	ensured   map[string]bool // the tunnels whose connector this cycle kept running
	reread    map[string]bool // the tunnels whose token this cycle read again
	existing  []reconcile.TunnelState
	shown     []reconcile.TunnelState
	before    []connector.Status
	// offer is what this cycle's state shows waiting for a confirmation.
	offer confirmable
	// lost are the addresses whose binding lost its proof in this cycle, and
	// egressHeld says why the tunnel run writes nothing: the egress filter
	// could not be given the targets of the cycle.
	lost       map[netip.Addr]bool
	egressHeld string

	// writerRead says that readWriter read a writer identity this process
	// may use, and gateVerdict is what the tunnel run found of the writer,
	// for the gate of the next cycle. gatewayHeld says why the tunnel run
	// writes nothing: the gateway could not be programmed. legKept says, by
	// hostname, why a target on a leg keeps a rule that reaches nothing,
	// legIfaces is, by network, the interface of its leg, legNets holds the
	// networks with a leg on this node, and frozen the service addresses
	// that forward nothing in this cycle: their routes left their address.
	writerRead  bool
	gateVerdict string
	gatewayHeld string
	legKept     map[string]string
	legIfaces   map[string]string
	legNets     map[string]bool
	frozen      map[netip.Addr]bool

	// waitWhy says why the admin's requests wait, when the DNS step was
	// reached; confirmWhy why a confirmation the DNS run could not keep
	// waits, and adoptWaits why each adoption waits. adopted holds the names
	// whose adoption reached its write in this cycle.
	waitWhy    string
	confirmWhy string
	adoptWaits map[string]string
	adopted    map[string]bool

	// waiting is what the runs of the cycle left for Cloudflare's rate limit
	// and not said yet.
	waiting reconcile.Waiting

	// tenants are the guests outside the pool of the appliance configured
	// with one of its MACs, each with that MAC: their routes are not served.
	tenants map[model.GuestRef]string

	// node is the agent of this node, and steps the pass of it the cycle
	// runs, from the resolution to the end of the cycle; netLegs are the
	// networks with the marks of their legs, which the pins of targets on a
	// leg name.
	node    *agent.Agent
	steps   *agent.Steps
	netLegs []gateway.Leg
	// agentRefused says that the agent would not follow this cycle's
	// desired state: the cycle feeds it nothing.
	agentRefused bool
	// cluster is the identity of the cluster the node is in, as this cycle
	// read it; leaseHeld says that the lease let the cycle write nothing.
	cluster   store.ClusterIdentity
	leaseHeld bool
	// nodes are the entries of the node registry as this cycle read and
	// wrote them, by name, and reports what the agents reported, read once
	// by the cycle.
	nodes   map[string]store.NodeRev
	reports map[string]NodeReport
	// addOnly says why nothing is withdrawn at Cloudflare in this cycle:
	// no rule a tunnel serves is dropped or blocked, no record deleted, no
	// connector pruned. Empty in a cycle that writes as it sees.
	addOnly string
	// fleet is what the agents reported, checked again, cover what the
	// cycle judges of coverage, and placements the placements stored when
	// the cycle began; all three are of a cluster only.
	fleet      *fleetView
	cover      coverageView
	placements map[string]store.Placement
	// placing is where the cycle placed the routes, moves the moves under
	// way, and movesFailed says that they could not be read.
	placing     *placement
	moves       map[string]store.Move
	movesFailed bool
	// runTokens are the tokens of the connectors this node keeps running,
	// by tunnel id, and pruned says that the cycle removed the connectors of
	// every other tunnel.
	runTokens map[string]string
	pruned    bool
	// joining are, by hostname, the joining node that does not reach a
	// shared route: the route moves to its guest.s node before the node
	// is admitted.
	joining map[string]string
	// replicaCache is the replica set of the cycle.
	replicaCache []string
}

func (e *Engine) newCycle(ctx context.Context) *cycleRun {
	now := e.d.Now()
	return &cycleRun{e: e, ctx: ctx, now: now, st: e.State().carried(now)}
}

// run goes through the steps of a cycle in order. A step that cannot go on
// safely ends the cycle; what it found so far is the state.
func (c *cycleRun) run() State {
	defer c.endSteps()
	c.st.Problems = append(c.st.Problems, c.e.d.Problems...)
	c.expireRequests()
	c.noteEgress()
	if c.prepare() && c.inspect() {
		c.build()
		c.feedEgress()
		if why, saved := c.saveMemory(); !saved {
			c.hold(why)
		}
		c.reconcile()
		c.advanceMoves()
		c.savePlacements()
		c.retirePinned()
		c.publishRemote()
	}
	c.watchTunnels()
	c.showRollouts()
	c.showNetworks()
	if !c.leaseHeld {
		c.saveMemory()
	} else if r := c.e.d.Remote; r != nil {
		// A controller that may not write serves no agent a snapshot.
		r.Serve(nil)
	}
	c.noteRogueConnectors()
	c.sayWaiting()
	if !c.checked {
		c.markUnchecked()
	}
	c.notePending(c.adoptWaits)
	c.st.Waiting = c.offer.waiting(c.st.Routes)
	c.st.Offer = offerOf(c.st.Waiting)
	if views := c.nodeViews(); views != nil {
		c.st.Nodes = views
	}
	if c.fleet != nil {
		c.st.Cluster = c.clusterView()
	}
	c.st.FinishedAt = c.e.d.Now()
	c.e.lastVerdict = cmp.Or(c.gateVerdict, verdictNoRun)
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
	s, notes, err := c.e.d.Store.LoadSettings()
	if err != nil {
		c.hold(c.storeProblem("reading the settings", err))
		return false
	}
	for _, note := range notes {
		c.problem("%s", note)
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
	if !c.readRegistry() {
		return false
	}
	node, err := c.e.fleet.agentOf(inst)
	if err != nil {
		c.hold(c.problem("starting the agent of this node: %v", err))
		return false
	}
	if c.node = node; c.e.egress == nil {
		c.e.egress = node.EgressSet()
	}
	c.readWriter()
	if !c.leaseAdmits() {
		c.leaseHeld = true
		return false
	}
	switch err := c.e.recall(c.install.ID); {
	case err != nil:
		c.storeHold = true
		c.hold(c.problem("reading what the engine remembered: %v; nothing is changed at Cloudflare until it can be read: "+
			"fix the file or remove it; removing it forgets the connectors kept for tunnels no credential sees, "+
			"the zones that left their listing and the guests confirmed gone", err))
	case c.e.setAside != "":
		c.problem("%s", c.e.setAside)
		c.e.setAside = ""
	}
	if !c.storeHold && c.readPlacements() {
		c.registryStep()
	}
	return true
}

// readWriter reads leader.json once; that identity is ours for the whole
// cycle. Without a valid one of this install, Cloudflare is left alone, but
// the inventory and the routes are still worked out for display. In the
// appliance it must be of this start of the container too: one of an earlier
// start may be the record of a copy, and a new epoch is drawn only once the
// container proved to be the one installed. A verdict of unknown, which only
// says that leader.json could not be used, goes with the first cycle that
// uses it, also when that cycle then holds for another reason, as for a
// missing credential; the verdicts of Cloudflare stay until it is asked again.
func (c *cycleRun) readWriter() {
	c.e.us = planner.Writer{}
	w, found, err := c.e.d.Store.Writer()
	var why string
	switch {
	case err != nil:
		why = c.storeProblem("reading the writer identity", err)
	case !found && c.appliance():
		why = c.problem(problemNoWriterAppliance)
	case !found:
		why = c.problem(problemNoWriter)
	case w.Validate() != nil:
		why = c.problem("the writer identity in leader.json is not valid: %v", w.Validate())
	case w.InstallID != c.install.ID:
		why = c.problem("leader.json names install %s, but this is install %s; run %s", w.InstallID, c.install.ID, c.recoverCommand())
	case c.e.d.Incarnation != "" && w.Incarnation != c.e.d.Incarnation:
		why = c.problem(problemEarlierStart)
	default:
		c.e.us, c.writerRead = w, true
		if c.st.WriterVerdict == VerdictUnknown {
			c.st.WriterVerdict = VerdictOK
		}
		return
	}
	c.hold(why)
	c.st.WriterVerdict = VerdictUnknown
}

// inspect refreshes the inventory, collects the routes, reads the
// credentials and their zones, settles the claims and resolves the addresses
// of the winners. It returns false when the cycle has to hold. Everything it
// needs from the store is read before anything is saved, so that a hold
// changes nothing on disk. The zones come before the claims: a route the
// hostname policy refuses, as the apex of a zone, must not take a claim.
func (c *cycleRun) inspect() bool {
	if !c.refresh() || !c.collect() {
		return false
	}
	c.managed()
	if !c.load() || !c.guardVanished() || !c.syncCredentials() {
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
	// A copy learns nothing from the snapshot, not even into its own store.
	if !c.identify() {
		return false
	}
	c.checkGateway()
	if !c.quorate() || !c.learnNodeAddrs() {
		return false
	}
	if !c.snap.Complete {
		c.hold(c.problem(problemIncomplete))
		return false
	}
	c.listing = listingOf(c.snap)
	return true
}

// load reads the claims, the bindings and the acknowledged segments and
// builds the denylist, with the gateways of the SDN subnets denied and the
// gateways and resolvers of the nodes and the appliance soft-denied.
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
	segments, err := c.e.d.Store.Segments()
	if err != nil {
		c.hold(c.storeProblem("reading the acknowledged segments", err))
		return false
	}
	deny, err := resolve.NewDenylist(c.e.addrs.list, nil)
	if err != nil {
		c.hold(c.problem("building the denylist: %v", err))
		return false
	}
	acc := c.e.accessNow(c.now, c.snap)
	if !c.learnSoft(acc) {
		return false
	}
	c.stored, c.bindings, c.segments, c.access = claims, bindings, segments, acc
	c.deny = deny.WithDenied(sdnGateways(acc.subnets)).WithSoft(softMap(c.soft))
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
	routes, refused := planner.RefuseUnnamed(c.col.Routes, c.zones.planned, c.settings.AllowHosts)
	c.refused = refused
	for _, st := range refused {
		c.problem("hostname %s of %s is not published: %s", st.Hostname, st.Owner, st.Reason)
	}
	held, heldUnnamed := planner.RefuseHeldUnnamed(c.col.Held, c.zones.planned, c.settings.AllowHosts)
	refused = slices.Concat(refused, heldUnnamed)
	if c.e.remembered {
		// Unread, the memory does not say which zones were served.
		var heldUnzoned []planner.RouteStatus
		served := c.e.zones.servedOnce()
		routes, c.unzoned = planner.RefuseUnzoned(routes, c.zones.planned, served)
		held, heldUnzoned = planner.RefuseHeldUnzoned(held, c.zones.planned, served)
		refused = slices.Concat(refused, c.unzoned, heldUnzoned)
	}
	c.claims = planner.ResolveClaims(planner.ClaimInput{
		Routes:   routes,
		Held:     held,
		Claims:   c.stored,
		Identity: identity,
		Refused:  refused,
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

// build plans the Cloudflare state. The plan is made even when Cloudflare is
// held, for the route states; a cycle that could not read the credentials
// does not get here, and the routes stay as the last cycle showed them.
func (c *cycleRun) build() {
	writer := c.e.us
	if writer.InstallID == "" {
		writer = planner.Writer{InstallID: c.install.ID}
	}
	c.place()
	in := planner.BuildInput{
		Winners:   c.claims.Winners,
		Conflicts: c.claims.Conflicts,
		Claims:    c.claims.Claims,
		Targets:   c.targets(),
		Zones:     c.zones.planned,
		Writer:    writer,
		Refused:   slices.Concat(c.refused, c.unzoned),
	}
	if p := c.placing; p != nil {
		in.Placements, in.Also = p.inputs, p.also
		maps.Copy(in.Targets, p.targets)
	}
	c.plan = planner.Build(in)
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
	if c.cfHold || !c.goOn("before DNS") {
		return
	}
	c.reconcileDNS()
}

// begin starts the pass of the agent of this node that the cycle runs in
// steps, with s; the agent's lock is held until the cycle ends.
func (c *cycleRun) begin(s agentproto.Snapshot) (*agent.Steps, error) {
	// The seq of the snapshot the pass will end with is at least this one:
	// the agent checks the snapshot it begins with as it checks that one.
	v := c.node.View()
	s.Seq = c.e.seqs.floor(s, v.Epoch, v.Seq)
	st, err := c.node.Begin(c.ctx, s)
	if err != nil {
		return nil, err
	}
	c.steps = st
	return st, nil
}

// endSteps ends the pass of the agent, if the cycle began one.
func (c *cycleRun) endSteps() {
	if c.steps != nil {
		c.steps.End()
	}
}

// goOn reports whether the cycle's context still lets it go on.
func (c *cycleRun) goOn(where string) bool {
	if err := c.ctx.Err(); err != nil {
		c.hold(c.problem("the cycle ended %s (%v); the rest is left as it is", where, err))
		return false
	}
	return true
}
