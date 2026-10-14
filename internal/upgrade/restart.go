package upgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	// ReadyWait is how long a connector has to be ready again after its
	// restart.
	ReadyWait = 60 * time.Second
	readyPoll = time.Second

	// ReplicaWait is how long the connector of an agent waits for another
	// replica of its tunnel to be ready before it restarts.
	ReplicaWait = 5 * time.Minute
	replicaPoll = 5 * time.Second
	// heartbeatFresh is the oldest heartbeat a restart is decided on: an
	// agent reports every 10 s and hears one with each answer, and the
	// controller calls a node unknown after 30 s without a status.
	heartbeatFresh = 30 * time.Second
)

var (
	// ErrNoOtherReplica is the error of a connector that was not restarted
	// because no other replica of its tunnel was ready.
	ErrNoOtherReplica = errors.New("no other replica of the tunnel is ready")
	// ErrCannotTell is the error of an agent that cannot tell whether the
	// other replicas of its tunnels are ready.
	ErrCannotTell = errors.New("pco cannot tell whether another replica of each tunnel is ready")
)

// AgentView is the agent of this node as the daemon shows it, with what the
// last heartbeat of its controller counted.
type AgentView func(ctx context.Context) (agentproto.AgentView, error)

// Connectors is what a restart needs of the connector manager.
type Connectors interface {
	List(ctx context.Context) ([]string, error)
	Status(ctx context.Context, tunnelID string) (connector.Status, error)
}

// ConnectorRestart restarts the connectors that run, one after the other, so
// that they run the cloudflared just installed, and waits for each to be
// ready again. A connector that does not run stays as it is.
type ConnectorRestart struct {
	Connectors Connectors
	Systemd    connector.Systemd
	Now        func() time.Time
	// Sleep waits for d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Say prints a line for the admin: the outage before and after.
	Say func(format string, args ...any)
	// Replicas, when set, is the agent of a node that is one replica of
	// its tunnels among others: a ready connector restarts only while the
	// controller counts another ready replica of its tunnel, so that the
	// upgrade never takes out the only one. Nil on a node alone, whose
	// tunnels have no other replica to wait for.
	Replicas AgentView
}

// CheckReplicas refuses when the agent cannot tell whether the other
// replicas of its tunnels are ready: no daemon, no heartbeat, or one too
// old. It is asked before anything is installed, and returns the tunnels of
// this node's connectors that the heartbeat counts one ready replica of at
// most: their connectors will wait.
func CheckReplicas(ctx context.Context, view AgentView, conns Connectors) ([]string, error) {
	if _, why := otherReady(ctx, view, ""); why != "" {
		return nil, fmt.Errorf("%w: %s; an agent restarts its connectors only beside a ready replica, and --force restarts them without",
			ErrCannotTell, why)
	}
	v, err := view(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := conns.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the connectors: %w", err)
	}
	var lone []string
	for _, id := range ids {
		if v.ReadyReplicas[id] <= 1 {
			lone = append(lone, id)
		}
	}
	return lone, nil
}

// otherReady is how many replicas of the tunnel besides this node's ready
// connector the last heartbeat counted, or why that cannot be told.
func otherReady(ctx context.Context, view AgentView, tunnelID string) (int, string) {
	v, err := view(ctx)
	switch {
	case err != nil:
		return 0, "the daemon does not answer: " + err.Error()
	case !v.Heard:
		return 0, "the agent of this node has heard no heartbeat of its controller"
	case v.HeartbeatAge < 0:
		return 0, fmt.Sprintf("the last heartbeat of the controller is %s old: the clock of this node stepped back",
			v.HeartbeatAge.Round(time.Second))
	case v.HeartbeatAge > heartbeatFresh:
		return 0, fmt.Sprintf("the last heartbeat of the controller is %s old", v.HeartbeatAge.Round(time.Second))
	}
	// The count takes in this node's connector, which is ready.
	return max(v.ReadyReplicas[tunnelID]-1, 0), ""
}

// Restart restarts the connectors. A connector of an agent that finds no
// other ready replica of its tunnel is left as it is, and the others restart
// all the same.
func (r ConnectorRestart) Restart(ctx context.Context) error {
	ids, err := r.Connectors.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the connectors: %w", err)
	}
	var refused []error
	for _, id := range ids {
		err := r.restartOne(ctx, id)
		switch {
		case errors.Is(err, ErrNoOtherReplica):
			refused = append(refused, err)
		case err != nil:
			return errors.Join(append(refused, err)...)
		}
	}
	return errors.Join(refused...)
}

// restartOne restarts one connector. Ready again means ready under another
// connector id than before the restart: the old process may still answer
// /ready while systemd stops it, and every start of cloudflared gets an id of
// its own.
func (r ConnectorRestart) restartOne(ctx context.Context, id string) error {
	st, err := r.Connectors.Status(ctx, id)
	if err != nil {
		return fmt.Errorf("the connector of tunnel %s: %w", id, err)
	}
	if !st.Active {
		r.Say("the connector of tunnel %s does not run; it runs the new cloudflared once it is started", id)
		return nil
	}
	before := st.ConnectorID
	if r.Replicas != nil && st.Ready {
		others, err := r.waitForReplica(ctx, id)
		if err != nil {
			return err
		}
		r.Say("restarting the connector of tunnel %s: %s of the tunnel %s ready and serves it meanwhile", id, otherReplicas(others), isAre(others))
	} else {
		r.Say("restarting the connector of tunnel %s: the tunnel has no connection from this appliance until it is ready again, at most %s",
			id, ReadyWait)
	}
	unit := connector.UnitName(id)
	start := r.Now()
	if err := r.Systemd.Restart(ctx, unit); err != nil {
		return fmt.Errorf("restarting %s: %w", unit, err)
	}
	for {
		if err := r.Sleep(ctx, readyPoll); err != nil {
			return err
		}
		st, err = r.Connectors.Status(ctx, id)
		waited := r.Now().Sub(start)
		if err == nil && st.Active && st.Ready && (before == "" || st.ConnectorID != before) {
			r.Say("the connector of tunnel %s is ready again after %s: %s", id, waited.Round(time.Second), st.Text())
			return nil
		}
		if waited >= ReadyWait {
			if err != nil {
				return fmt.Errorf("the connector of tunnel %s is not ready %s after its restart: %w", id, ReadyWait, err)
			}
			return fmt.Errorf("the connector of tunnel %s is not ready %s after its restart: %s; pco status shows how it fares",
				id, ReadyWait, st.Text())
		}
	}
}

// waitForReplica waits until the controller counts a ready replica of the
// tunnel besides this node's, ReplicaWait at most.
func (r ConnectorRestart) waitForReplica(ctx context.Context, id string) (int, error) {
	start := r.Now()
	said := false
	for {
		others, why := otherReady(ctx, r.Replicas, id)
		if why == "" && others > 0 {
			return others, nil
		}
		if why == "" {
			why = "the controller counts no other ready replica"
		}
		if r.Now().Sub(start) >= ReplicaWait {
			return 0, fmt.Errorf("the connector of tunnel %s is not restarted: %w %s later (%s); it runs the cloudflared before until "+
				"systemctl restart %s, once pco agent status counts another ready replica",
				id, ErrNoOtherReplica, ReplicaWait, why, connector.UnitName(id))
		}
		if !said {
			r.Say("waiting up to %s for another replica of tunnel %s to be ready before its connector restarts: %s", ReplicaWait, id, why)
			said = true
		}
		if err := r.Sleep(ctx, replicaPoll); err != nil {
			return 0, err
		}
	}
}

func otherReplicas(n int) string {
	if n == 1 {
		return "1 other replica"
	}
	return fmt.Sprintf("%d other replicas", n)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
