package upgrade

import (
	"context"
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	// ReadyWait is how long a connector has to be ready again after its
	// restart.
	ReadyWait = 60 * time.Second
	readyPoll = time.Second
)

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
}

// Restart restarts the connectors.
func (r ConnectorRestart) Restart(ctx context.Context) error {
	ids, err := r.Connectors.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the connectors: %w", err)
	}
	for _, id := range ids {
		if err := r.restartOne(ctx, id); err != nil {
			return err
		}
	}
	return nil
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
	r.Say("restarting the connector of tunnel %s: the tunnel has no connection from this appliance until it is ready again, at most %s",
		id, ReadyWait)
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
