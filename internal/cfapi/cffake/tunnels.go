package cffake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// runSecret is the secret of every tunnel the fake makes: 32 bytes, as long as
// Cloudflare's, and plainly not a real one.
const runSecret = "cffake-not-a-secret-0123456789ab"

type tunnel struct {
	cfapi.Tunnel
	account    string
	version    int // 0 until a configuration was written
	ingress    []planner.IngressRule
	foreign    bool // settings in the configuration that IngressRule cannot show
	connectors []cfapi.Connector
	deleted    bool // a tombstone: Cloudflare keeps the tunnel it deleted, and lists it
	deletedAt  time.Time
	secret     []byte // nil until it is rotated: runSecret
}

// listedTunnel is a tunnel of a listing, which holds the ones that were
// deleted as well.
type listedTunnel struct {
	cfapi.Tunnel
	DeletedAt *time.Time // nil for a tunnel that is not deleted
}

// deletedFilter is the is_deleted of a listing.
type deletedFilter int

const (
	liveAndDeleted deletedFilter = iota
	liveOnly
	deletedOnly
)

// tunnelFilter is what a listing of the tunnels of an account asks for: the
// exact name, or the prefix of a name, among the tunnels that are deleted or
// are not.
type tunnelFilter struct {
	name    string
	prefix  string
	deleted deletedFilter
}

func (flt tunnelFilter) matches(t *tunnel) bool {
	switch {
	case flt.deleted == liveOnly && t.deleted, flt.deleted == deletedOnly && !t.deleted:
		return false
	}
	return (flt.name == "" || t.Name == flt.name) && strings.HasPrefix(t.Name, flt.prefix)
}

// RunToken is the token the fake hands out for a tunnel to run it with: what
// cloudflared reads, the base64 of the JSON of the account tag, the tunnel id
// and the secret. The secret is the same for every tunnel and is no secret, so
// a cloudflared that is started with the token gets as far as the edge.
func RunToken(accountID, tunnelID string) string {
	return runToken(accountID, tunnelID, []byte(runSecret))
}

// RunTokenWith is the token the fake hands out for a tunnel whose secret was
// rotated to secret.
func RunTokenWith(accountID, tunnelID string, secret []byte) string {
	return runToken(accountID, tunnelID, secret)
}

func runToken(accountID, tunnelID string, secret []byte) string {
	payload, _ := json.Marshal(struct {
		Account string `json:"a"`
		Secret  []byte `json:"s"`
		Tunnel  string `json:"t"`
	}{accountID, secret, tunnelID}) // cannot fail: only strings and bytes
	return base64.StdEncoding.EncodeToString(payload)
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

// tunnelIn returns the tunnel of an account that is not deleted. A deleted one
// is no tunnel for any call that names it.
func (f *Fake) tunnelIn(accountID, tunnelID string) (*tunnel, error) {
	if err := f.account(accountID); err != nil {
		return nil, err
	}
	i := slices.IndexFunc(f.tunnels, func(t *tunnel) bool { return t.account == accountID && t.ID == tunnelID && !t.deleted })
	if i < 0 {
		return nil, notFound("tunnel", tunnelID)
	}
	return f.tunnels[i], nil
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
	if t, err := f.tunnelIn(accountID, tunnelID); err == nil {
		t.foreign = v
	}
}

// SetConnectors sets what Connectors lists for a tunnel; none by default. The
// status of the tunnel follows: "healthy" with connectors, "inactive" without.
// A tunnel with connectors cannot be deleted. An unknown tunnel is ignored.
func (f *Fake) SetConnectors(accountID, tunnelID string, c []cfapi.Connector) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, err := f.tunnelIn(accountID, tunnelID); err == nil {
		t.connectors = clone(c)
		t.Status = "inactive"
		if len(c) > 0 {
			t.Status = "healthy"
		}
	}
}

// matching returns the tunnels of an account that pass the filter, oldest
// first, and how many tunnels the account has, deleted ones included: the
// total of a listing is about the account, not about the filter. The lock must
// be held.
func (f *Fake) matching(accountID string, flt tunnelFilter) (matched []listedTunnel, total int) {
	for _, t := range f.tunnels {
		if t.account != accountID {
			continue
		}
		total++
		if !flt.matches(t) {
			continue
		}
		listed := listedTunnel{Tunnel: t.Tunnel}
		if t.deleted {
			deletedAt := t.deletedAt
			listed.DeletedAt = &deletedAt
		}
		matched = append(matched, listed)
	}
	return matched, total
}

// tunnelListing is what every listing of the tunnels of an account does once
// its arguments are checked: it is a call of the operation, of the method
// named, and the account must be known. The calls that list for the client
// pass the filter for tunnels that are not deleted; the handler lists with
// what the request asked for.
func (f *Fake) tunnelListing(ctx context.Context, method, accountID, arg string, flt tunnelFilter) (matched []listedTunnel, total int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelRead, method, accountID, arg); err != nil {
		return nil, 0, err
	}
	if err := f.account(accountID); err != nil {
		return nil, 0, err
	}
	matched, total = f.matching(accountID, flt)
	return matched, total, nil
}

func (f *Fake) FindTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, bool, error) {
	if err := checkTunnelName(accountID, name); err != nil {
		return cfapi.Tunnel{}, false, err
	}
	matches, _, err := f.tunnelListing(ctx, "FindTunnel", accountID, name, tunnelFilter{name: name, deleted: liveOnly})
	if err != nil {
		return cfapi.Tunnel{}, false, err
	}
	switch len(matches) {
	case 0:
		return cfapi.Tunnel{}, false, nil
	case 1:
		return matches[0].Tunnel, true, nil
	}
	return cfapi.Tunnel{}, false, fmt.Errorf("finding tunnel %q: %d tunnels have that name", name, len(matches))
}

// Tunnels lists the tunnels of an account that are not deleted and whose name
// starts with namePrefix, which is matched with regard to case, oldest first.
func (f *Fake) Tunnels(ctx context.Context, accountID, namePrefix string) ([]cfapi.Tunnel, error) {
	if err := cfapi.CheckID("account id", accountID); err != nil {
		return nil, err
	}
	if err := cfapi.CheckName("tunnel name prefix", namePrefix); err != nil {
		return nil, err
	}
	matches, _, err := f.tunnelListing(ctx, "Tunnels", accountID, namePrefix, tunnelFilter{prefix: namePrefix, deleted: liveOnly})
	if err != nil {
		return nil, err
	}
	var out []cfapi.Tunnel
	for _, m := range matches {
		out = append(out, m.Tunnel)
	}
	return out, nil
}

// CreateTunnel makes a tunnel. A name that a tunnel that is not deleted has is
// taken; the name of a deleted one is free.
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
	if slices.ContainsFunc(f.tunnels, func(t *tunnel) bool { return t.account == accountID && t.Name == name && !t.deleted }) {
		return cfapi.Tunnel{}, &cfapi.Error{
			Status: http.StatusConflict, Codes: []int{codeTunnelExists},
			Message: "tunnel with name already exists",
		}
	}
	return f.newTunnel(accountID, name).Tunnel, nil
}

// DeleteTunnel deletes a tunnel the way Cloudflare does: the tunnel stays as a
// tombstone, with the time of its deletion, that the listings of the API show
// unless they ask for the tunnels that are not deleted. Nothing in cfapi.API
// shows it, DeletedTunnelsIn does.
func (f *Fake) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "DeleteTunnel", accountID, tunnelID); err != nil {
		return err
	}
	t, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return err
	}
	if len(t.connectors) > 0 {
		return &cfapi.Error{Status: http.StatusBadRequest, Message: "Cannot delete a tunnel that has active connections"}
	}
	t.deleted, t.deletedAt = true, f.now()
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
	t, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return "", err
	}
	if t.secret != nil {
		return runToken(accountID, t.ID, t.secret), nil
	}
	return RunToken(accountID, t.ID), nil
}

// RotateTunnelSecret gives the tunnel a new secret, which the tokens it hands
// out from then on are made of. The connectors it lists stay, as connected
// ones do at Cloudflare.
func (f *Fake) RotateTunnelSecret(ctx context.Context, accountID, tunnelID string, secret []byte) error {
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return err
	}
	if err := cfapi.CheckSecret(secret); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "RotateTunnelSecret", accountID, tunnelID); err != nil {
		return err
	}
	t, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return err
	}
	t.secret = slices.Clone(secret)
	return nil
}

// liveTunnel returns a tunnel of an account that is not deleted, as it is now.
func (f *Fake) liveTunnel(accountID, tunnelID string) (cfapi.Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return cfapi.Tunnel{}, err
	}
	return t.Tunnel, nil
}

// CleanUpConnections drops the connectors of a tunnel, which is inactive
// then.
func (f *Fake) CleanUpConnections(ctx context.Context, accountID, tunnelID string) error {
	if err := checkTunnelID(accountID, tunnelID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opTunnelWrite, "CleanUpConnections", accountID, tunnelID); err != nil {
		return err
	}
	t, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return err
	}
	t.connectors, t.Status = nil, "inactive"
	return nil
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
	t, err := f.tunnelIn(accountID, tunnelID)
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
	t, err := f.tunnelIn(accountID, tunnelID)
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
	t, err := f.tunnelIn(accountID, tunnelID)
	if err != nil {
		return nil, err
	}
	return clone(t.connectors), nil
}
