package connector

import (
	"context"
	"encoding/json"
	"errors"
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
	MetricsAddr string `json:"metricsAddr,omitempty"` // empty when the tunnel has no usable env file
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

	addr, _, err := readMetricsAddr(m.path(envFile(tunnelID)))
	switch {
	case errors.Is(err, errNoAddress):
		return st, nil
	case err != nil:
		return Status{}, fmt.Errorf("tunnel %s: reading env file: %w", tunnelID, err)
	}
	st.MetricsAddr = addr
	if active {
		st.Ready, st.Connections = m.probe(ctx, addr)
	}
	return st, nil
}

// probe asks the /ready endpoint of cloudflared. It answers 200 once the
// connector holds a connection to the edge, with a body such as
// {"status":200,"readyConnections":4,"connectorId":"..."}.
func (m *Manager) probe(ctx context.Context, addr string) (ready bool, connections int) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/ready", nil)
	if err != nil {
		return false, 0
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		m.log.Debug().Err(err).Str("addr", addr).Msg("connector did not answer /ready")
		return false, 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, 0
	}
	var body struct {
		ReadyConnections int `json:"readyConnections"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReadyBody)).Decode(&body); err != nil {
		return true, 0
	}
	return true, body.ReadyConnections
}
