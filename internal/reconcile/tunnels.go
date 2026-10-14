package reconcile

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	// putInterval is the least time between two writes of the configuration
	// of one tunnel.
	putInterval = 15 * time.Second

	heldObserve = "observe mode"
)

// TunnelReconciler brings the tunnels of each account, the shared one and
// those pinned to a node, in line with their plans. It remembers when it
// last wrote the configuration of each tunnel and what it last read of each
// pinned one, so one instance should serve a process for its whole life.
// Calls to Run are serialised.
type TunnelReconciler struct {
	clients Clients
	writer  func() (us, stored planner.Writer, err error)
	now     func() time.Time
	log     zerolog.Logger

	mu        sync.Mutex
	lastPut   map[string]time.Time    // by account id and tunnel id
	lastSweep map[string]time.Time    // of the probe tunnels, by account id
	pinned    map[string]pinnedMemory // the last read of each pinned tunnel, by account id and name
	listed    map[string]pinnedListing
	hint      string // what a foreign verdict says to do; empty for the host's words
}

// SetRecoverHint sets what the next runs say to do about a configuration
// written by a writer leader.json does not know, in place of "another
// installation uses install id <id>, or the store was lost (pco setup
// --recover)"; empty puts that back.
func (r *TunnelReconciler) SetRecoverHint(hint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hint = hint
}

// NewTunnelReconciler returns a reconciler that reaches each account through
// the client of its credential.
//
// writer returns the identity this process writes as (us) and leader.json as
// stored now (stored). Run asks it before its first call to Cloudflare; in
// Enforce mode again before each create, before it reads the configuration of
// each tunnel, before it reads again a configuration that was not found, and
// before it deletes a probe tunnel; and once more before it stops on the
// sentinel of another writer. It should fail whenever this process may
// not write at all, as when its lease is lost.
func NewTunnelReconciler(clients Clients, writer func() (us, stored planner.Writer, err error), now func() time.Time, log zerolog.Logger) *TunnelReconciler {
	return &TunnelReconciler{
		clients:   clients,
		writer:    writer,
		now:       now,
		log:       log,
		lastPut:   make(map[string]time.Time),
		lastSweep: make(map[string]time.Time),
		pinned:    make(map[string]pinnedMemory),
		listed:    make(map[string]pinnedListing),
	}
}

// TunnelResult is what a run found and did.
type TunnelResult struct {
	Tunnels  []TunnelState // one per planned tunnel and per known tunnel without a plan, by account id and name
	Actions  []Action      // in the order performed or planned
	Problems []string
	Verdict  WriterVerdict // WriterProceed unless another writer stopped the run
	// Sentinel is the writer whose sentinel in a configuration stopped the
	// run; zero when the run went on, or stopped on leader.json alone.
	Sentinel planner.Writer
	// Waiting names the tunnels the run could not look up, as Cloudflare's
	// rate limit refused it; that is not a problem of its own.
	Waiting Waiting
}

// Run brings tunnels in line with plans: the shared tunnel of an account
// and the ones pinned to a node are each a tunnel of their own, created,
// written and rate limited on its own. In accounts listed in `known`
// (account id -> credential id) a tunnel of this install without a plan gets
// an empty rule set when it exists: the shared tunnel, and the pinned ones
// found by the listing of the account. As such a tunnel is emptied, known
// must hold only accounts for which the caller has a complete, current plan.
//
// Run writes only as long as the writer callback names us as the writer
// stored in leader.json, with the same generation and nonce; otherwise it
// stops with WriterStale. A configuration is written only where it differs
// from the plan or holds settings pco does not manage, and only when the
// sentinels in it let this writer proceed: a stale or foreign verdict stops
// the run at once. A failure on one tunnel is a problem, and the run goes on
// with the next. In Observe mode Run only reads, and returns the actions it
// would take as held.
//
// The shared tunnel is read every run. A pinned tunnel is read when a write
// may be planned for it, that is when its plan changed since its
// configuration was last verified, and otherwise once a minute for drift;
// between the reads the run reports what it last saw of it. The pinned
// tunnels of a known account are listed once a minute from the first run
// that plans one there.
//
// A tunnel the run could not look up or create, or did not reach because it
// stopped, is returned with Unknown set. Callers must never prune connectors
// or records for an Unknown tunnel: only Exists == false && !Unknown means
// that the tunnel is absent. The result's Tunnels, with its Verdict, are what
// the DNS run of the same cycle must be given: a record is pointed only at a
// tunnel this run verified.
//
// An enforcing run that did not stop also deletes the probe tunnels the
// credential check left behind in the accounts whose tunnel it looked up: a
// tunnel named as a probe of this install, more than ten minutes old and
// without connectors. It deletes no other tunnel. An account is swept at most
// every ten minutes, the first time by the first enforcing run.
func (r *TunnelReconciler) Run(ctx context.Context, plans []planner.TunnelPlan, known map[string]string, mode Mode) TunnelResult {
	return r.run(ctx, plans, known, mode, heldObserve, nil)
}

// RunAddOnly runs as Run does, but withdraws nothing of the caller's: a
// tunnel is written with the rules of its plan and every rule it serves now
// for a hostname keep accepts that the plan leaves out or would answer 503
// for. Only a configuration that carries a sentinel of this install, which
// pco wrote, keeps anything: a rule someone else put into a tunnel is never
// written again under this writer's sentinel. The state of each tunnel says
// the rules it kept beyond its plan. Pinned tunnels are read in such a run,
// whatever was last seen of them.
func (r *TunnelReconciler) RunAddOnly(ctx context.Context, plans []planner.TunnelPlan, known map[string]string, mode Mode, keep func(host string) bool) TunnelResult {
	if keep == nil {
		keep = func(string) bool { return false }
	}
	return r.run(ctx, plans, known, mode, heldObserve, keep)
}

// RunHeld runs as Run does in Observe mode, and gives why as the reason of
// every write it holds: a run whose writes the caller holds for a reason of
// its own still finds out what each tunnel holds.
func (r *TunnelReconciler) RunHeld(ctx context.Context, plans []planner.TunnelPlan, known map[string]string, why string) TunnelResult {
	return r.run(ctx, plans, known, Observe, why, nil)
}

func (r *TunnelReconciler) run(ctx context.Context, plans []planner.TunnelPlan, known map[string]string, mode Mode, held string, keep func(string) bool) TunnelResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	run := &tunnelRun{r: r, mode: mode, held: held, keep: keep, hint: r.hint, listedNow: make(map[string]bool)}
	goOn := run.start()
	if goOn {
		run.listPinned(ctx, plans, known)
	}
	for _, t := range run.targets(plans, known) {
		if goOn && ctx.Err() != nil {
			run.problem(fmt.Sprintf("run stopped: %v", ctx.Err()))
			goOn = false
		}
		if !goOn {
			run.res.Tunnels = append(run.res.Tunnels, t.unknown())
			continue
		}
		if !t.pinned() {
			var st TunnelState
			st, goOn = run.reconcile(ctx, t)
			run.res.Tunnels = append(run.res.Tunnels, st)
			continue
		}
		st, ok := run.lastSeen(t)
		switch {
		case run.keep != nil:
			st, goOn = run.reconcile(ctx, t)
		case !ok:
			st, goOn = run.reconcile(ctx, t)
			run.remember(t, st)
		}
		run.res.Tunnels = append(run.res.Tunnels, st)
	}
	if goOn && mode == Enforce {
		run.sweepProbeTunnels(ctx)
	}
	return run.res
}

// target is one tunnel of an account and the rules it should have.
type target struct {
	account    string
	credential string
	name       string
	kind       string // as the plan says it: empty for the shared tunnel
	node       string
	rules      []planner.IngressRule
	planned    bool // false for a tunnel without a plan: it is emptied, never created
	// tun is the tunnel as the listing of this run found it, when listed.
	tun    cfapi.Tunnel
	listed bool
}

func (t target) String() string { return t.name + " in account " + t.account }

func (t target) pinned() bool { return t.kind == planner.PlacementPinned }

// key names the tunnel among all accounts.
func (t target) key() string { return t.account + "/" + t.name }

// unknown is the state of a tunnel that the run knows nothing about.
func (t target) unknown() TunnelState {
	return TunnelState{AccountID: t.account, CredentialID: t.credential, Name: t.name, Kind: t.kind, Node: t.node, Unknown: true}
}

// targets lists the tunnels to look at, by account id and name: one per
// plan, and one that serves nothing for the shared tunnel of each known
// account without a shared plan and for each pinned tunnel listed in a known
// account without a plan. Without a writer identity the name of the shared
// one is not known and left empty.
func (run *tunnelRun) targets(plans []planner.TunnelPlan, known map[string]string) []target {
	out := make([]target, 0, len(plans)+len(known))
	planned := make(map[tunnelRef]bool, len(plans))
	plannedIn := make(map[string]bool, len(plans))
	for _, p := range plans {
		kind := p.Kind
		if kind == planner.PlacementShared {
			kind = ""
		}
		out = append(out, target{
			account: p.AccountID, credential: p.CredentialID, name: p.Name, kind: kind, node: p.Node, rules: p.Rules, planned: true,
		})
		planned[tunnelRef{p.AccountID, p.Name}] = true
		plannedIn[p.AccountID] = true
	}
	var shared string
	if run.us.InstallID != "" {
		shared = planner.TunnelName(run.us.InstallID)
	}
	for account, credential := range known {
		hasShared := planned[tunnelRef{account, shared}]
		if shared == "" {
			hasShared = plannedIn[account]
		}
		if !hasShared {
			out = append(out, target{account: account, credential: credential, name: shared, rules: emptyRules(run.us)})
		}
		if shared == "" {
			continue
		}
		// The listing may be up to a minute old: a tunnel it names may be
		// gone, which the lookup then says, and one made since is seen by
		// the next listing, or by its own plan.
		for _, tun := range run.r.listed[account].tunnels {
			if planned[tunnelRef{account, tun.Name}] {
				continue
			}
			node, _ := planner.PinnedTunnelNode(run.us.InstallID, tun.Name)
			out = append(out, target{
				account: account, credential: credential, name: tun.Name, kind: planner.PlacementPinned, node: node, rules: emptyRules(run.us),
			})
		}
	}
	for i := range out {
		if out[i].pinned() {
			out[i].tun, out[i].listed = run.listedTunnel(out[i])
		}
	}
	slices.SortStableFunc(out, func(a, b target) int {
		return cmp.Or(cmp.Compare(a.account, b.account), cmp.Compare(a.name, b.name))
	})
	return out
}

// emptyRules is the ingress of a tunnel that serves nothing.
func emptyRules(us planner.Writer) []planner.IngressRule {
	return []planner.IngressRule{planner.SentinelRule(us), planner.CatchAllRule()}
}

// tunnelRun is the state of one Run.
type tunnelRun struct {
	r         *TunnelReconciler
	mode      Mode
	held      string            // why a write is held in Observe mode
	keep      func(string) bool // in a run that only adds, the hostnames whose served rules are kept
	hint      string            // what a foreign verdict says to do, as SetRecoverHint set it
	us        planner.Writer    // set once start has validated it
	listedNow map[string]bool   // accounts whose pinned tunnels this run listed
	res       TunnelResult
}

// start reads the identity the run writes as and reports whether the run may
// go on.
func (run *tunnelRun) start() bool {
	us, fault, stale := startWriter(run.r.writer)
	run.us = us
	return run.admit("", fault, stale)
}

// reread asks for the writer identity again before a write may follow, and
// reports whether the run goes on.
func (run *tunnelRun) reread(t target) bool {
	fault, stale := recheckWriter(run.r.writer, run.us)
	return run.admit(t.String()+": ", fault, stale)
}

// admit takes what a check of the writer found and reports whether the run
// may go on. A run that may not stops; as stale when another writer is
// stored.
func (run *tunnelRun) admit(prefix, fault string, stale bool) bool {
	switch {
	case stale:
		run.stop(prefix, WriterStale, fault)
	case fault != "":
		run.problem(prefix + fault)
	default:
		return true
	}
	return false
}

// stop ends the run with a verdict other than proceed.
func (run *tunnelRun) stop(prefix string, v WriterVerdict, why string) {
	end := "; writing stops"
	if v == WriterStale {
		end = "; this writer is stale and stops"
	}
	run.problem(prefix + why + end)
	run.res.Verdict = v
}

// reconcile brings the tunnel of one account in line. It returns what it
// found out about the tunnel and whether the run goes on.
func (run *tunnelRun) reconcile(ctx context.Context, t target) (TunnelState, bool) {
	st := t.unknown()
	api := run.r.clients[t.credential]
	if api == nil {
		run.problem(fmt.Sprintf("%s: no client for credential %s", t, t.credential))
		return st, true
	}
	if fault := run.planFault(t); fault != "" {
		run.problem(fmt.Sprintf("%s: %s", t, fault))
		return st, true
	}

	// The listing of this run already found a pinned tunnel it holds.
	tun, found, err := t.tun, t.listed, error(nil)
	if !t.listed {
		tun, found, err = api.FindTunnel(ctx, t.account, t.name)
	}
	switch {
	case err != nil && cfapi.IsRateLimited(err):
		run.waitRead(t.account)
		return st, true
	case err != nil:
		run.problem(fmt.Sprintf("%s: finding the tunnel: %v", t, err))
		return st, true
	}
	st.Unknown = false
	fresh := false
	if !found {
		switch {
		case !t.planned:
			return st, true
		case run.mode == Observe:
			run.act(t, CreateTunnel, createDetail(t), run.held)
			run.act(t, PutConfig, putDetail(t, cfapi.TunnelConfig{}), run.held)
			return st, true
		case !run.reread(t):
			// A create is a write too: a takeover since the last check must
			// stop it. The lookup's answer, no tunnel, still holds.
			return st, false
		}
		if tun, fresh, found = run.create(ctx, api, t); !found {
			return t.unknown(), true
		}
	}
	st.ID, st.Exists = tun.ID, true
	return st, run.converge(ctx, api, t, &st, fresh)
}

// planFault says why the plan of t may not be written, if it may not. The
// tunnel must be one of this install: the shared one, or the one pinned to
// the node the plan names. The sentinel written is what fences the other
// writers, so it must be this writer's, in its place before the catch-all.
func (run *tunnelRun) planFault(t target) string {
	if fault := run.nameFault(t); fault != "" {
		return fault
	}
	n := len(t.rules)
	switch {
	case n < 2 || t.rules[n-1] != planner.CatchAllRule():
		return "the planned rules do not end with the catch-all"
	case t.rules[n-2] != planner.SentinelRule(run.us):
		return "the planned rules lack the sentinel of this writer before the catch-all"
	}
	return ""
}

// nameFault says why the name of t is not one of a tunnel this install can
// have, if it is not.
func (run *tunnelRun) nameFault(t target) string {
	id := run.us.InstallID
	switch t.kind {
	case "", planner.PlacementShared:
		if t.name != planner.TunnelName(id) {
			return "the plan is not for the tunnel of this install, " + planner.TunnelName(id)
		}
	case planner.PlacementPinned:
		want, err := planner.PinnedTunnelName(id, t.node)
		switch {
		case err != nil:
			return "the plan is not for a tunnel this install can have: " + err.Error()
		case t.name != want:
			return fmt.Sprintf("the plan is not for the tunnel of this install pinned to node %s, %s", t.node, want)
		}
	default:
		return fmt.Sprintf("the plan is not for a tunnel this install can have: placement kind %q is not known", t.kind)
	}
	return ""
}

// keepWhy says why a configuration is left as it is rather than written
// over, if it is. A tunnel without a plan is emptied only when this install
// wrote what it holds, and a tunnel pinned to a node is written over only
// when pco wrote what it holds: rules without a sentinel are somebody
// else's. The shared tunnel of this install is written over whatever it
// holds, as that is how a lost store is recovered through it.
func (run *tunnelRun) keepWhy(t target, remote []planner.IngressRule) string {
	switch {
	case !t.planned && !writtenBy(remote, run.us.InstallID):
		return "the configuration holds no sentinel of this install"
	case t.planned && t.pinned() && len(remote) > 0 && !writtenByPCO(remote):
		return "the configuration holds no sentinel, so pco did not write it"
	}
	return ""
}

// writtenBy reports whether rules carry a sentinel of the install.
func writtenBy(rules []planner.IngressRule, installID string) bool {
	return slices.ContainsFunc(rules, func(rule planner.IngressRule) bool {
		w, ok := planner.ParseSentinel(rule.Hostname)
		return ok && w.InstallID == installID
	})
}

// writtenByPCO reports whether rules carry a sentinel of any install.
func writtenByPCO(rules []planner.IngressRule) bool {
	return slices.ContainsFunc(rules, func(rule planner.IngressRule) bool {
		_, ok := planner.ParseSentinel(rule.Hostname)
		return ok
	})
}

// waitRead notes that a read about an account waits for Cloudflare's rate
// limit, once per account.
func (run *tunnelRun) waitRead(account string) {
	read := TunnelRead + account
	if !slices.Contains(run.res.Waiting.Reads, read) {
		run.res.Waiting.Reads = append(run.res.Waiting.Reads, read)
	}
}

// create creates the tunnel of t. found reports whether there is a tunnel
// now, and fresh whether this call created it.
func (run *tunnelRun) create(ctx context.Context, api cfapi.API, t target) (tun cfapi.Tunnel, fresh, found bool) {
	tun, err := api.CreateTunnel(ctx, t.account, t.name)
	switch {
	case err == nil:
		run.act(t, CreateTunnel, createDetail(t), "")
		run.r.log.Debug().Str("account", t.account).Str("tunnel", t.name).Msg("created tunnel")
		return tun, true, true
	case !cfapi.IsConflict(err):
		run.act(t, CreateTunnel, createDetail(t), err.Error())
		run.problem(fmt.Sprintf("%s: creating the tunnel: %v", t, err))
		return cfapi.Tunnel{}, false, false
	}

	// Another writer was quicker, or the answer to an earlier create was lost.
	run.act(t, CreateTunnel, createDetail(t), "tunnel already exists")
	tun, found, err = api.FindTunnel(ctx, t.account, t.name)
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: finding the tunnel after its name was taken: %v", t, err))
	case !found:
		run.problem(fmt.Sprintf("%s: the name is taken, but no tunnel of that name is found", t))
	}
	return tun, false, err == nil && found
}

// converge writes the planned rules to an existing tunnel when its
// configuration differs from them, and reports whether the run goes on. fresh
// says that the run created the tunnel.
func (run *tunnelRun) converge(ctx context.Context, api cfapi.API, t target, st *TunnelState, fresh bool) bool {
	// A write is decided on the configuration read next. Reading the writer
	// just before it lets a takeover since the last check stop the write.
	if run.mode == Enforce && !run.reread(t) {
		return false
	}
	remote, err := api.TunnelConfig(ctx, t.account, st.ID)
	switch {
	case err == nil:
	case fresh && cfapi.IsNotFound(err):
		// A tunnel created moments ago may not have a configuration yet.
		remote = cfapi.TunnelConfig{}
	case cfapi.IsNotFound(err):
		// So may one whose creator stopped before its first write. A single
		// 404 may be transient, though, and taken for no configuration it
		// would write over whatever sentinel the tunnel holds.
		var read, goOn bool
		if remote, read, goOn = run.readAgain(ctx, api, t, st.ID, err); !read {
			return goOn
		}
	default:
		run.problem(fmt.Sprintf("%s: reading the configuration: %v", t, err))
		return true
	}
	st.Version = remote.Version
	if run.keep != nil && run.wrote(remote.Ingress) {
		t.rules, st.Kept = keepServed(t.rules, remote.Ingress, run.keep)
	}
	if EqualIngress(remote.Ingress, t.rules) && !remote.Foreign {
		st.Verified = true
		return true
	}
	if !t.planned && len(remote.Ingress) == 0 {
		// A configuration without rules serves nothing, which is all a
		// tunnel without a plan is to do.
		st.Verified = true
		return true
	}

	detail := putDetail(t, remote)
	if why := run.keepWhy(t, remote.Ingress); why != "" {
		run.act(t, PutConfig, detail, why)
		run.problem(fmt.Sprintf("%s: %s; left as it is", t, why))
		return true
	}
	// Every answer the run went on with named us as the stored writer.
	if v, _ := judgeConfig(run.us, run.us, remote.Ingress); v != WriterProceed {
		run.refuse(t, detail, remote.Ingress)
		return false
	}
	if run.mode == Observe {
		run.act(t, PutConfig, detail, run.held)
		return true
	}
	key := t.account + "/" + st.ID
	if wait := run.r.putWait(key); wait > 0 {
		run.act(t, PutConfig, detail, fmt.Sprintf("rate limit: next write in %s", wait))
		return true
	}
	// Counted before the call: a write whose answer is lost may have landed.
	run.r.lastPut[key] = run.r.now()
	run.put(ctx, api, t, st, detail)
	return true
}

// readAgain answers a configuration read of the tunnel of t, found before,
// that answered notFound. When the lookup still finds the tunnel of that
// name under the same id, the configuration is read once more: only a second
// 404 means that the tunnel has none yet, and any configuration it returns is
// judged like any other. read reports whether remote is that answer;
// otherwise there is a problem. goOn says whether the run goes on.
func (run *tunnelRun) readAgain(ctx context.Context, api cfapi.API, t target, id string, notFound error) (remote cfapi.TunnelConfig, read, goOn bool) {
	tun, found, err := api.FindTunnel(ctx, t.account, t.name)
	if err != nil || !found || tun.ID != id {
		msg := fmt.Sprintf("%s: reading the configuration: %v", t, notFound)
		if err != nil {
			msg += fmt.Sprintf("; looking it up again: %v", err)
		}
		run.problem(msg)
		return cfapi.TunnelConfig{}, false, true
	}
	// The write is decided on this read, so the writer is read just before.
	if run.mode == Enforce && !run.reread(t) {
		return cfapi.TunnelConfig{}, false, false
	}
	remote, err = api.TunnelConfig(ctx, t.account, id)
	switch {
	case err == nil:
		return remote, true, true
	case cfapi.IsNotFound(err):
		return cfapi.TunnelConfig{}, true, true
	}
	run.problem(fmt.Sprintf("%s: reading the configuration again: %v", t, err))
	return cfapi.TunnelConfig{}, false, true
}

// refuse stops the run on remote rules whose sentinels do not let this writer
// proceed. A takeover during the run shows as a sentinel newer than ours, so
// the verdict is taken again against leader.json as it is now: a newer writer
// that leader.json names makes this one stale, while a sentinel it does not
// explain stays foreign. When leader.json cannot be read, only a sentinel
// that is foreign whatever it holds makes the verdict foreign; a newer one is
// taken for a writer of this install.
func (run *tunnelRun) refuse(t target, detail string, remote []planner.IngressRule) {
	us, stored, err := run.r.writer()
	var v WriterVerdict
	var why string
	var by planner.Writer
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: reading the writer identity: %v", t, err))
		v, by, why = judgeUnread(run.us, remote, run.hint)
	case us != run.us:
		v, why = WriterStale, writerFault(run.us, us, stored)
	default:
		// The table answers proceed from the sentinels and us alone, and
		// those did not let this writer proceed: the verdict stops the run
		// either way.
		v, by = judgeConfig(run.us, stored, remote)
		why = foreignWhy(run.us, by, run.hint)
		if v == WriterStale {
			why = writerFault(run.us, run.us, stored)
		}
	}
	run.act(t, PutConfig, detail, heldVerdict(v))
	run.stop(t.String()+": ", v, why)
	run.res.Sentinel = by
}

// judgeUnread is the verdict on remote rules that do not let us proceed, when
// leader.json cannot be read, with the writer of the sentinel it rests on. A
// sentinel of our generation with another nonce is foreign whatever
// leader.json holds; a newer one leader.json may name, so the writer is taken
// to be stale.
func judgeUnread(us planner.Writer, remote []planner.IngressRule, hint string) (WriterVerdict, planner.Writer, string) {
	var newer planner.Writer
	for _, rule := range remote {
		w, ok := planner.ParseSentinel(rule.Hostname)
		switch {
		case !ok || JudgeWriter(us, w, true, us) == WriterProceed:
		case JudgeWriter(us, w, true, w) == WriterForeign:
			// Foreign even with leader.json naming that very writer.
			return WriterForeign, w, foreignWhy(us, w, hint)
		case newer == (planner.Writer{}):
			newer = w
		}
	}
	return WriterStale, newer, fmt.Sprintf("the configuration was written by generation %d nonce %s, newer than this writer",
		newer.Generation, newer.Nonce)
}

// foreignWhy says why a configuration by a writer leader.json does not know
// stops the run, and what to do about it: hint, or the host's words when it
// is empty.
func foreignWhy(us, by planner.Writer, hint string) string {
	if hint == "" {
		hint = fmt.Sprintf("another installation uses install id %s, or the store was lost (pco setup --recover)", us.InstallID)
	}
	return fmt.Sprintf("the configuration was written by generation %d nonce %s, which leader.json does not know: %s",
		by.Generation, by.Nonce, hint)
}

// putWait returns how long the configuration of a tunnel must wait before it
// may be written again.
func (r *TunnelReconciler) putWait(key string) time.Duration {
	last, ok := r.lastPut[key]
	if !ok {
		return 0
	}
	now := r.now()
	if now.Before(last) {
		// The clock stepped back. Counting from now holds the write for one
		// interval; counting from last would hold it until the clock caught up.
		r.lastPut[key] = now
		last = now
	}
	return last.Add(putInterval).Sub(now)
}

// put writes the planned rules and reads them back. Whatever the outcome, it
// is not tried again in this run.
func (run *tunnelRun) put(ctx context.Context, api cfapi.API, t target, st *TunnelState, detail string) {
	version, err := api.PutTunnelConfig(ctx, t.account, st.ID, t.rules)
	if err != nil {
		run.act(t, PutConfig, detail, err.Error())
		run.problem(fmt.Sprintf("%s: writing the configuration: %v", t, err))
		return
	}
	run.act(t, PutConfig, detail, "")
	st.Version = version
	run.r.log.Debug().Str("account", t.account).Str("tunnel", t.name).Int("version", version).Msg("wrote tunnel configuration")

	got, err := api.TunnelConfig(ctx, t.account, st.ID)
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: reading the configuration back: %v", t, err))
	case !EqualIngress(got.Ingress, t.rules) || got.Foreign:
		run.problem(fmt.Sprintf("config changed under us on %s", t))
	default:
		st.Version, st.Verified = got.Version, true
	}
}

// act records an action; it was applied when held is empty.
func (run *tunnelRun) act(t target, kind ActionKind, detail, held string) {
	run.res.Actions = append(run.res.Actions, Action{
		Kind:       kind,
		Credential: t.credential,
		AccountID:  t.account,
		Target:     t.name,
		Detail:     detail,
		Applied:    held == "",
		Held:       held,
	})
}

func (run *tunnelRun) problem(msg string) {
	run.r.log.Warn().Msg(msg)
	run.res.Problems = append(run.res.Problems, msg)
}

func createDetail(t target) string { return "in account " + t.account }

func putDetail(t target, remote cfapi.TunnelConfig) string {
	d := fmt.Sprintf("in account %s: %d rules", t.account, len(t.rules))
	switch {
	case remote.Foreign:
		return fmt.Sprintf("%s replace version %d, which holds settings pco does not manage", d, remote.Version)
	case len(remote.Ingress) == 0:
		return d + ", first configuration"
	}
	return fmt.Sprintf("%s replace version %d", d, remote.Version)
}

func heldVerdict(v WriterVerdict) string {
	if v == WriterStale {
		return "stale writer"
	}
	return "foreign writer"
}
