package cffake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

type tunnel struct {
	cfapi.Tunnel
	account    string
	version    int // 0 until a configuration was written
	ingress    []planner.IngressRule
	foreign    bool // settings in the configuration that IngressRule cannot show
	connectors []cfapi.Connector
}

// checkTunnelName checks the arguments of the calls that take an account and a
// tunnel name, the way the client does before it sends anything.
func checkTunnelName(accountID, name string) error {
	if err := cfapi.CheckID("account id", accountID); err != nil {
		return err
	}
	return cfapi.CheckName("tunnel name", name)
}

// checkTunnelID checks the arguments of the calls that take an account and a
// tunnel id.
func checkTunnelID(accountID, tunnelID string) error {
	if err := cfapi.CheckID("account id", accountID); err != nil {
		return err
	}
	return cfapi.CheckID("tunnel id", tunnelID)
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

// SetForeign says whether the configuration of a tunnel holds settings that
// TunnelConfig reports as Foreign, such as a path on a rule. A successful
// PutTunnelConfig clears it, as writing the configuration replaces those
// settings. An unknown tunnel is ignored.
func (f *Fake) SetForeign(accountID, tunnelID string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, _, err := f.tunnelIn(accountID, tunnelID); err == nil {
		t.foreign = v
	}
}

// SetConnectors sets what Connectors lists for a tunnel; none by default. The
// status of the tunnel follows: "healthy" with connectors, "inactive" without.
// A tunnel with connectors cannot be deleted. An unknown tunnel is ignored.
func (f *Fake) SetConnectors(accountID, tunnelID string, c []cfapi.Connector) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, _, err := f.tunnelIn(accountID, tunnelID); err == nil {
		t.connectors = clone(c)
		t.Status = "inactive"
		if len(c) > 0 {
			t.Status = "healthy"
		}
	}
}

func (f *Fake) FindTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, bool, error) {
	if err := checkTunnelName(accountID, name); err != nil {
		return cfapi.Tunnel{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "FindTunnel", accountID, name); err != nil {
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

// Tunnels lists the tunnels of an account whose name starts with namePrefix,
// which is matched with regard to case, oldest first.
func (f *Fake) Tunnels(ctx context.Context, accountID, namePrefix string) ([]cfapi.Tunnel, error) {
	if err := cfapi.CheckID("account id", accountID); err != nil {
		return nil, err
	}
	if err := cfapi.CheckName("tunnel name prefix", namePrefix); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "Tunnels", accountID, namePrefix); err != nil {
		return nil, err
	}
	if err := f.account(accountID); err != nil {
		return nil, err
	}
	var out []cfapi.Tunnel
	for _, t := range f.tunnels {
		if t.account == accountID && strings.HasPrefix(t.Name, namePrefix) {
			out = append(out, t.Tunnel)
		}
	}
	return out, nil
}

func (f *Fake) CreateTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, error) {
	if err := checkTunnelName(accountID, name); err != nil {
		return cfapi.Tunnel{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "CreateTunnel", accountID, name); err != nil {
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
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "DeleteTunnel", accountID, tunnelID); err != nil {
		return err
	}
	t, i, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return err
	}
	if len(t.connectors) > 0 {
		return &cfapi.Error{Status: http.StatusBadRequest, Message: "Cannot delete a tunnel that has active connections"}
	}
	f.tunnels = slices.Delete(f.tunnels, i, i+1)
	return nil
}

func (f *Fake) TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return "", err
	}
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
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return cfapi.TunnelConfig{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "TunnelConfig", accountID, tunnelID); err != nil {
		return cfapi.TunnelConfig{}, err
	}
	t, _, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return cfapi.TunnelConfig{}, err
	}
	return cfapi.TunnelConfig{Version: t.version, Ingress: clone(t.ingress), Foreign: t.foreign}, nil
}

// PutTunnelConfig refuses what Cloudflare refuses, with a 400: no rules, a rule
// without a service, a last rule that has a hostname, and a rule without a
// hostname before the last. Otherwise it stores the rules and counts a new
// version, also when they equal the ones before, and drops any foreign
// settings.
func (f *Fake) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []planner.IngressRule) (int, error) {
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "PutTunnelConfig", accountID, tunnelID); err != nil {
		return 0, err
	}
	t, _, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return 0, err
	}
	if err := checkIngress(rules); err != nil {
		return 0, err
	}
	t.ingress = clone(rules)
	t.foreign = false
	t.version++
	return t.version, nil
}

// checkIngress returns the 400 Cloudflare answers to an ingress it does not
// accept, which is one that cloudflared could not run.
func checkIngress(rules []planner.IngressRule) error {
	if len(rules) == 0 {
		return badIngress("the ingress has no rules; it must end with a rule that matches all URLs")
	}
	for i, r := range rules {
		last := i == len(rules)-1
		switch {
		case r.Service == "":
			return badIngress(fmt.Sprintf("ingress rule #%d has no service", i+1))
		case last && r.Hostname != "":
			return badIngress("the last ingress rule must match all URLs (it must not have a hostname)")
		case !last && r.Hostname == "":
			return badIngress(fmt.Sprintf("ingress rule #%d matches all URLs but is not the last one", i+1))
		}
	}
	return nil
}

func badIngress(msg string) error {
	return &cfapi.Error{Status: http.StatusBadRequest, Message: msg}
}

// Connectors lists what SetConnectors set; none by default.
func (f *Fake) Connectors(ctx context.Context, accountID, tunnelID string) ([]cfapi.Connector, error) {
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, "Connectors", accountID, tunnelID); err != nil {
		return nil, err
	}
	t, _, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return nil, err
	}
	return clone(t.connectors), nil
}
