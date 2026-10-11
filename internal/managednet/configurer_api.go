package managednet

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

const (
	// managedRole is the role the appliance's installer grants pco's user
	// and token on a guest whose cards pco may change.
	managedRole = "PCOManaged"
	networkPriv = "VM.Config.Network"
)

// GuestAPI is the part of the Proxmox API client the appliance's configurer
// reads and changes guests with; *pve.Client is one.
type GuestAPI interface {
	NextConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error)
	SetGuestConfig(ctx context.Context, node string, ref model.GuestRef, values map[string]string, digest string) error
	DeleteGuestConfigKeys(ctx context.Context, node string, ref model.GuestRef, keys []string, digest string) error
	PendingChanges(ctx context.Context, node string, vmid int) ([]string, error)
}

// APIConfigurer changes the cards of guests through the Proxmox API, as the
// appliance's token, under the digest of the configuration it read. The
// token may change the cards of a guest only where the admin granted it
// VM.Config.Network.
type APIConfigurer struct {
	access func(context.Context) (access.Data, error)
	token  string
	cards
}

// NewAPIConfigurer returns the configurer of the appliance profile. acl
// returns the access control as last read, without the pools of the guests,
// which come from each guest; token is the appliance's token,
// user@realm!name.
func NewAPIConfigurer(api GuestAPI, acl func(context.Context) (access.Data, error), token string) *APIConfigurer {
	return &APIConfigurer{access: acl, token: token, cards: cards{cfg: apiConfig{api: api}}}
}

func (c *APIConfigurer) AddNIC(ctx context.Context, g model.Guest, index int, spec NICSpec) (bool, error) {
	live, err := c.add(ctx, g, index, spec)
	if err != nil {
		return false, fmt.Errorf("adding net%d to %s: %w", index, g.Ref, err)
	}
	return live, nil
}

func (c *APIConfigurer) RemoveNIC(ctx context.Context, g model.Guest, index int) error {
	if err := c.removeNIC(ctx, g, index); err != nil {
		return fmt.Errorf("removing net%d from %s: %w", index, g.Ref, err)
	}
	return nil
}

func (c *APIConfigurer) SetLinkDown(ctx context.Context, g model.Guest, index int) error {
	if err := c.linkDown(ctx, g, index); err != nil {
		return fmt.Errorf("setting the link of net%d of %s down: %w", index, g.Ref, err)
	}
	return nil
}

// CanWrite says whether the token holds VM.Config.Network on the guest,
// granted on it or on its pool. When it does not, the reason is the pveum
// line that grants it to pco's user and the token, as both need it.
func (c *APIConfigurer) CanWrite(ctx context.Context, g model.Guest) (bool, string, error) {
	d, err := c.access(ctx)
	if err != nil {
		return false, "", fmt.Errorf("whether pco may change %s is not known: %w", g.Ref, err)
	}
	if g.Pool != "" {
		pools := maps.Clone(d.Pools)
		if pools == nil {
			pools = map[model.GuestRef]string{}
		}
		pools[g.Ref] = g.Pool
		d.Pools = pools
	}
	if slices.Contains(access.Privileges(d, c.token, "/vms/"+strconv.Itoa(g.Ref.VMID)), networkPriv) {
		return true, "", nil
	}
	user, _, _ := strings.Cut(c.token, "!")
	return false, fmt.Sprintf("pveum acl modify /vms/%d --users %s --tokens '%s' --roles %s", g.Ref.VMID, user, c.token, managedRole), nil
}

// apiConfig is a guest's configuration as the token reads and writes it.
type apiConfig struct{ api GuestAPI }

// read is one read of the configuration with its pending values, and the
// digest of that same read, which the write is made under.
func (c apiConfig) read(ctx context.Context, g model.Guest) (map[string]string, string, error) {
	cfg, err := c.api.NextConfig(ctx, g.Node, g.Ref)
	if err != nil {
		return nil, "", err
	}
	return cfg.Values, cfg.Digest, nil
}

func (c apiConfig) set(ctx context.Context, g model.Guest, key, value, digest string) error {
	return c.api.SetGuestConfig(ctx, g.Node, g.Ref, map[string]string{key: value}, digest)
}

func (c apiConfig) remove(ctx context.Context, g model.Guest, key, digest string) error {
	return c.api.DeleteGuestConfigKeys(ctx, g.Node, g.Ref, []string{key}, digest)
}

func (c apiConfig) pending(ctx context.Context, g model.Guest) ([]string, error) {
	return c.api.PendingChanges(ctx, g.Node, g.Ref.VMID)
}
