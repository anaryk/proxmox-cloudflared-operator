package agent

import (
	"bytes"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const problemUnreadable = "the desired state kept on this node cannot be read (%v): it was damaged or edited by hand; " +
	"this node waits for the controller's next one"

// load reads what the agent kept on disk: the epoch it follows, the desired
// state it last accepted and the seq of that under the epoch, the actions it
// ran and the egress set it last gave. A node set up anew for another install
// forgets all of the one before, and starts from nothing.
func (a *Agent) load() error {
	a.remembered = agentproto.Epoch{InstallID: a.d.InstallID}
	e, found, err := a.d.Store.AgentEpoch()
	if err != nil {
		return fmt.Errorf("agent: reading the epoch it follows: %w", err)
	}
	switch {
	case found && e.InstallID == a.d.InstallID:
		a.remembered = agentproto.Epoch(e)
	case found:
		if err := a.forgetInstall(e.InstallID); err != nil {
			return err
		}
	}

	raw, found, err := a.d.Store.AgentSnapshot()
	switch {
	case err != nil:
		a.unreadable = fmt.Sprintf(problemUnreadable, err)
	case found:
		s, err := agentproto.DecodeSnapshot(bytes.NewReader(raw))
		switch {
		case err != nil:
			a.unreadable = fmt.Sprintf(problemUnreadable, err)
		case s.Epoch.InstallID == a.d.InstallID && s.Node == a.d.Node:
			var buf bytes.Buffer
			if err := agentproto.EncodeSnapshot(&buf, s); err != nil {
				return err
			}
			a.desired, a.desiredRaw, a.seqAt = &s, buf.Bytes(), a.started
			a.noteEndpoints(s)
			switch c, same := s.Epoch.Order(a.remembered); {
			case same:
				a.applied = s.Seq
			case c > 0:
				// The epoch is written before the desired state, so one
				// below it was lost or edited: the desired state's is what
				// is followed, and nothing below it is taken.
				if err := a.d.Store.SaveAgentEpoch(store.Epoch(s.Epoch)); err != nil {
					return fmt.Errorf("agent: keeping the epoch of the desired state: %w", err)
				}
				a.remembered, a.applied = s.Epoch, s.Seq
			}
		}
	}

	done, err := a.d.Store.AgentActions()
	if err != nil {
		return fmt.Errorf("agent: reading the actions it ran: %w", err)
	}
	a.done = done

	set, found, err := a.d.Store.AgentEgress()
	if err != nil {
		return fmt.Errorf("agent: reading the egress set it gave: %w", err)
	}
	if !found {
		// The engine of an earlier release kept the set in its memory; one of
		// another install says nothing for this one.
		// One that cannot be read holds nothing the agent trusts: what it
		// verifies enters the set again.
		m, err := a.d.Store.EngineMemory()
		if err != nil {
			a.d.Log.Warn().Err(err).Msg("the egress set the engine kept cannot be read; a target enters the set as it verifies")
		}
		if err == nil && (m.InstallID == "" || m.InstallID == a.d.InstallID) {
			set = m.Egress
		}
	}
	a.set = fromAddrPorts(set)
	return nil
}

// forgetInstall drops what the agent kept of install id, which this node
// was enrolled in before: the epoch, the desired state, the egress set and
// the actions. What it verified for that install says nothing for this one.
func (a *Agent) forgetInstall(id string) error {
	for what, forget := range map[string]func() error{
		"epoch":         a.d.Store.DeleteAgentEpoch,
		"desired state": a.d.Store.DeleteAgentSnapshot,
		"egress set":    func() error { return a.d.Store.SaveAgentEgress(nil) },
		"actions":       func() error { return a.d.Store.SaveAgentActions(map[string]store.ActionDone{}) },
	} {
		if err := forget(); err != nil {
			return fmt.Errorf("agent: forgetting the %s of install %s: %w", what, id, err)
		}
	}
	a.d.Log.Info().Str("install", id).Msg("this node was enrolled in another install before; what it kept of it is forgotten")
	return nil
}

// persist keeps s as the desired state the agent follows, durably, before it
// is applied: the epoch first, then the snapshot, written as the controller
// sent it, tokens included, and only when it is not what is kept already.
// The error says when the epoch moved and the snapshot was not kept.
func (a *Agent) persist(s agentproto.Snapshot) error {
	if _, same := s.Epoch.Order(a.remembered); !same {
		if err := a.d.Store.SaveAgentEpoch(store.Epoch(s.Epoch)); err != nil {
			return fmt.Errorf("saving the epoch: %w", err)
		}
		a.remembered, a.applied = s.Epoch, 0
	}
	var buf bytes.Buffer
	if err := agentproto.EncodeSnapshot(&buf, s); err != nil {
		return err
	}
	if !bytes.Equal(buf.Bytes(), a.desiredRaw) {
		if err := a.d.Store.SaveAgentSnapshot(buf.Bytes()); err != nil {
			err = fmt.Errorf("saving the desired state: %w", err)
			if d := a.desired; d != nil {
				if _, same := d.Epoch.Order(s.Epoch); !same {
					err = fmt.Errorf("%w; the one of epoch %d seq %d it kept before is what runs", err, d.Epoch.Generation, d.Seq)
				}
			}
			return err
		}
	}
	if a.desired == nil || a.applied != s.Seq {
		a.seqAt = a.d.Now()
	}
	a.desired, a.desiredRaw, a.applied, a.unreadable = &s, buf.Bytes(), s.Seq, ""
	a.noteEndpoints(s)
	return nil
}

// noteEndpoints keeps where s says the controller answers; a snapshot that
// says nothing of it keeps what was known.
func (a *Agent) noteEndpoints(s agentproto.Snapshot) {
	if len(s.Endpoints) > 0 {
		eps := slices.Clone(s.Endpoints)
		a.endpoints.Store(&eps)
	}
}

// ControllerEndpoints are where the controller answers, as the desired state
// the agent follows says; none when it says nothing of it.
func (a *Agent) ControllerEndpoints() []string {
	if eps := a.endpoints.Load(); eps != nil {
		return slices.Clone(*eps)
	}
	return nil
}

// forgetDesired deletes the desired state kept on disk, or, with keep, only
// its run tokens. What the agent remembers follows the disk: one that could
// not be changed is remembered as it is.
func (a *Agent) forgetDesired(keep bool) error {
	if !keep || a.desired == nil {
		if err := a.d.Store.DeleteAgentSnapshot(); err != nil {
			return err
		}
		a.desired, a.desiredRaw = nil, nil
		return nil
	}
	s := *a.desired
	s.Tunnels = make([]agentproto.Tunnel, len(a.desired.Tunnels))
	for i, t := range a.desired.Tunnels {
		t.Token = agentproto.Secret{}
		s.Tunnels[i] = t
	}
	var buf bytes.Buffer
	if err := agentproto.EncodeSnapshot(&buf, s); err != nil {
		return err
	}
	if err := a.d.Store.SaveAgentSnapshot(buf.Bytes()); err != nil {
		return err
	}
	a.desired, a.desiredRaw = &s, buf.Bytes()
	return nil
}

// saveDone keeps the actions the agent ran, when they changed, and remembers
// what the store keeps of them.
func (a *Agent) saveDone(next map[string]store.ActionDone) error {
	next = store.NewestAgentActions(next)
	if maps.Equal(next, a.done) {
		return nil
	}
	if err := a.d.Store.SaveAgentActions(next); err != nil {
		return fmt.Errorf("saving the actions this node ran: %w", err)
	}
	a.done = next
	return nil
}

// saveSet keeps the egress set as the filter was given it, less the
// addresses whose MAC moved and that were not verified again yet: a daemon
// that starts anew must not let one back in on what it knew before the move.
// The store writes only a change. The caller does not hold egMu.
func (a *Agent) saveSet() error {
	a.egMu.Lock()
	set := a.unsuspected()
	a.egMu.Unlock()
	if err := a.d.Store.SaveAgentEgress(toAddrPorts(set)); err != nil {
		return fmt.Errorf("saving the egress set: %w", err)
	}
	return nil
}

func fromAddrPorts(aps []netip.AddrPort) []egress.Target {
	out := make([]egress.Target, 0, len(aps))
	for _, ap := range aps {
		out = append(out, egress.Target{Addr: ap.Addr(), Port: ap.Port()})
	}
	return out
}

func toAddrPorts(ts []egress.Target) []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(ts))
	for _, t := range ts {
		out = append(out, netip.AddrPortFrom(t.Addr, t.Port))
	}
	return out
}
