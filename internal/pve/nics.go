package pve

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// NICs returns the network cards of a guest's configuration, the value of
// net<N> by N, and the digest a write to it is made under: those it runs
// with, and those whose addition waits for its next start, which hold their
// slots too.
func (c *Client) NICs(ctx context.Context, node string, ref model.GuestRef) (map[int]string, string, error) {
	cfg, err := c.GuestConfig(ctx, node, ref)
	if err != nil {
		return nil, "", err
	}
	pending, err := c.PendingConfig(ctx, node, ref)
	if err != nil {
		return nil, "", err
	}
	out := map[int]string{}
	for _, values := range []map[string]string{pending, cfg.Values} {
		for key, value := range values {
			if index, ok := nicIndex(key); ok {
				out[index] = value
			}
		}
	}
	return out, cfg.Digest, nil
}

// nicIndex reads the N of a key net<N>.
func nicIndex(key string) (int, bool) {
	digits, ok := strings.CutPrefix(key, "net")
	if !ok || digits == "" {
		return 0, false
	}
	index, err := strconv.Atoi(digits)
	return index, err == nil && index >= 0 && strconv.Itoa(index) == digits
}

// VLANAware reports whether a bridge of the node filters VLANs; a bridge the
// node does not have is an error.
func (c *Client) VLANAware(ctx context.Context, node, bridge string) (bool, error) {
	ifaces, err := c.NodeNetwork(ctx, node)
	if err != nil {
		return false, err
	}
	for _, ifc := range ifaces {
		if ifc.Name == bridge && ifc.Type == "bridge" {
			return ifc.VLANAware, nil
		}
	}
	return false, fmt.Errorf("bridge %s is no bridge of node %s", bridge, node)
}

// AddNIC sets net<index> of a guest's configuration to spec, under the
// digest; a stale digest is ErrStaleDigest.
func (c *Client) AddNIC(ctx context.Context, node string, ref model.GuestRef, index int, spec, digest string) error {
	return c.SetGuestConfig(ctx, node, ref, map[string]string{"net" + strconv.Itoa(index): spec}, digest)
}

// RemoveNIC deletes net<index> of a guest's configuration, under the digest;
// a stale digest is ErrStaleDigest.
func (c *Client) RemoveNIC(ctx context.Context, node string, ref model.GuestRef, index int, digest string) error {
	return c.DeleteGuestConfigKeys(ctx, node, ref, []string{"net" + strconv.Itoa(index)}, digest)
}
