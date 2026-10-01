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

// TunnelReconciler brings the tunnel of each account in line with its plan.
// It remembers when it last wrote the configuration of each tunnel, so one
// instance should serve a process for its whole life. Calls to Run are
// serialised.
type TunnelReconciler struct {
	clients Clients
	writer  func() (us, stored planner.Writer, err error)
	now     func() time.Time
	log     zerolog.Logger

	mu      sync.Mutex
	lastPut map[string]time.Time // by account id and tunnel id
}

// NewTunnelReconciler returns a reconciler that reaches each account through
// the client of its credential.
//
// writer returns the identity this process writes as (us) and leader.json as
// stored now (stored). Run asks it before its first call to Cloudflare; in
// Enforce mode again before each create, before it reads the configuration of
// each tunnel, before it looks a tunnel up again whose configuration was not
// found, and before it deletes a probe tunnel; and once more before it stops
// on the sentinel of another writer. It should fail whenever this process may
// not write at all, as when its lease is lost.
func NewTunnelReconciler(clients Clients, writer func() (us, stored planner.Writer, err error), now func() time.Time, log zerolog.Logger) *TunnelReconciler {
	return &TunnelReconciler{
		clients: clients,
		writer:  writer,
		now:     now,
		log:     log,
		lastPut: make(map[string]time.Time),
	}
}

// TunnelResult is what a run found and did.
type TunnelResult struct {
	Tunnels  []TunnelState // one per planned or known account, by account id
	Actions  []Action      // in the order performed or planned
	Problems []string
	Verdict  WriterVerdict // WriterProceed unless another writer stopped the run
}

// Run brings tunnels in line with plans. Accounts listed in `known` (account
// id -> credential id) without a plan get an empty rule set when their tunnel exists.
// As such an account is emptied, known must hold only accounts for which the
// caller has a complete, current plan.
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
// without connectors. It deletes no other tunnel.
func (r *TunnelReconciler) Run(ctx context.Context, plans []planner.TunnelPlan, known map[string]string, mode Mode) TunnelResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	run := &tunnelRun{r: r, mode: mode}
	goOn := run.start()
	for _, t := range targets(plans, known, run.us) {
		if goOn && ctx.Err() != nil {
			run.problem(fmt.Sprintf("run stopped: %v", ctx.Err()))
			goOn = false
		}
		if !goOn {
			run.res.Tunnels = append(run.res.Tunnels, t.unknown())
			continue
		}
		var st TunnelState
		st, goOn = run.reconcile(ctx, t)
		run.res.Tunnels = append(run.res.Tunnels, st)
	}
	if goOn && mode == Enforce {
		run.sweepProbeTunnels(ctx)
	}
	return run.res
}

// target is the tunnel of one account and the rules it should have.
type target struct {
	account    string
	credential string
	name       string
	rules      []planner.IngressRule
	planned    bool // false for an account without a plan: its tunnel is emptied, never created
}

func (t target) String() string { return t.name + " in account " + t.account }

// unknown is the state of a tunnel that the run knows nothing about.
func (t target) unknown() TunnelState {
	return TunnelState{AccountID: t.account, CredentialID: t.credential, Name: t.name, Unknown: true}
}

// targets lists the tunnels to look at, by account id: one per plan, and one
// that serves nothing for each known account without a plan. Without a writer
// identity the name of the latter is not known and left empty.
func targets(plans []planner.TunnelPlan, known map[string]string, us planner.Writer) []target {
	out := make([]target, 0, len(plans)+len(known))
	planned := make(map[string]bool, len(plans))
	for _, p := range plans {
		out = append(out, target{account: p.AccountID, credential: p.CredentialID, name: p.Name, rules: p.Rules, planned: true})
		planned[p.AccountID] = true
	}
	var name string
	if us.InstallID != "" {
		name = planner.TunnelName(us.InstallID)
	}
	for account, credential := range known {
		if !planned[account] {
			out = append(out, target{account: account, credential: credential, name: name, rules: emptyRules(us)})
		}
	}
	slices.SortStableFunc(out, func(a, b target) int { return cmp.Compare(a.account, b.account) })
	return out
}

// emptyRules is the ingress of a tunnel that serves nothing.
func emptyRules(us planner.Writer) []planner.IngressRule {
	return []planner.IngressRule{planner.SentinelRule(us), planner.CatchAllRule()}
}

// tunnelRun is the state of one Run.
type tunnelRun struct {
	r    *TunnelReconciler
	mode Mode
	us   planner.Writer // set once start has validated it
	res  TunnelResult
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

	tun, found, err := api.FindTunnel(ctx, t.account, t.name)
	if err != nil {
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
			run.act(t, CreateTunnel, createDetail(t), heldObserve)
			run.act(t, PutConfig, putDetail(t, cfapi.TunnelConfig{}), heldObserve)
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
// sentinel written is what fences the other writers, so it must be this
// writer's, in its place before the catch-all.
func (run *tunnelRun) planFault(t target) string {
	n := len(t.rules)
	switch {
	case t.name != planner.TunnelName(run.us.InstallID):
		return "the plan is not for the tunnel of this install, " + planner.TunnelName(run.us.InstallID)
	case n < 2 || t.rules[n-1] != planner.CatchAllRule():
		return "the planned rules do not end with the catch-all"
	case t.rules[n-2] != planner.SentinelRule(run.us):
		return "the planned rules lack the sentinel of this writer before the catch-all"
	}
	return ""
}

// create creates the tunnel of t. found reports whether there is a tunnel
// now, and fresh whether this call created it.
func (run *tunnelRun) create(ctx context.Context, api cfapi.API, t target) (tun cfapi.Tunnel, fresh, found bool) {
	tun, err := api.CreateTunnel(ctx, t.account, t.name)
	switch {
	case err == nil:
		run.act(t, CreateTunnel, createDetail(t), "")
		run.r.log.Info().Str("account", t.account).Str("tunnel", t.name).Msg("created tunnel")
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
		// So may one whose creator stopped before its first write, as long
		// as it is still the tunnel of that name.
		still, goOn := run.stillThere(ctx, api, t, st.ID, err)
		if !still {
			return goOn
		}
		remote = cfapi.TunnelConfig{}
	default:
		run.problem(fmt.Sprintf("%s: reading the configuration: %v", t, err))
		return true
	}
	st.Version = remote.Version
	if EqualIngress(remote.Ingress, t.rules) && !remote.Foreign {
		st.Verified = true
		return true
	}

	detail := putDetail(t, remote)
	// Every answer the run went on with named us as the stored writer.
	if v, _ := judgeConfig(run.us, run.us, remote.Ingress); v != WriterProceed {
		run.refuse(t, detail, remote.Ingress)
		return false
	}
	if run.mode == Observe {
		run.act(t, PutConfig, detail, heldObserve)
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

// stillThere looks up again the tunnel of t, found before, whose
// configuration read answered notFound. still reports whether it is the
// tunnel of that name under the same id, which then has no configuration
// yet; otherwise the read is a problem. goOn says whether the run goes on.
func (run *tunnelRun) stillThere(ctx context.Context, api cfapi.API, t target, id string, notFound error) (still, goOn bool) {
	// The answer decides a write, so the writer is read just before it.
	if run.mode == Enforce && !run.reread(t) {
		return false, false
	}
	tun, found, err := api.FindTunnel(ctx, t.account, t.name)
	if err == nil && found && tun.ID == id {
		return true, true
	}
	msg := fmt.Sprintf("%s: reading the configuration: %v", t, notFound)
	if err != nil {
		msg += fmt.Sprintf("; looking it up again: %v", err)
	}
	run.problem(msg)
	return false, true
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
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: reading the writer identity: %v", t, err))
		v, why = judgeUnread(run.us, remote)
	case us != run.us:
		v, why = WriterStale, writerFault(run.us, us, stored)
	default:
		// The table answers proceed from the sentinels and us alone, and
		// those did not let this writer proceed: the verdict stops the run
		// either way.
		var by planner.Writer
		v, by = judgeConfig(run.us, stored, remote)
		why = foreignWhy(run.us, by)
		if v == WriterStale {
			why = writerFault(run.us, run.us, stored)
		}
	}
	run.act(t, PutConfig, detail, heldVerdict(v))
	run.stop(t.String()+": ", v, why)
}

// judgeUnread is the verdict on remote rules that do not let us proceed, when
// leader.json cannot be read. A sentinel of our generation with another nonce
// is foreign whatever leader.json holds; a newer one leader.json may name, so
// the writer is taken to be stale.
func judgeUnread(us planner.Writer, remote []planner.IngressRule) (WriterVerdict, string) {
	var newer planner.Writer
	for _, rule := range remote {
		w, ok := planner.ParseSentinel(rule.Hostname)
		switch {
		case !ok || JudgeWriter(us, w, true, us) == WriterProceed:
		case JudgeWriter(us, w, true, w) == WriterForeign:
			// Foreign even with leader.json naming that very writer.
			return WriterForeign, foreignWhy(us, w)
		case newer == (planner.Writer{}):
			newer = w
		}
	}
	return WriterStale, fmt.Sprintf("the configuration was written by generation %d nonce %s, newer than this writer",
		newer.Generation, newer.Nonce)
}

func foreignWhy(us, by planner.Writer) string {
	return fmt.Sprintf("the configuration was written by generation %d nonce %s, which leader.json does not know: "+
		"another installation uses install id %s, or the store was lost (pco setup --recover)",
		by.Generation, by.Nonce, us.InstallID)
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
	run.r.log.Info().Str("account", t.account).Str("tunnel", t.name).Int("version", version).Msg("wrote tunnel configuration")

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
