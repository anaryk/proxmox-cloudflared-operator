package managednet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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

// SetRemote has the configurer change the guests of the other nodes through
// their agents. It is called before the configurer is first used.
func (h *HostConfigurer) SetRemote(r Remote) {
	h.remote = r
	h.through = cards{cfg: remoteConfig{host: hostConfig{run: h.run}, remote: r}}
}

// remoteConfig is the configuration of a guest of another node: read with
// pvesh, which reaches every node of the cluster, and changed by the agent
// of the guest's node.
type remoteConfig struct {
	host   hostConfig
	remote Remote
}

func (c remoteConfig) read(ctx context.Context, g model.Guest) (map[string]string, string, error) {
	return c.host.read(ctx, g)
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

// pending reads the pending changes of a VM of another node through the API
// of the cluster: each key with a value waiting for a restart, or a deletion
// that waits.
func (c remoteConfig) pending(ctx context.Context, g model.Guest) ([]string, error) {
	path := fmt.Sprintf("/nodes/%s/qemu/%d/pending", g.Node, g.Ref.VMID)
	out, err := c.host.run.Run(ctx, "pvesh", "get", path, "--output-format", "json")
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Key     string          `json:"key"`
		Pending json.RawMessage `json:"pending"`
		Delete  json.RawMessage `json:"delete"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return nil, fmt.Errorf("reading the pending changes: %w", err)
	}
	var keys []string
	for _, e := range entries {
		if len(e.Pending) > 0 || len(e.Delete) > 0 {
			keys = append(keys, e.Key)
		}
	}
	return keys, nil
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
