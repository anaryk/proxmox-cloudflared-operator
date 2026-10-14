package managednet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// Runner runs a command on the host; the setup runner is one.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// HostConfigurer changes the cards of the guests of its node as root: it
// reads a configuration with pvesh and changes it with qm set or pct set
// under the digest it read (--digest), which refuses the change when the
// configuration changed meanwhile. qm and pct change the guests of the node
// they run on only: with a Remote, the guests of another node are changed by
// the agent of that node, from a typed request.
type HostConfigurer struct {
	node string
	run  Runner
	cards
	// remote and through change the guests of the other nodes; nil
	// without SetRemote.
	remote  Remote
	through cards
}

// NewHostConfigurer returns the configurer of the host profile, on node.
func NewHostConfigurer(run Runner, node string) *HostConfigurer {
	return &HostConfigurer{node: node, run: run, cards: cards{cfg: hostConfig{run: run}}}
}

// of is what changes the cards of g: root on this node, the agent of the
// guest's node through the remote, or nothing, and why.
func (h *HostConfigurer) of(g model.Guest) (cards, error) {
	if g.Node == h.node {
		return h.cards, nil
	}
	if h.remote != nil && h.remote.Agent(g.Node) {
		return h.through, nil
	}
	if h.remote != nil {
		return cards{}, fmt.Errorf("the guest is on node %s, which has no pco agent to change it", g.Node)
	}
	return cards{}, h.local(g)
}

func (h *HostConfigurer) AddNIC(ctx context.Context, g model.Guest, index int, spec NICSpec) (bool, error) {
	live := false
	on, err := h.of(g)
	if err == nil {
		live, err = on.add(ctx, g, index, spec)
	}
	if err != nil {
		return false, fmt.Errorf("adding net%d to %s: %w", index, g.Ref, err)
	}
	return live, nil
}

func (h *HostConfigurer) RemoveNIC(ctx context.Context, g model.Guest, index int) error {
	on, err := h.of(g)
	if err == nil {
		err = on.removeNIC(ctx, g, index)
	}
	if err != nil {
		return fmt.Errorf("removing net%d from %s: %w", index, g.Ref, err)
	}
	return nil
}

func (h *HostConfigurer) SetLinkDown(ctx context.Context, g model.Guest, index int) error {
	on, err := h.of(g)
	if err == nil {
		err = on.linkDown(ctx, g, index)
	}
	if err != nil {
		return fmt.Errorf("setting the link of net%d of %s down: %w", index, g.Ref, err)
	}
	return nil
}

func (h *HostConfigurer) SetLinkUp(ctx context.Context, g model.Guest, index int) error {
	on, err := h.of(g)
	if err == nil {
		err = on.linkUp(ctx, g, index)
	}
	if err != nil {
		return fmt.Errorf("setting the link of net%d of %s up: %w", index, g.Ref, err)
	}
	return nil
}

// CanWrite says yes for a guest on this node: root may change every guest
// there, and qm and pct reach no other; and for a guest of another node whose
// agent changes it.
func (h *HostConfigurer) CanWrite(_ context.Context, g model.Guest) (bool, string, error) {
	if _, err := h.of(g); err != nil {
		return false, err.Error(), nil
	}
	return true, "", nil
}

// local refuses a guest of another node.
func (h *HostConfigurer) local(g model.Guest) error {
	if g.Node != h.node {
		return fmt.Errorf("the guest is on node %s, and pco changes the guests of %s only", g.Node, h.node)
	}
	return nil
}

// hostConfig is a guest's configuration as root reads and writes it on the
// host.
type hostConfig struct{ run Runner }

func (c hostConfig) read(ctx context.Context, g model.Guest) (map[string]string, string, error) {
	path := fmt.Sprintf("/nodes/%s/%s/%d/config", g.Node, g.Ref.Kind, g.Ref.VMID)
	out, err := c.run.Run(ctx, "pvesh", "get", path, "--output-format", "json")
	if err != nil {
		return nil, "", err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, "", err
	}
	values := make(map[string]string, len(raw))
	for key, value := range raw {
		if text, ok := text(value); ok {
			values[key] = text
		}
	}
	digest := values["digest"]
	delete(values, "digest")
	return values, digest, nil
}

func (c hostConfig) set(ctx context.Context, g model.Guest, key, value, digest string) error {
	return c.write(ctx, g, "--"+key, value, "--digest", digest)
}

func (c hostConfig) remove(ctx context.Context, g model.Guest, key, digest string) error {
	return c.write(ctx, g, "--delete", key, "--digest", digest)
}

// write runs qm set or pct set; the error of a stale digest is
// pve.ErrStaleDigest.
func (c hostConfig) write(ctx context.Context, g model.Guest, args ...string) error {
	_, err := c.run.Run(ctx, tool(g), append([]string{"set", strconv.Itoa(g.Ref.VMID)}, args...)...)
	if err != nil && pve.SaysStaleDigest(err.Error()) {
		return fmt.Errorf("%w: %w", pve.ErrStaleDigest, err)
	}
	return err
}

// pending reads qm pending, which has no JSON output: one line per key, as
// "<state> <key>: <value>", where the state is cur for a value the VM runs
// with, new for one that waits for a restart and del for a deletion that
// waits. Only the state and the key are read.
func (c hostConfig) pending(ctx context.Context, g model.Guest) ([]string, error) {
	out, err := c.run.Run(ctx, "qm", "pending", strconv.Itoa(g.Ref.VMID))
	if err != nil {
		return nil, err
	}
	var keys []string
	for line := range strings.Lines(out) {
		state, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		key, _, ok := strings.Cut(rest, ":")
		if ok && (state == "new" || state == "del") {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func tool(g model.Guest) string {
	if g.Ref.Kind == model.KindQEMU {
		return "qm"
	}
	return "pct"
}

// text returns a JSON value of a configuration as Proxmox writes it in the
// file: a string as it is, a number as its digits, a boolean as 1 or 0.
func text(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0, bytes.Equal(raw, []byte("null")):
		return "", false
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	case bytes.Equal(raw, []byte("true")):
		return "1", true
	case bytes.Equal(raw, []byte("false")):
		return "0", true
	case raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9':
		return string(raw), true
	}
	return "", false
}
