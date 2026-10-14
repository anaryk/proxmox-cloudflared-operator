package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

// reasonNotResolved is the reason of a target another node chooses the
// address of.
const reasonNotResolved = "this node does not resolve this target, and it verifies no address another node chose"

// view is what the agent last found, for Status and View. It is guarded by
// repMu.
type view struct {
	epoch       agentproto.Epoch
	seq         uint64
	seqAt       time.Time
	targets     []agentproto.TargetReport
	setOf       map[string]egress.Target // the target the set holds for each report's address, by key
	conns       []connector.Status
	egress      string
	set         []egress.Target
	localBlocks []netip.Addr
	services    []netip.Addr // what the gateway maps
	actions     []agentproto.ActionResult
	notes       []string // what holds whatever a pass finds
	problems    []string // of the last pass
	stood       string
}

// publish keeps what a pass found. A pass that did not get to its end keeps
// the targets, the connectors and the actions of the last one that did. The
// caller holds the agent's lock.
func (a *Agent) publish(p *pass) {
	a.repMu.Lock()
	defer a.repMu.Unlock()
	if p.done {
		a.rep.targets, a.rep.setOf = p.reports()
		a.rep.conns = p.conns
		a.rep.egress = p.egress
		a.rep.localBlocks = p.localBlocks
		a.rep.actions = a.actionResults(p.s)
	}
	a.rep.problems = p.problems
	a.fill()
}

// republish keeps what holds whatever a pass finds. The caller holds the
// agent's lock, or is New.
func (a *Agent) republish() {
	a.repMu.Lock()
	defer a.repMu.Unlock()
	if a.stood != "" {
		a.rep.targets, a.rep.actions = nil, nil
	}
	a.fill()
}

// fill copies into rep what the agent follows. The caller holds repMu and the
// agent's lock.
func (a *Agent) fill() {
	r := &a.rep
	r.epoch, r.seq, r.seqAt, r.stood = a.remembered, a.applied, a.seqAt, a.stood
	r.set = slices.Clone(a.set)
	r.services = slices.Clone(a.gwServices)
	r.notes = nil
	for _, note := range []string{a.stood, a.unreadable, a.refused} {
		if note != "" {
			r.notes = append(r.notes, note)
		}
	}
}

// reports says how each target of the snapshot fares on this node, and what
// the set holds for each that verified. The result is the resolver's, before
// the minimum held anything back, for the controller to judge as it judges
// its own. Whether a target is in the set is judged when the status is made,
// against the set as it is then.
func (p *pass) reports() ([]agentproto.TargetReport, map[string]egress.Target) {
	setOf := map[string]egress.Target{}
	out := make([]agentproto.TargetReport, 0, len(p.s.Targets))
	for _, t := range p.s.Targets {
		r := agentproto.TargetReport{Key: t.Key}
		res, resolved := p.results[t.Key]
		switch {
		case !t.Resolve && !resolved:
			r.Addr, r.Reason = t.Addr, reasonNotResolved
		case !resolved:
			r.Reason = "not resolved"
		default:
			served := p.served[t.Key]
			r.Addr, r.Level, r.OK = res.Target.Addr, string(res.Level), p.verified[t.Key]
			r.AttestedBy = p.attested[t.Key]
			r.Attested = r.AttestedBy != ""
			r.Reason = cmp.Or(p.held[t.Key], res.Target.Reason)
			switch b := served.Binding; {
			case b != nil:
				r.VerifiedAt = b.VerifiedAt
			case r.OK:
				r.VerifiedAt = p.now
			}
			if et, ok := p.verifiedTarget(t); ok {
				setOf[t.Key] = egress.Target{Addr: et.Addr, Port: et.Port}
			}
			raw, err := EncodeResult(res)
			if err != nil {
				p.problem("target %s: encoding its result: %v", t.Key, err)
			}
			r.Result = raw
		}
		out = append(out, r)
	}
	return out, setOf
}

// inSet is what the filter holds now, by target: the set the agent gave it,
// less the addresses whose MAC moved and those blocked on this node. A set
// the filter did not take is in no table; one the admin switched off holds
// nothing back.
func (a *Agent) inSet(r view) map[egress.Target]bool {
	if r.egress != egressOn && r.egress != egressOff {
		return nil
	}
	a.egMu.Lock()
	defer a.egMu.Unlock()
	out := map[egress.Target]bool{}
	for _, t := range a.unsuspected() {
		if !slices.Contains(r.localBlocks, t.Addr) {
			out[egress.Target{Addr: t.Addr, Port: t.Port}] = true
		}
	}
	return out
}

// judge decides which targets verified in this pass: proven at the level
// they require, their port answers, and a connector can dial them.
func (p *pass) judge() {
	for _, t := range p.s.Targets {
		res, ok := p.served[t.Key]
		if !ok {
			continue
		}
		_, dialed := res.Target.Dialed()
		p.verified[t.Key] = serves(t, res) && res.Target.Reachable && dialed
	}
}

// Status is what the agent reports to the controller: what the last pass
// found and what the agent follows, as of now.
func (a *Agent) Status(context.Context) agentproto.Status {
	now := a.d.Now()
	a.repMu.Lock()
	r := a.rep
	a.repMu.Unlock()
	in := a.inSet(r)
	st := agentproto.Status{
		Protocol:       agentproto.Range{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax},
		Build:          version.Version,
		Node:           a.d.Node,
		Kind:           a.d.Kind,
		AgentStartedAt: a.started,
		EpochSeen:      r.epoch,
		SeqApplied:     r.seq,
		Targets:        slices.Clone(r.targets),
		Connectors:     slices.Clone(r.conns),
		Egress:         r.egress,
		LocalBlocks:    slices.Clone(r.localBlocks),
		ServiceAddrs:   slices.Clone(r.services),
		Clock:          now,
		Actions:        slices.Clone(r.actions),
		Problems:       a.problems(r, now),
	}
	for i, t := range st.Targets {
		st.Targets[i].InEgressSet = in[r.setOf[t.Key]]
	}
	a.facts(&st)
	return st
}

// problems are the lines of the status and of the view.
func (a *Agent) problems(r view, now time.Time) []string {
	out := slices.Concat(r.notes, r.problems)
	if silent := a.silentFor(now); silent >= silentAfter {
		out = append(out, fmt.Sprintf("no controller answered for %s; this node serves what it was last told", silent.Round(time.Second)))
	}
	return out
}

// View is what pco agent status shows of the agent.
func (a *Agent) View() agentproto.AgentView {
	now := a.d.Now()
	a.repMu.Lock()
	r := a.rep
	a.repMu.Unlock()
	v := agentproto.AgentView{
		Node: a.d.Node, Kind: a.d.Kind, SilentFor: a.silentFor(now), Epoch: r.epoch, Seq: r.seq,
		EgressTargets: len(r.set), StoodDown: r.stood, Problems: append([]string{}, a.problems(r, now)...),
	}
	if !r.seqAt.IsZero() {
		v.SeqAge = now.Sub(r.seqAt)
	}
	for _, c := range r.conns {
		if c.Active && c.Install == a.d.InstallID {
			v.Connectors++
		}
	}
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	if a.hb != nil {
		v.Heard, v.ReadyReplicas, v.HeartbeatAge = true, maps.Clone(a.hb.ReadyReplicas), now.Sub(a.hbAt)
	}
	return v
}

// Heartbeat takes the controller's answer to a status: that a controller is
// alive, which nodes report, and how many replicas of each tunnel are ready.
// One of another install, or of an epoch below the one the agent follows, is
// a stale controller's and counts for nothing.
func (a *Agent) Heartbeat(h agentproto.Heartbeat) {
	a.repMu.Lock()
	following := a.rep.epoch
	a.repMu.Unlock()
	if h.Epoch.InstallID != a.d.InstallID {
		return
	}
	if c, _ := h.Epoch.Order(following); c < 0 {
		return
	}
	h.ReadyReplicas, h.Nodes = maps.Clone(h.ReadyReplicas), maps.Clone(h.Nodes)
	now := a.d.Now()
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	a.hb, a.hbAt, a.heardAt = &h, now, now
}

// HeardLive says whether, at now, a heartbeat less than within old came from
// a controller that called its lease valid: the node of an agent that hears
// one never tries to take the lease from it.
func (a *Agent) HeardLive(now time.Time, within time.Duration) bool {
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	return a.hb != nil && a.hb.LeaseValid && now.Sub(a.hbAt) < within
}

// hear notes that a controller answered now.
func (a *Agent) hear() {
	now := a.d.Now()
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	a.heardAt = now
}

// heardSince reports whether a controller answered at or after t.
func (a *Agent) heardSince(t time.Time) bool {
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	return !a.heardAt.IsZero() && !a.heardAt.Before(t)
}

// silentFor is how long no controller has answered, at most since the agent
// started.
func (a *Agent) silentFor(now time.Time) time.Duration {
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	last := a.started
	if a.heardAt.After(last) {
		last = a.heardAt
	}
	return max(now.Sub(last), 0)
}

// otherReady is how many replicas of a tunnel other than this node's the last
// heartbeat counted ready.
func (a *Agent) otherReady(tunnelID string) int {
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	if a.hb == nil {
		return 0
	}
	n := a.hb.ReadyReplicas[tunnelID]
	if a.ownReady[tunnelID] {
		n--
	}
	return n
}

// multiNode reports whether the last heartbeat named a node besides this
// one: then a second node is registered.
func (a *Agent) multiNode() bool {
	a.heardMu.Lock()
	defer a.heardMu.Unlock()
	return a.hb != nil && len(a.hb.Nodes) > 1
}

// wireResult is a resolve.Result as a status carries it. Its level is a
// plain string: a manual route has a level no binding may.
type wireResult struct {
	Target     planner.ResolvedTarget    `json:"target"`
	Binding    *resolve.Binding          `json:"binding,omitempty"`
	Candidates []resolve.CandidateResult `json:"candidates,omitempty"`
	Level      string                    `json:"level,omitempty"`
	SoftDenied string                    `json:"softDenied,omitempty"`
}

// EncodeResult writes the result of a resolution as a status carries it.
func EncodeResult(res resolve.Result) (json.RawMessage, error) {
	return json.Marshal(wireResult{
		Target: res.Target, Binding: res.Binding, Candidates: res.Candidates, Level: string(res.Level), SoftDenied: res.SoftDenied,
	})
}

// DecodeResult reads what EncodeResult writes.
func DecodeResult(raw json.RawMessage) (resolve.Result, error) {
	var w wireResult
	if err := json.Unmarshal(raw, &w); err != nil {
		return resolve.Result{}, fmt.Errorf("reading the result of a resolution: %w", err)
	}
	switch l := resolve.Level(w.Level); l {
	case "", resolve.LevelObserved, resolve.LevelFiltered, resolve.LevelPort, resolve.LevelManual:
	default:
		return resolve.Result{}, fmt.Errorf("reading the result of a resolution: unknown identity level %q", w.Level)
	}
	return resolve.Result{Target: w.Target, Binding: w.Binding, Candidates: w.Candidates, Level: resolve.Level(w.Level), SoftDenied: w.SoftDenied}, nil
}
