package managednet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// ErrSent is the error of a change to a guest of another node, which was
// handed to the agent of that node: qm and pct change the guests of the node
// they run on only. The agent's answer comes with a later status, and the
// configuration read in a later cycle shows the change.
var ErrSent = errors.New("the change was handed to the agent of the guest's node")

// Remote is how the host configurer changes the cards of the guests of the
// other nodes of a cluster: through the agent of the guest's node, which
// builds the command itself from a typed request.
type Remote interface {
	// Agent says whether node has an admitted agent that writes the guests
	// of its node.
	Agent(node string) bool
	// Write hands w to the agent of node. It returns ErrSent once the
	// request waits for the agent, and the error the agent answered when
	// it refused or failed it.
	Write(ctx context.Context, node string, w agentproto.PVEWrite) error
}

// GuestReader is the part of the Proxmox API client the host configurer
// reads the guests of the other nodes with; *pve.Client is one.
type GuestReader interface {
	NextConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error)
	PendingChanges(ctx context.Context, node string, vmid int) ([]string, error)
}

// SetRemote has the configurer change the guests of the other nodes through
// their agents, reading them through api. It is called before the
// configurer is first used.
func (h *HostConfigurer) SetRemote(r Remote, api GuestReader) {
	h.remote = r
	h.through = cards{cfg: remoteConfig{api: api, remote: r}}
}

// remoteConfig is the configuration of a guest of another node: read
// through the Proxmox API, which reaches every node of the cluster, and
// changed by the agent of the guest's node. pvesh is not run for it: to
// reach another node pvesh goes through ssh as root, whose keys the
// daemon's unit keeps out of its reach.
type remoteConfig struct {
	api    GuestReader
	remote Remote
}

func (c remoteConfig) read(ctx context.Context, g model.Guest) (map[string]string, string, error) {
	cfg, err := c.api.NextConfig(ctx, g.Node, g.Ref)
	if err != nil {
		return nil, "", err
	}
	return cfg.Values, cfg.Digest, nil
}

func (c remoteConfig) set(ctx context.Context, g model.Guest, key, value, digest string) error {
	w, err := cardWrite(g, key, value, digest)
	if err != nil {
		return err
	}
	return c.remote.Write(ctx, g.Node, w)
}

func (c remoteConfig) remove(ctx context.Context, g model.Guest, key, digest string) error {
	index, err := strconv.Atoi(strings.TrimPrefix(key, "net"))
	if err != nil {
		return fmt.Errorf("%s is no card", key)
	}
	return c.remote.Write(ctx, g.Node, agentproto.PVEWrite{Guest: g.Ref, NIC: index, Remove: true, Digest: digest})
}

// pending reads the pending changes of a VM of another node through the
// API: each key with a value waiting for a restart, or a deletion that
// waits.
func (c remoteConfig) pending(ctx context.Context, g model.Guest) ([]string, error) {
	return c.api.PendingChanges(ctx, g.Node, g.Ref.VMID)
}

// cardWrite is the typed request that sets key of g to value, a card as pco
// writes it: a VM's virtio card, or a container's veth card, with the options
// pco sets and nothing else. A value with any other option is refused: the
// agent writes only what the request can name.
func cardWrite(g model.Guest, key, value, digest string) (agentproto.PVEWrite, error) {
	index, err := strconv.Atoi(strings.TrimPrefix(key, "net"))
	if err != nil || !strings.HasPrefix(key, "net") {
		return agentproto.PVEWrite{}, fmt.Errorf("%s is no card", key)
	}
	w := agentproto.PVEWrite{Guest: g.Ref, NIC: index, Digest: digest}
	for i, part := range strings.Split(value, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch {
		case i == 0 && g.Ref.Kind == model.KindQEMU:
			if k != "virtio" {
				return agentproto.PVEWrite{}, fmt.Errorf("%s of %s is a %s card; the agent of its node writes virtio cards only", key, g.Ref, k)
			}
			w.MAC = v
		case k == "bridge":
			w.Bridge = v
		case k == "tag":
			if w.Tag, err = strconv.Atoi(v); err != nil {
				return agentproto.PVEWrite{}, fmt.Errorf("%s of %s: tag %q", key, g.Ref, v)
			}
		case k == "firewall":
			on := isTrue(v)
			w.Firewall = &on
		case k == "link_down":
			if isTrue(v) {
				down := true
				w.LinkDown = &down
			}
		case g.Ref.Kind == model.KindLXC && k == "name":
			w.Name = v
		case g.Ref.Kind == model.KindLXC && k == "hwaddr":
			w.MAC = v
		case g.Ref.Kind == model.KindLXC && k == "ip":
			if w.IP, err = netip.ParsePrefix(v); err != nil {
				return agentproto.PVEWrite{}, fmt.Errorf("%s of %s: address %q", key, g.Ref, v)
			}
		default:
			return agentproto.PVEWrite{}, fmt.Errorf("%s of %s has the option %s, which the agent of its node does not write", key, g.Ref, k)
		}
	}
	if mac, err := net.ParseMAC(w.MAC); err == nil {
		w.MAC = mac.String()
	}
	return w, nil
}
