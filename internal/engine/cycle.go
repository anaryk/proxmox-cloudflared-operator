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

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
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
	admissionApprove      = "approve"
	issueWaitingApproval  = "waiting for approval"
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
	results   map[string]resolve.Result // of the winners, by hostname
	zones     zoneSet
	plan      planner.Plan

	// cfHold says that nothing is changed at Cloudflare or on the
	// connectors in this cycle; the problems say why.
	cfHold  bool
	recheck recheck
}

func (e *Engine) newCycle(ctx context.Context) *cycleRun {
	now := e.d.Now()
	return &cycleRun{e: e, ctx: ctx, now: now, st: e.State().carried(now)}
}

// run goes through the steps of a cycle in order. A step that cannot go on
// safely ends the cycle; what it found so far is the state.
func (c *cycleRun) run() State {
	if c.prepare() && c.inspect() {
		c.build()
		c.reconcile()
	}
	return c.st.normalized()
}

func (c *cycleRun) problem(format string, args ...any) {
	c.st.Problems = append(c.st.Problems, fmt.Sprintf(format, args...))
}

// storeProblem reports a store error. The cases that need the admin are named
// as such, never taken for "nothing stored".
func (c *cycleRun) storeProblem(doing string, err error) {
	switch {
	case errors.Is(err, store.ErrNotMounted):
		c.problem(problemNotMounted)
	case errors.Is(err, store.ErrNoRoot):
		c.problem(problemNotSetUp)
	default:
		c.problem("%s: %v", doing, err)
	}
}

// prepare reads the settings, the install, the node registry and the writer
// identity. Without the first three nothing else is done.
func (c *cycleRun) prepare() bool {
	s, err := c.e.d.Store.Settings()
	if err != nil {
		c.storeProblem("reading the settings", err)
		return false
	}
	c.settings = s
	c.st.Mode = modeName(s.ObserveOnly)
	c.e.interval.Store(int64(s.PollInterval))

	inst, found, err := c.e.d.Store.Install()
	switch {
	case err != nil:
		c.storeProblem("reading the install identity", err)
		return false
	case !found:
		c.problem(problemNotSetUp)
		return false
	}
	c.install = inst
	if !c.registered() {
		return false
	}
	c.readWriter()
	return true
}

// registered checks that the node registry names this node and no other:
// until the cluster milestone exactly one node may run pco.
func (c *cycleRun) registered() bool {
	nodes, err := c.e.d.Store.Nodes()
	if err != nil {
		c.storeProblem("reading the node registry", err)
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
		c.problem(problemNotRegistered, c.e.d.Node)
		return false
	case len(others) > 0:
		slices.Sort(others)
		c.problem("the node registry names %s besides %s; pco runs on one registered node only",
			strings.Join(others, ", "), c.e.d.Node)
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
	switch {
	case err != nil:
		c.storeProblem("reading the writer identity", err)
	case !found:
		c.problem(problemNoWriter)
	case w.Validate() != nil:
		c.problem("the writer identity in leader.json is not valid: %v", w.Validate())
	case w.InstallID != c.install.ID:
		c.problem("leader.json names install %s, but this is install %s; run pco setup --recover", w.InstallID, c.install.ID)
	default:
		c.e.us = w
		return
	}
	c.cfHold = true
}

// inspect refreshes the inventory, collects the routes, settles the claims and
// resolves the addresses of the winners. It returns false when the cycle has
// to hold. Everything it needs from the store is read before anything is
// saved, so that a hold changes nothing on disk.
func (c *cycleRun) inspect() bool {
	if !c.refresh() || !c.collect() || !c.load() {
		return false
	}
	c.settleClaims()
	return c.resolveTargets()
}

func (c *cycleRun) refresh() bool {
	ctx, cancel := context.WithTimeout(c.ctx, refreshTimeout)
	defer cancel()
	c.snap = c.e.d.Inventory.Refresh(ctx)
	c.st.Complete = c.snap.Complete
	c.st.Problems = append(c.st.Problems, c.snap.Problems...)
	if !c.learnNodeAddrs() {
		return false
	}
	if !c.snap.Complete {
		c.problem(problemIncomplete)
		return false
	}
	return true
}

// learnNodeAddrs adds the node addresses of the snapshot to the ones known,
// which start with those saved before. Only a cycle that goes on saves them.
func (c *cycleRun) learnNodeAddrs() bool {
	a := &c.e.addrs
	if !a.loaded {
		saved, err := c.e.d.Store.NodeAddrs()
		if err != nil {
			c.problem("reading the saved node addresses: %v", err)
			return false
		}
		a.list, a.loaded = saved, true
	}
	merged := slices.Compact(slices.SortedFunc(slices.Values(append(slices.Clone(a.list), c.snap.NodeAddrs()...)), netip.Addr.Compare))
	if len(merged) != len(a.list) {
		a.list, a.dirty = merged, true
	}
	return true
}

// collect turns the guests and the manual routes into candidate routes and
// drops those of guests that wait for approval.
func (c *cycleRun) collect() bool {
	manual, err := c.e.d.Store.ManualRoutes()
	if err != nil {
		c.storeProblem("reading the manual routes", err)
		return false
	}
	c.manual = manual
	if c.settings.Admission == admissionApprove {
		approvals, err := c.e.d.Store.Approvals()
		if err != nil {
			c.storeProblem("reading the approvals", err)
			return false
		}
		c.approvals = approvals
	}
	c.col = c.collectFrom(c.snap)
	c.st.Issues = c.col.Issues
	if c.col.PolicyInvalid {
		c.problem(problemPolicyInvalid)
		return false
	}
	return true
}

func (c *cycleRun) collectFrom(snap inventory.Snapshot) planner.Collected {
	col := planner.Collect(snap.Guests, c.manual, planner.Settings{
		GateTag:    c.settings.GateTag,
		AllowHosts: c.settings.AllowHosts,
		DenyHosts:  c.settings.DenyHosts,
	})
	if c.approvals != nil {
		col = admit(col, snap, c.approvals)
	}
	return col
}

// admit drops the routes and held names of guests whose identity is not
// approved, with one issue per guest. Manual routes are the admin's own and
// pass.
func admit(col planner.Collected, snap inventory.Snapshot, approvals map[string]string) planner.Collected {
	waiting := make(map[model.GuestRef]bool)
	approved := func(owner string) bool {
		ref, err := model.ParseGuestRef(owner)
		if err != nil {
			return true
		}
		g, ok := snap.Guest(ref)
		if ok && g.Identity != "" && approvals[owner] == g.Identity {
			return true
		}
		waiting[ref] = true
		return false
	}
	col.Routes = slices.DeleteFunc(slices.Clone(col.Routes), func(rt model.Route) bool { return !approved(rt.Owner()) })
	col.Held = slices.DeleteFunc(slices.Clone(col.Held), func(h planner.HeldName) bool { return !approved(h.Owner) })
	issues := slices.Clone(col.Issues)
	for ref := range waiting {
		issues = append(issues, planner.Issue{Guest: ref, Msg: issueWaitingApproval})
	}
	slices.SortStableFunc(issues, compareIssues)
	col.Issues = issues
	return col
}

// compareIssues is the order Collect gives its issues: by guest, then
// position, issues of the settings last.
func compareIssues(a, b planner.Issue) int {
	return cmp.Or(
		model.CompareOwners(a.Guest.String(), b.Guest.String()),
		cmp.Compare(a.Line, b.Line),
		cmp.Compare(a.Col, b.Col),
	)
}

// load reads the claims and the bindings and builds the denylist.
func (c *cycleRun) load() bool {
	claims, err := c.e.d.Store.Claims()
	if err != nil {
		c.storeProblem("reading the claims", err)
		return false
	}
	bindings, err := c.e.d.Store.Bindings()
	if err != nil {
		c.problem("reading the bindings: %v", err)
		return false
	}
	deny, err := resolve.NewDenylist(c.e.addrs.list, nil)
	if err != nil {
		c.problem("building the denylist: %v", err)
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
		c.problem("saving the claims: %v; nothing is changed at Cloudflare until they are saved", err)
		c.cfHold = true
	}
	c.events = append(c.events, claimEvents(c.now, c.claims.Events)...)
}

// resolveTargets resolves the address of every winner, with the binding of
// its hostname, and saves the bindings. A cycle whose context ends in the
// middle plans and changes nothing.
func (c *cycleRun) resolveTargets() bool {
	c.results = c.resolveAll(c.bindings, c.deny)
	if err := c.ctx.Err(); err != nil {
		c.problem(problemStoppedResolve, err)
		return false
	}

	next := make(map[string]resolve.Binding, len(c.results))
	for host, res := range c.results {
		if res.Binding != nil {
			next[host] = *res.Binding
		}
	}
	if err := c.e.d.Store.SaveBindings(next); err != nil {
		c.problem("saving the bindings: %v", err)
	}
	if c.e.addrs.dirty {
		if err := c.e.d.Store.SaveNodeAddrs(c.e.addrs.list); err != nil {
			c.problem("saving the node addresses: %v", err)
		} else {
			c.e.addrs.dirty = false
		}
	}
	return true
}

// resolveAll runs Resolve for each winner, at most resolveConcurrency at a
// time and each with a deadline of its own, so that one guest that reports
// many addresses cannot hold up the others.
func (c *cycleRun) resolveAll(stored map[string]resolve.Binding, deny resolve.Denylist) map[string]resolve.Result {
	winners := c.claims.Winners
	out := make([]resolve.Result, len(winners))
	slots := make(chan struct{}, resolveConcurrency)
	done := make(chan struct{})
	started := 0
	for i, rt := range winners {
		select {
		case slots <- struct{}{}:
		case <-c.ctx.Done():
		}
		if c.ctx.Err() != nil {
			break
		}
		var prev *resolve.Binding
		if b, ok := stored[rt.Hostname]; ok {
			prev = &b
		}
		started++
		go func() {
			defer func() { <-slots; done <- struct{}{} }()
			ctx, cancel := context.WithTimeout(c.ctx, resolveTimeout)
			defer cancel()
			out[i] = c.e.d.Resolver.Resolve(ctx, rt, c.snap, prev, deny)
		}()
	}
	for range started {
		<-done
	}
	results := make(map[string]resolve.Result, len(winners))
	for i, rt := range winners[:started] {
		results[rt.Hostname] = out[i]
	}
	return results
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

func (c *cycleRun) targets() map[string]planner.ResolvedTarget {
	out := make(map[string]planner.ResolvedTarget, len(c.results))
	for host, res := range c.results {
		out[host] = res.Target
	}
	return out
}

// syncCredentials reads the credentials, keeps a client for each and works
// out the zones. It returns false when the credentials cannot be read.
// Credentials that cannot be read, none at all, or zones that were never
// listed hold Cloudflare.
func (c *cycleRun) syncCredentials() bool {
	creds, err := c.e.d.Store.Credentials()
	if err != nil {
		c.storeProblem("reading the credentials", err)
		c.cfHold = true
		return false
	}
	c.e.syncClients(c, creds)
	info := make([]credentialInfo, len(creds))
	ids := make([]string, len(creds))
	for i, cr := range creds {
		info[i] = credentialInfo{id: cr.ID, label: cr.Label, kind: cr.Kind}
		ids[i] = cr.ID
	}
	c.st.Credentials = c.e.credentialViews(info)
	if len(creds) == 0 {
		c.problem(problemNoCredential)
		c.cfHold = true
		return true
	}
	c.refreshZones(ids)
	c.zones = c.e.zones.set(ids, c.settings.ZonePins)
	c.st.Problems = append(c.st.Problems, c.zones.problems...)
	if !c.zones.ready {
		c.cfHold = true
	}
	return true
}

// syncClients keeps one client per stored credential, built anew when its
// token changed. A credential whose client is new has its zones listed anew.
func (e *Engine) syncClients(c *cycleRun, creds []store.Credential) {
	seen := make(map[string]bool, len(creds))
	for _, cr := range creds {
		seen[cr.ID] = true
		if old, ok := e.tokens[cr.ID]; ok && old.Equal(cr.Token) && e.clients[cr.ID] != nil {
			continue
		}
		e.dropClient(cr.ID)
		api, err := e.d.NewClient(cr)
		if err != nil {
			c.problem("credential %s: building its client: %v", cr.ID, err)
			continue
		}
		e.clients[cr.ID] = api
		e.tokens[cr.ID] = cr.Token
		e.zones.due = true
	}
	for _, id := range slices.Sorted(maps.Keys(e.tokens)) {
		if !seen[id] {
			e.dropClient(id)
		}
	}
}

// dropClient forgets the client of a credential and what was listed with it.
func (e *Engine) dropClient(id string) {
	delete(e.clients, id)
	delete(e.tokens, id)
	e.zones.forget(id)
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
		c.problem("the cycle ended %s (%v); the rest is left as it is", where, err)
		return false
	}
	return true
}
