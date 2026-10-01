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

	notFoundService = "http_status:404"
	heldObserve     = "observe mode"
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
// the client of its credential. writer returns the identity this process
// writes as (us) and leader.json as stored now (stored). Run asks it before
// its first call to Cloudflare and again right before each configuration
// write, and stops when it fails; it should fail whenever this process may
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
	// Tunnels are the tunnels looked at, by account id. A tunnel whose lookup
	// failed, or that was to be created and was not, is left out: whether it
	// exists is not known.
	Tunnels  []TunnelState
	Actions  []Action // in the order performed or planned
	Problems []string
	Verdict  WriterVerdict // WriterProceed unless a sentinel stopped the run
}

// Run brings tunnels in line with plans. Accounts listed in `known` (account
// id -> credential id) without a plan get an empty rule set when their tunnel exists.
//
// A configuration is written only where it differs from the plan or holds
// settings pco does not manage, and only when the sentinels in it let this
// writer proceed; a stale or foreign verdict stops the run at once. A failure
// on one tunnel is a problem and the run goes on with the next. In Observe
// mode Run only reads, and returns the actions it would take as held.
func (r *TunnelReconciler) Run(ctx context.Context, plans []planner.TunnelPlan, known map[string]string, mode Mode) TunnelResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	run := &tunnelRun{r: r, mode: mode}
	us, stored, err := r.writer()
	if err != nil {
		run.problem(fmt.Sprintf("reading the writer identity: %v", err))
		return run.res
	}
	if err := us.Validate(); err != nil {
		run.problem(fmt.Sprintf("cannot write as this writer: %v", err))
		return run.res
	}
	run.us, run.stored = us, stored
	for _, t := range targets(plans, known, us) {
		if !run.reconcile(ctx, t) {
			break
		}
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

// targets lists the tunnels to look at, by account id: one per plan, and one
// that serves nothing for each known account without a plan.
func targets(plans []planner.TunnelPlan, known map[string]string, us planner.Writer) []target {
	out := make([]target, 0, len(plans)+len(known))
	planned := make(map[string]bool, len(plans))
	for _, p := range plans {
		out = append(out, target{account: p.AccountID, credential: p.CredentialID, name: p.Name, rules: p.Rules, planned: true})
		planned[p.AccountID] = true
	}
	for account, credential := range known {
		if !planned[account] {
			out = append(out, target{account: account, credential: credential, name: planner.TunnelName(us.InstallID), rules: emptyRules(us)})
		}
	}
	slices.SortStableFunc(out, func(a, b target) int { return cmp.Compare(a.account, b.account) })
	return out
}

// emptyRules is the ingress of a tunnel that serves nothing.
func emptyRules(us planner.Writer) []planner.IngressRule {
	return []planner.IngressRule{
		{Hostname: planner.SentinelHostname(us), Service: notFoundService},
		{Service: notFoundService},
	}
}

// tunnelRun is the state of one Run.
type tunnelRun struct {
	r      *TunnelReconciler
	mode   Mode
	us     planner.Writer
	stored planner.Writer // as last read
	res    TunnelResult
}

// reconcile brings the tunnel of one account in line and reports whether the
// run goes on.
func (run *tunnelRun) reconcile(ctx context.Context, t target) bool {
	api := run.r.clients[t.credential]
	switch {
	case api == nil:
		run.problem(fmt.Sprintf("%s: no client for credential %s", t, t.credential))
		return true
	case !hasSentinel(t.rules, run.us):
		// Other writers are fenced by the sentinel written here, so it must
		// be this writer's.
		run.problem(fmt.Sprintf("%s: the planned rules lack the sentinel of this writer", t))
		return true
	}

	tun, found, err := api.FindTunnel(ctx, t.account, t.name)
	if err != nil {
		run.problem(fmt.Sprintf("%s: finding the tunnel: %v", t, err))
		return true
	}
	st := TunnelState{AccountID: t.account, CredentialID: t.credential, Name: t.name}
	if !found {
		switch {
		case !t.planned:
			run.res.Tunnels = append(run.res.Tunnels, st)
			return true
		case run.mode == Observe:
			run.res.Tunnels = append(run.res.Tunnels, st)
			run.act(t, CreateTunnel, createDetail(t), heldObserve)
			run.act(t, PutConfig, putDetail(t, cfapi.TunnelConfig{}), heldObserve)
			return true
		}
		if tun, found = run.create(ctx, api, t); !found {
			return true
		}
	}
	st.ID, st.Exists = tun.ID, true
	goOn := run.converge(ctx, api, t, &st)
	run.res.Tunnels = append(run.res.Tunnels, st)
	return goOn
}

func hasSentinel(rules []planner.IngressRule, us planner.Writer) bool {
	host := planner.SentinelHostname(us)
	return slices.ContainsFunc(rules, func(r planner.IngressRule) bool { return r.Hostname == host })
}

// create creates the tunnel of t and reports whether there is one now.
func (run *tunnelRun) create(ctx context.Context, api cfapi.API, t target) (cfapi.Tunnel, bool) {
	tun, err := api.CreateTunnel(ctx, t.account, t.name)
	switch {
	case err == nil:
		run.act(t, CreateTunnel, createDetail(t), "")
		run.r.log.Info().Str("account", t.account).Str("tunnel", t.name).Msg("created tunnel")
		return tun, true
	case !cfapi.IsConflict(err):
		run.act(t, CreateTunnel, createDetail(t), err.Error())
		run.problem(fmt.Sprintf("%s: creating the tunnel: %v", t, err))
		return cfapi.Tunnel{}, false
	}

	// Another writer was quicker, or the answer to an earlier create was lost.
	run.act(t, CreateTunnel, createDetail(t), "tunnel already exists")
	tun, found, err := api.FindTunnel(ctx, t.account, t.name)
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: finding the tunnel after its name was taken: %v", t, err))
	case !found:
		run.problem(fmt.Sprintf("%s: the name is taken, but no tunnel of that name is found", t))
	}
	return tun, err == nil && found
}

// converge writes the planned rules to an existing tunnel when its
// configuration differs from them, and reports whether the run goes on.
func (run *tunnelRun) converge(ctx context.Context, api cfapi.API, t target, st *TunnelState) bool {
	remote, err := api.TunnelConfig(ctx, t.account, st.ID)
	if err != nil {
		run.problem(fmt.Sprintf("%s: reading the configuration: %v", t, err))
		return true
	}
	st.Version = remote.Version
	if EqualIngress(remote.Ingress, t.rules) && !remote.Foreign {
		return true
	}

	detail := putDetail(t, remote)
	key := t.account + "/" + st.ID
	wait := run.r.putWait(key)
	due := run.mode == Enforce && wait <= 0
	if due && !run.rereadWriter(t) {
		return false
	}
	if v, by := judgeConfig(run.us, run.stored, remote.Ingress); v != WriterProceed {
		run.act(t, PutConfig, detail, heldVerdict(v))
		run.problem(verdictProblem(t, v, by, run.us))
		run.res.Verdict = v
		return false
	}
	switch {
	case run.mode == Observe:
		run.act(t, PutConfig, detail, heldObserve)
	case !due:
		run.act(t, PutConfig, detail, fmt.Sprintf("rate limit: next write in %s", wait))
	default:
		// Counted before the call: a write whose answer is lost may have landed.
		run.r.lastPut[key] = run.r.now()
		run.put(ctx, api, t, st, detail)
	}
	return true
}

// putWait returns how long the configuration of a tunnel must wait before it
// may be written again.
func (r *TunnelReconciler) putWait(key string) time.Duration {
	last, ok := r.lastPut[key]
	if !ok {
		return 0
	}
	return last.Add(putInterval).Sub(r.now())
}

// rereadWriter asks for the writer identity right before a write and reports
// whether the write may go on to be judged.
func (run *tunnelRun) rereadWriter(t target) bool {
	us, stored, err := run.r.writer()
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: reading the writer identity before the write: %v", t, err))
		return false
	case us != run.us:
		// The plan carries the sentinel of the identity the run started with.
		run.problem(fmt.Sprintf("%s: the writer identity changed during the run; not writing", t))
		return false
	}
	run.stored = stored
	return true
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
	run.r.log.Info().Str("account", t.account).Str("tunnel", t.name).Int("version", version).Msg("wrote tunnel configuration")

	got, err := api.TunnelConfig(ctx, t.account, st.ID)
	if err != nil {
		run.problem(fmt.Sprintf("%s: reading the configuration back: %v", t, err))
		return
	}
	st.Version = got.Version
	if !EqualIngress(got.Ingress, t.rules) || got.Foreign {
		run.problem(fmt.Sprintf("config changed under us on %s", t))
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

// verdictProblem says why the sentinel of writer by stops this writer.
func verdictProblem(t target, v WriterVerdict, by, us planner.Writer) string {
	if v == WriterStale {
		return fmt.Sprintf("%s: written by generation %d, newer than ours (%d): this writer is stale and stops", t, by.Generation, us.Generation)
	}
	return fmt.Sprintf("%s: written by generation %d with nonce %s, which leader.json does not know: "+
		"another installation uses install id %s, or the store was lost (pco setup --recover); writing stops",
		t, by.Generation, by.Nonce, us.InstallID)
}
