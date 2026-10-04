package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// probeTimeout bounds one request to the /ready endpoint of a connector.
	probeTimeout = 2 * time.Second
	// maxReadyBody is how much of the answer is read. The real one is a line.
	maxReadyBody = 64 << 10
)

// Status is what is known of the connector of one tunnel.
type Status struct {
	TunnelID    string `json:"tunnelId"`
	Active      bool   `json:"active"`                // unit started or starting, including the restart back-off
	Ready       bool   `json:"ready"`                 // /ready answered 200
	Connections int    `json:"connections"`           // readyConnections
	ConnectorID string `json:"connectorId,omitempty"` // as /ready names the running cloudflared; empty when it is not ready
	MetricsAddr string `json:"metricsAddr,omitempty"` // empty when the tunnel has no usable env file
	// Install is the install the env file names; empty when it names none,
	// as one written before connectors named their install.
	Install string `json:"install,omitempty"`
	// TokenRefused says that a connector that runs and is not ready logged
	// last that Cloudflare refused its token, as after the secret of the
	// tunnel was rotated.
	TokenRefused bool `json:"tokenRefused,omitempty"`
	// MetricsPortHeld says that a connector that runs and is not ready logged
	// last that another process holds its metrics port, so that it cannot
	// start. The next Ensure gives it another port.
	MetricsPortHeld bool `json:"metricsPortHeld,omitempty"`
}

// Text says how a connector fares: "inactive", "active, not ready" or
// "active, ready, 4 connections".
func (s Status) Text() string {
	switch {
	case !s.Active:
		return "inactive"
	case !s.Ready:
		return "active, not ready"
	case s.Connections == 1:
		return "active, ready, 1 connection"
	}
	return fmt.Sprintf("active, ready, %d connections", s.Connections)
}

// Status reports the unit's state and, when it runs, asks its metrics endpoint
// whether it is connected. A connector that does not answer is not ready; that
// is no error. A tunnel without a usable env file has no endpoint to ask and
// is reported as it is, not ready.
func (m *Manager) Status(ctx context.Context, tunnelID string) (Status, error) {
	if err := checkID(tunnelID); err != nil {
		return Status{}, err
	}
	unit := UnitName(tunnelID)
	active, err := m.sd.IsActive(ctx, unit)
	if err != nil {
		return Status{}, fmt.Errorf("checking %s: %w", unit, err)
	}
	st := Status{TunnelID: tunnelID, Active: active}

	values, err := readEnv(m.path(envFile(tunnelID)))
	if err != nil {
		return Status{}, fmt.Errorf("tunnel %s: reading env file: %w", tunnelID, err)
	}
	st.Install = values[installKey]
	addr, port, err := metricsOf(values)
	if err != nil {
		return st, nil
	}
	st.MetricsAddr = addr
	if active {
		r := m.probe(ctx, addr)
		st.Ready, st.Connections, st.ConnectorID = r.ready, r.ReadyConnections, r.ConnectorID
		if !st.Ready {
			m.readJournal(ctx, &st, port)
		}
	}
	return st, nil
}

// readiness is what /ready answered.
type readiness struct {
	ready            bool
	ReadyConnections int    `json:"readyConnections"`
	ConnectorID      string `json:"connectorId"`
}

// probe asks the /ready endpoint of cloudflared. It answers 200 once the
// connector holds a connection to the edge, with a body such as
// {"status":200,"readyConnections":4,"connectorId":"..."}.
func (m *Manager) probe(ctx context.Context, addr string) readiness {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/ready", nil)
	if err != nil {
		return readiness{}
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		m.log.Debug().Err(err).Str("addr", addr).Msg("connector did not answer /ready")
		return readiness{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return readiness{}
	}
	var r readiness
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReadyBody)).Decode(&r); err != nil {
		return readiness{ready: true}
	}
	r.ready = true
	return r
}
