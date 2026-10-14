package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// holderWait is how long a promotion waits for this node to hold the
	// lease, which the keeper takes at its next poll.
	holderWait = 20 * time.Second
	// promoteReads bounds the reads of the sentinels at Cloudflare, and
	// promoteWait a whole promotion: both inside the minute the command
	// line waits for the answer.
	promoteReads = 45 * time.Second
	promoteWait  = 55 * time.Second
)

// PromoteRequest is what pco promote asks: without Confirm only the
// differences; with it a new epoch, which Force draws also when nothing
// differs, and Cluster names the cluster a store of another cluster was made
// in, as the differences show it: its name, or its fingerprint. Shown are
// the differences the admin was shown: a confirmed promotion reads them
// again and refuses when they changed since.
type PromoteRequest struct {
	Confirm bool             `json:"confirm,omitempty"`
	Force   bool             `json:"force,omitempty"`
	Cluster string           `json:"cluster,omitempty"`
	Shown   []DifferenceView `json:"shown,omitempty"`
}

// Promote makes this node the writer of a store the controller only
// observes: one restored from an earlier copy, behind what the agents
// remember, or made in another cluster. Without Confirm it only says what
// differs between the store and what the agents and Cloudflare say. With
// it, once this node holds the lease, the store is made this cluster's and a
// new epoch is drawn above every sentinel of the install at Cloudflare and
// every epoch an agent reported; the settings are set to observe-only, so
// that what the controller would change is shown before pco apply lets it.
// It refuses when there is nothing to promote, unless Force. On the host
// only: the appliance recovers with pco appliance recover.
func (e *Engine) Promote(ctx context.Context, req PromoteRequest) (PromoteResult, error) {
	if e.d.Lease == nil {
		return PromoteResult{}, fmt.Errorf("%w: pco promote is for the host profile; an appliance draws a new epoch with pco appliance recover", ErrRefused)
	}
	ctx, cancel := e.timeout(ctx, promoteWait)
	defer cancel()
	inst, found, err := e.d.Store.Install()
	switch {
	case err != nil:
		return PromoteResult{}, fmt.Errorf("reading the install identity: %w", err)
	case !found:
		return PromoteResult{}, fmt.Errorf("%w: %s", ErrRefused, problemNotSetUp)
	}
	if err := e.takenOverHere(inst); err != nil {
		return PromoteResult{}, err
	}
	if !req.Confirm {
		// Nothing is changed: the reads at Cloudflare need no cycle lock.
		return PromoteResult{Differences: e.differences(ctx, inst, e.readSentinels(ctx, inst.ID))}, nil
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return PromoteResult{}, err
	}
	defer e.release()
	// Read again under the lock: the install saved below is this one.
	switch inst, found, err = e.d.Store.Install(); {
	case err != nil:
		return PromoteResult{}, fmt.Errorf("reading the install identity again: %w", err)
	case !found:
		return PromoteResult{}, fmt.Errorf("%w: %s", ErrRefused, problemNotSetUp)
	}
	if err := e.takenOverHere(inst); err != nil {
		return PromoteResult{}, err
	}

	res := PromoteResult{Differences: req.Shown}
	other, err := e.otherCluster(inst)
	if err != nil {
		return res, err
	}
	if err := e.nameOfOther(other, req); err != nil {
		return res, err
	}
	if err := e.awaitHolder(ctx); err != nil {
		return res, err
	}
	// Everything is read again now that the admin confirmed: leader.json,
	// the registry, the agents, the sentinels. A promotion goes on only on
	// what was shown, and Force only lets one through that has nothing to do.
	sent := e.readSentinels(ctx, inst.ID)
	if sent.err != nil {
		return res, fmt.Errorf("%w: the generation at Cloudflare cannot be read: %w", ErrRefused, sent.err)
	}
	res.Differences = e.differences(ctx, inst, sent)
	if !sameDifferences(req.Shown, res.Differences) {
		return res, fmt.Errorf("%w: what differs between the store, the agents and Cloudflare changed since it was shown; "+
			"run pco promote again to see it", ErrRefused)
	}
	if err := e.somethingToDo(res.Differences, other, req); err != nil {
		return res, err
	}
	if inst.Restored != nil && other != nil {
		if err := e.oldClusterStopped(ctx, inst.ID); err != nil {
			return res, err
		}
	}
	seed := e.promotionSeed(ctx, inst.ID, sent.highest)
	// A store that went back takes no write until a tenure begins anew: the
	// admin adopts it as it is. The engine holds on its own until the new
	// epoch is drawn.
	e.d.Store.StartTenure()
	claimedFrom := ""
	if inst.Restored != nil {
		claimedFrom = inst.Restored.ClaimedFrom
		// Copies of the run tokens may be left on the disks of the install
		// that was lost: every tunnel gets a new secret once pco may write.
		// Asked for before the restore ends, so that a promotion cut short
		// and done again asks once more rather than never.
		if err := e.d.Store.SaveRestoreRotation(store.RestoreRotation{Since: e.d.Now()}); err != nil {
			return res, fmt.Errorf("asking for new secrets of the tunnels: %w", err)
		}
		res.RotatesSecrets, e.restoreUnsaved = true, nil
	}
	if other != nil || inst.Restored != nil {
		// A store restored from a bundle is this cluster's from now on: the
		// other nodes join it as they join any install.
		if other != nil {
			inst.Cluster = other
		}
		inst.Restored = nil
		if err := e.d.Store.SaveInstall(inst); err != nil {
			return res, fmt.Errorf("making this cluster the store's: %w", err)
		}
	}
	if err := e.d.Lease.Promote(ctx, seed); err != nil {
		return res, fmt.Errorf("drawing a new epoch: %w", err)
	}
	if err := e.observeOnly(); err != nil {
		return res, err
	}
	st := e.d.Lease.State()
	e.d.Store.StartTenure()
	e.tenure, e.tenureAt, e.tenureWrote, e.wentBack = st.Writer, e.d.Now(), false, nil
	res.Drawn, res.Generation, res.Nonce = true, st.Writer.Generation, st.Writer.Nonce
	e.above.clear()
	msg := fmt.Sprintf("promoted to generation %d; observe-only until pco apply", st.Writer.Generation)
	e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: kindLease, Subject: e.d.Node, Message: msg, Actor: ActorOf(ctx)})
	e.adminEvent(ctx, e.d.Node, msg)
	if claimedFrom != "" {
		e.events.add(Event{At: e.d.Now(), Level: levelWarn, Kind: KindSecurity, Subject: e.d.Node, Node: e.d.Node, Security: true,
			Actor: ActorOf(ctx), Message: fmt.Sprintf("node %s took the restored store into this cluster over the claim of node %s, "+
				"which was not an online member of it", e.d.Node, claimedFrom)})
	}
	e.Trigger()
	return res, nil
}

// nameOfOther refuses a promotion of a store of another cluster unless the
// admin typed the name, or the fingerprint, of the cluster it was made in.
func (e *Engine) nameOfOther(other *store.ClusterIdentity, req PromoteRequest) error {
	if other == nil {
		return nil
	}
	inst, _, err := e.d.Store.Install()
	if err != nil {
		return fmt.Errorf("reading the install identity: %w", err)
	}
	rec := inst.Cluster
	if req.Cluster == "" || (req.Cluster != rec.Name && req.Cluster != rec.CASHA256) {
		return fmt.Errorf("%w: the store was made in cluster %s (%s); type its name, or its fingerprint when it has none, "+
			"to make this cluster its writer", ErrRefused, clusterName(*rec), rec.CASHA256)
	}
	return nil
}

// somethingToDo refuses a promotion that has nothing to do, unless forced:
// the store and the agents agree, the controller is not behind, the store did
// not go back and was made here.
func (e *Engine) somethingToDo(diffs []DifferenceView, other *store.ClusterIdentity, req PromoteRequest) error {
	if other != nil {
		return nil
	}
	st := e.d.Lease.State()
	if req.Force || len(diffs) > 0 || st.Behind != "" || e.storeWentBack() != nil {
		return nil
	}
	return fmt.Errorf("%w: nothing differs between the store and the agents, and this controller is not behind: "+
		"there is nothing to promote; --force draws a new epoch all the same", ErrRefused)
}

// otherCluster is the identity of the cluster this node is in when the store
// was made in another one; nil when it was made here, or records none.
func (e *Engine) otherCluster(inst store.Install) (*store.ClusterIdentity, error) {
	read := e.d.Cluster
	if read == nil || inst.Cluster == nil {
		return nil, nil
	}
	here, err := read()
	if err != nil {
		return nil, fmt.Errorf("reading the identity of this cluster: %w", err)
	}
	if inst.Cluster.CASHA256 == here.CASHA256 {
		return nil, nil
	}
	return &here, nil
}

// takenOverHere refuses the promotion of a store restored into another
// cluster unless pco setup took it into this one in full, on this node:
// until then its registry and its CA are those of the lost cluster, and
// another node that took it promotes it there.
func (e *Engine) takenOverHere(inst store.Install) error {
	if inst.Restored == nil {
		return nil
	}
	other, err := e.otherCluster(inst)
	if err != nil {
		return err
	}
	if other == nil {
		return nil
	}
	if by := inst.Restored.TakenOverBy; by != "" && by != e.d.Node {
		return fmt.Errorf("%w: node %s took the restored store into this cluster: run pco promote there", ErrRefused, by)
	}
	done, why, err := e.d.Store.TakenOver(inst)
	switch {
	case err != nil:
		return fmt.Errorf("%w: how far the restored store was taken into this cluster cannot be read: %w", ErrRefused, err)
	case !done:
		return fmt.Errorf("%w: the restored store is not taken into this cluster: %s; run pco setup on this node first", ErrRefused, why)
	}
	return nil
}

// oldClusterStopped refuses the promotion of a store restored into another
// cluster while a connector of the install is connected at Cloudflare:
// nothing of this cluster runs one before the promotion, so the cluster the
// bundle was made in, or a copy of it, still runs the install. Every stored
// credential looks; one that cannot refuses too.
func (e *Engine) oldClusterStopped(ctx context.Context, installID string) error {
	ctx, cancel := e.timeout(ctx, promoteReads)
	defer cancel()
	creds, err := e.d.Store.Credentials()
	if err != nil {
		return fmt.Errorf("reading the credentials: %w", err)
	}
	if len(creds) == 0 {
		return fmt.Errorf("%w: cannot tell whether the cluster the bundle was made in still runs install %s: no Cloudflare credential is stored",
			ErrRefused, installID)
	}
	var live []string
	for _, cred := range creds {
		api, err := e.d.NewClient(cred)
		if err == nil {
			var found []string
			found, err = cfapi.Connected(ctx, api, installID)
			live = append(live, found...)
		}
		if err != nil {
			return fmt.Errorf("%w: cannot tell whether the cluster the bundle was made in still runs install %s: credential %s: %w",
				ErrRefused, installID, cred.ID, err)
		}
	}
	if len(live) > 0 {
		slices.Sort(live)
		return fmt.Errorf("%w: the cluster the bundle was made in still runs install %s, or a copy of it does: at Cloudflare %s; "+
			"stop pco and its connectors there (systemctl stop pco 'pco-cloudflared@*'), and promote once Cloudflare shows no connection",
			ErrRefused, installID, strings.Join(slices.Compact(live), "; "))
	}
	return nil
}

// sentinelRead is what a read of the sentinels at Cloudflare found: the
// highest generation, or why it could not be read; skipped when this node
// does not hold the lease, as only the controller reads Cloudflare.
type sentinelRead struct {
	highest int
	err     error
	skipped bool
}

// readSentinels reads the sentinels of the install at Cloudflare, when this
// node holds the lease.
func (e *Engine) readSentinels(ctx context.Context, installID string) sentinelRead {
	if !e.d.Lease.State().Holder {
		return sentinelRead{skipped: true}
	}
	highest, err := e.highestSentinel(ctx, installID)
	return sentinelRead{highest: highest, err: err}
}

// sameDifferences says whether what was shown is what differs now. A line
// of the sentinels that was to be read once confirmed says nothing yet.
func sameDifferences(shown, now []DifferenceView) bool {
	if slices.Contains(shown, DifferenceView{Subject: subjectSentinels, Store: storeOf(shown), Reported: sentinelsLater}) {
		drop := func(d DifferenceView) bool { return d.Subject == subjectSentinels }
		shown = slices.DeleteFunc(slices.Clone(shown), drop)
		now = slices.DeleteFunc(slices.Clone(now), drop)
	}
	return slices.Equal(shown, now)
}

// storeOf is the Store of the line of the sentinels among diffs.
func storeOf(diffs []DifferenceView) string {
	for _, d := range diffs {
		if d.Subject == subjectSentinels {
			return d.Store
		}
	}
	return ""
}

const (
	subjectSentinels = "sentinels at Cloudflare"
	sentinelsLater   = "read once confirmed: this node does not hold the lease yet"
)

// differences says what the store and the agents' reports, and the
// sentinels as read, say differently: the epoch of each agent, the nodes,
// the revisions the tenure wrote, the cluster, the generation of the
// sentinels.
func (e *Engine) differences(ctx context.Context, inst store.Install, sent sentinelRead) []DifferenceView {
	out := []DifferenceView{}
	rec, _, found, err := e.d.Store.Leader()
	stored := "none"
	switch {
	case err != nil:
		stored = "unreadable: " + err.Error()
	case found:
		stored = epochText(rec.Writer)
	}
	if ep, ok, err := e.d.Store.AgentEpoch(); err == nil && ok && (!found || !sameEpochAs(rec.Writer, agentproto.Epoch(ep))) {
		out = append(out, DifferenceView{Subject: "epoch the agent of " + e.d.Node + " remembers", Store: stored, Reported: agentproto.Epoch(ep).String()})
	}
	reports := e.fleet.Reports(ctx, 0, nil)
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		ep := reports[name].Status.EpochSeen
		if ep.Nonce != "" && (!found || !sameEpochAs(rec.Writer, ep)) {
			out = append(out, DifferenceView{Subject: "epoch the agent of " + name + " follows", Store: stored, Reported: ep.String()})
		}
	}
	if nodes, err := e.d.Store.Nodes(); err == nil {
		registered := make(map[string]bool, len(nodes))
		for _, n := range nodes {
			registered[n.Name] = true
			if _, ok := reports[n.Name]; !ok && n.State != store.NodeRemoved {
				out = append(out, DifferenceView{Subject: "node " + n.Name, Store: "registered" + stateText(n.State), Reported: "no report"})
			}
		}
		for _, name := range slices.Sorted(maps.Keys(reports)) {
			if !registered[name] {
				out = append(out, DifferenceView{Subject: "node " + name, Store: "not registered", Reported: "reports"})
			}
		}
	}
	if err := e.storeWentBack(); err != nil {
		out = append(out, DifferenceView{Subject: "revisions", Store: err.Error(), Reported: "written by this controller"})
	}
	if read := e.d.Cluster; read != nil {
		if here, err := read(); err == nil && inst.Cluster != nil && inst.Cluster.CASHA256 != here.CASHA256 {
			out = append(out, DifferenceView{Subject: "cluster",
				Store: clusterName(*inst.Cluster) + " (" + inst.Cluster.CASHA256 + ")", Reported: clusterName(here) + " (" + here.CASHA256 + ")"})
		}
	}
	switch {
	case sent.skipped:
		// Only the controller reads Cloudflare; the confirmed promotion
		// reads the sentinels once this node holds the lease.
		out = append(out, DifferenceView{Subject: subjectSentinels, Store: stored, Reported: sentinelsLater})
	case sent.err != nil:
		out = append(out, DifferenceView{Subject: subjectSentinels, Store: stored, Reported: "not read: " + sent.err.Error()})
	case !found || sent.highest > rec.Generation:
		out = append(out, DifferenceView{Subject: subjectSentinels, Store: stored, Reported: fmt.Sprintf("generation %d", sent.highest)})
	}
	return out
}

func epochText(w planner.Writer) string { return fmt.Sprintf("%d.%s", w.Generation, w.Nonce) }

func sameEpochAs(w planner.Writer, e agentproto.Epoch) bool {
	return w.Generation == e.Generation && w.Nonce == e.Nonce
}

func stateText(state string) string {
	if state == "" {
		return ""
	}
	return ", " + state
}

// awaitHolder waits, for a while, until this node holds the lease: a node
// whose store was of another cluster takes it only now.
func (e *Engine) awaitHolder(ctx context.Context) error {
	wait, cancel := e.timeout(ctx, holderWait)
	defer cancel()
	for {
		st := e.d.Lease.State()
		if st.Holder {
			return nil
		}
		tick, stop := e.after(time.Second)
		select {
		case <-wait.Done():
			stop()
			why := st.Waiting
			if why == "" {
				why = "the lease keeper has not tried yet"
			}
			return fmt.Errorf("%w: this node does not hold the lease: %s", ErrRefused, why)
		case <-tick:
		}
	}
}

// promotionSeed is the generation a promotion draws above: the highest of
// the sentinels of the install at Cloudflare, as read, and of the epochs the
// agents reported. A sentinel that cannot be read could hold a higher one:
// the promotion is refused before it gets here.
func (e *Engine) promotionSeed(ctx context.Context, installID string, sentinel int) int {
	seed := sentinel
	if ep, found, err := e.d.Store.AgentEpoch(); err == nil && found && ep.InstallID == installID {
		seed = max(seed, ep.Generation)
	}
	for _, r := range e.fleet.Reports(ctx, 0, nil) {
		if ep := r.Status.EpochSeen; ep.InstallID == installID {
			seed = max(seed, ep.Generation)
		}
	}
	return seed
}

// highestSentinel is the highest generation of a writer of the install in
// the sentinels of its tunnels, through every stored credential; 0 when
// there is none.
func (e *Engine) highestSentinel(ctx context.Context, installID string) (int, error) {
	ctx, cancel := e.timeout(ctx, promoteReads)
	defer cancel()
	creds, err := e.d.Store.Credentials()
	if err != nil {
		return 0, fmt.Errorf("reading the credentials: %w", err)
	}
	if len(creds) == 0 {
		return 0, errors.New("no Cloudflare credential is stored")
	}
	highest := 0
	for _, cred := range creds {
		api, err := e.d.NewClient(cred)
		if err != nil {
			return 0, fmt.Errorf("credential %s: %w", cred.ID, err)
		}
		g, err := sentinelOf(ctx, api, installID)
		if err != nil {
			return 0, fmt.Errorf("credential %s: %w", cred.ID, err)
		}
		highest = max(highest, g)
	}
	return highest, nil
}

// sentinelOf is the highest generation of the install in the sentinels of
// the tunnels one credential sees, in the accounts it lists and those of its
// zones.
func sentinelOf(ctx context.Context, api cfapi.API, installID string) (int, error) {
	listed, err := api.Accounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing the accounts: %w", err)
	}
	zones, err := api.Zones(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing the zones: %w", err)
	}
	accounts := map[string]bool{}
	for _, a := range listed {
		accounts[a.ID] = true
	}
	for _, z := range zones {
		accounts[z.AccountID] = true
	}
	highest := 0
	for _, account := range slices.Sorted(maps.Keys(accounts)) {
		tunnels, err := api.Tunnels(ctx, account, planner.TunnelName(installID))
		if err != nil {
			return 0, fmt.Errorf("listing the tunnels of account %s: %w", account, err)
		}
		for _, t := range tunnels {
			cfg, err := api.TunnelConfig(ctx, account, t.ID)
			if err != nil {
				return 0, fmt.Errorf("reading the configuration of tunnel %s (%s): %w", t.Name, t.ID, err)
			}
			for _, rule := range cfg.Ingress {
				if w, ok := planner.ParseSentinel(rule.Hostname); ok && w.InstallID == installID {
					highest = max(highest, w.Generation)
				}
			}
		}
	}
	return highest, nil
}

// observeOnly sets the settings to observe-only, which a new epoch drawn by
// hand begins with.
func (e *Engine) observeOnly() error {
	s, err := e.d.Store.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	if s.ObserveOnly {
		return nil
	}
	s.ObserveOnly = true
	if err := e.d.Store.SaveSettings(s); err != nil {
		return fmt.Errorf("setting observe-only: %w", err)
	}
	return nil
}
