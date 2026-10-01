package cffake

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

type tunnel struct {
	cfapi.Tunnel
	account string
	version int // 0 until a configuration was written
	ingress []planner.IngressRule
}

func (f *Fake) account(id string) error {
	if !slices.ContainsFunc(f.accounts, func(a cfapi.Account) bool { return a.ID == id }) {
		return notFound("account", id)
	}
	return nil
}

// tunnelIn returns the tunnel of an account, and its place in the list.
func (f *Fake) tunnelIn(accountID, tunnelID string) (*tunnel, int, error) {
	if err := f.account(accountID); err != nil {
		return nil, 0, err
	}
	i := slices.IndexFunc(f.tunnels, func(t *tunnel) bool { return t.account == accountID && t.ID == tunnelID })
	if i < 0 {
		return nil, 0, notFound("tunnel", tunnelID)
	}
	return f.tunnels[i], i, nil
}

func (f *Fake) newTunnel(accountID, name string) *tunnel {
	f.tunnelSeq++
	t := &tunnel{
		Tunnel: cfapi.Tunnel{
			ID:        fmt.Sprintf("00000000-0000-4000-8000-%012d", f.tunnelSeq),
			Name:      name,
			Status:    "inactive",
			CreatedAt: f.now(),
		},
		account: accountID,
	}
	f.tunnels = append(f.tunnels, t)
	return t
}

func (f *Fake) FindTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "FindTunnel", accountID, name); err != nil {
		return cfapi.Tunnel{}, false, err
	}
	if err := blank("tunnel name", name); err != nil {
		return cfapi.Tunnel{}, false, err
	}
	if err := f.account(accountID); err != nil {
		return cfapi.Tunnel{}, false, err
	}
	var matches []cfapi.Tunnel
	for _, t := range f.tunnels {
		if t.account == accountID && t.Name == name {
			matches = append(matches, t.Tunnel)
		}
	}
	switch len(matches) {
	case 0:
		return cfapi.Tunnel{}, false, nil
	case 1:
		return matches[0], true, nil
	}
	return cfapi.Tunnel{}, false, fmt.Errorf("finding tunnel %q: %d tunnels have that name", name, len(matches))
}

func (f *Fake) CreateTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "CreateTunnel", accountID, name); err != nil {
		return cfapi.Tunnel{}, err
	}
	if err := blank("tunnel name", name); err != nil {
		return cfapi.Tunnel{}, err
	}
	if err := f.account(accountID); err != nil {
		return cfapi.Tunnel{}, err
	}
	if slices.ContainsFunc(f.tunnels, func(t *tunnel) bool { return t.account == accountID && t.Name == name }) {
		return cfapi.Tunnel{}, &cfapi.Error{
			Status: http.StatusConflict, Codes: []int{codeTunnelExists},
			Message: "tunnel with name already exists",
		}
	}
	return f.newTunnel(accountID, name).Tunnel, nil
}

func (f *Fake) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "DeleteTunnel", accountID, tunnelID); err != nil {
		return err
	}
	_, i, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return err
	}
	f.tunnels = slices.Delete(f.tunnels, i, i+1)
	return nil
}

func (f *Fake) TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "TunnelToken", accountID, tunnelID); err != nil {
		return "", err
	}
	t, _, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return "", err
	}
	return "token-" + t.ID, nil
}

func (f *Fake) TunnelConfig(ctx context.Context, accountID, tunnelID string) (cfapi.TunnelConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "TunnelConfig", accountID, tunnelID); err != nil {
		return cfapi.TunnelConfig{}, err
	}
	t, _, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return cfapi.TunnelConfig{}, err
	}
	return cfapi.TunnelConfig{Version: t.version, Ingress: clone(t.ingress)}, nil
}

// PutTunnelConfig stores the rules and counts a new version, also when they
// equal the ones before.
func (f *Fake) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []planner.IngressRule) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "PutTunnelConfig", accountID, tunnelID); err != nil {
		return 0, err
	}
	t, _, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return 0, err
	}
	t.ingress = clone(rules)
	t.version++
	return t.version, nil
}

// Connectors lists none: no cloudflared ever connects to a fake.
func (f *Fake) Connectors(ctx context.Context, accountID, tunnelID string) ([]cfapi.Connector, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "Connectors", accountID, tunnelID); err != nil {
		return nil, err
	}
	if _, _, err := f.tunnelIn(accountID, tunnelID); err != nil {
		return nil, err
	}
	return nil, nil
}
