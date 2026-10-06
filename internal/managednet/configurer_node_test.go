//go:build linux

package managednet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// The tests in this file change real guests of a Proxmox VE node, as root,
// with the host configurer: they run only where PCO_NODE_LXC names a running
// container and PCO_NODE_QEMU a running VM whose cards they may change, on a
// node with a bridge named pconet. PCO_NODE_QEMU_HOTPLUG says whether that
// VM's hotplug includes network (1) or not (0, the default). Each test adds
// net1 (PCO_NODE_INDEX to take another), and removes it again whatever
// happens. The times measured are logged.

// liveWithin is how long a card may take to work inside a running guest.
const liveWithin = 3 * time.Second

// execRunner runs commands as the host runner does, with what they wrote to
// stderr in the error.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// nodeGuest returns the guest an environment variable names on this node, or
// skips the test.
func nodeGuest(t *testing.T, env string, kind model.GuestKind) (model.Guest, int) {
	t.Helper()
	vmid, err := strconv.Atoi(os.Getenv(env))
	if err != nil {
		t.Skipf("%s names no guest to change", env)
	}
	host, err := os.Hostname()
	require.NoError(t, err)
	node, _, _ := strings.Cut(host, ".")
	index := 1
	if s := os.Getenv("PCO_NODE_INDEX"); s != "" {
		index, err = strconv.Atoi(s)
		require.NoError(t, err)
	}
	g := model.Guest{Ref: model.GuestRef{Kind: kind, VMID: vmid}, Node: node, Running: true}
	tool := map[model.GuestKind]string{model.KindQEMU: "qm", model.KindLXC: "pct"}[kind]
	// A card there before the test is the admin's, and never removed by it.
	require.Empty(t, nodeConfig(t.Context(), t, g)["net"+strconv.Itoa(index)], "net%d of %s must be free", index, g.Ref)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if values := nodeConfig(ctx, t, g); values["net"+strconv.Itoa(index)] != "" {
			_, err := execRunner{}.Run(ctx, tool, "set", strconv.Itoa(vmid), "--delete", "net"+strconv.Itoa(index))
			require.NoError(t, err)
		}
	})
	return g, index
}

// nodeConfig reads the configuration of g with its pending changes.
func nodeConfig(ctx context.Context, t *testing.T, g model.Guest) map[string]string {
	t.Helper()
	out, err := execRunner{}.Run(ctx, "pvesh", "get",
		fmt.Sprintf("/nodes/%s/%s/%d/config", g.Node, g.Ref.Kind, g.Ref.VMID), "--output-format", "json")
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &raw))
	values := map[string]string{}
	for k, v := range raw {
		values[k] = fmt.Sprint(v)
	}
	return values
}

// within polls until ok, for at most limit, and returns how long it took.
func within(t *testing.T, limit time.Duration, ok func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !ok() {
		if time.Since(start) > limit {
			t.Fatalf("not within %s", limit)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return time.Since(start)
}

// requireStaleRefused changes the description of g and back, and checks that
// a change under the digest read before is refused.
func requireStaleRefused(t *testing.T, g model.Guest, key, value string) {
	t.Helper()
	before := nodeConfig(t.Context(), t, g)
	tool := map[model.GuestKind]string{model.KindQEMU: "qm", model.KindLXC: "pct"}[g.Ref.Kind]
	vmid := strconv.Itoa(g.Ref.VMID)
	r := execRunner{}
	_, err := r.Run(t.Context(), tool, "set", vmid, "--description", before["description"]+"\npco digest check")
	require.NoError(t, err)
	t.Cleanup(func() {
		args := []string{"set", vmid, "--description", before["description"]}
		if before["description"] == "" {
			args = []string{"set", vmid, "--delete", "description"}
		}
		_, err := r.Run(context.Background(), tool, args...)
		require.NoError(t, err)
	})
	_, err = r.Run(t.Context(), tool, "set", vmid, "--"+key, value, "--digest", before["digest"])
	require.ErrorContains(t, err, "detected modified configuration", "a stale digest is refused")
	require.Empty(t, nodeConfig(t.Context(), t, g)[key], "nothing was written")
}

func TestNodeContainerCardLive(t *testing.T) {
	g, index := nodeGuest(t, "PCO_NODE_LXC", model.KindLXC)
	c := managednet.NewHostConfigurer(execRunner{}, g.Node)
	key := "net" + strconv.Itoa(index)
	spec := managednet.NICSpec{MAC: "06:50:43:4f:76:05", Bridge: "pconet", Addr: netip.MustParsePrefix("10.203.118.5/24")}

	requireStaleRefused(t, g, key, "name=eth9,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24,firewall=0")

	inside := func() string {
		out, err := execRunner{}.Run(t.Context(), "pct", "exec", strconv.Itoa(g.Ref.VMID), "--", "ip", "-4", "-o", "addr", "show")
		require.NoError(t, err)
		return out
	}
	start := time.Now()
	live, err := c.AddNIC(t.Context(), g, index, spec)
	require.NoError(t, err)
	require.True(t, live)
	added := time.Since(start)
	up := within(t, liveWithin, func() bool { return strings.Contains(inside(), "10.203.118.5/24") })
	t.Logf("pct set took %s; the address was up inside %s after it returned (%s in all); %s is %s",
		added, up, added+up, key, nodeConfig(t.Context(), t, g)[key])
	require.LessOrEqual(t, added+up, liveWithin)

	require.NoError(t, c.SetLinkDown(t.Context(), g, index))
	require.Contains(t, nodeConfig(t.Context(), t, g)[key], "link_down=1")

	start = time.Now()
	require.NoError(t, c.RemoveNIC(t.Context(), g, index))
	removed := time.Since(start)
	gone := within(t, liveWithin, func() bool { return !strings.Contains(inside(), "10.203.118.5/24") })
	t.Logf("pct set --delete took %s; the address was gone inside %s after it returned", removed, gone)
	require.Empty(t, nodeConfig(t.Context(), t, g)[key])
}

func TestNodeVMCard(t *testing.T) {
	g, index := nodeGuest(t, "PCO_NODE_QEMU", model.KindQEMU)
	hotplug := os.Getenv("PCO_NODE_QEMU_HOTPLUG") == "1"
	c := managednet.NewHostConfigurer(execRunner{}, g.Node)
	key := "net" + strconv.Itoa(index)
	spec := managednet.NICSpec{MAC: "06:50:43:4f:76:06", Bridge: "pconet"}

	requireStaleRefused(t, g, key, "virtio=06:50:43:4f:76:06,bridge=pconet,firewall=0")

	pending := func() string {
		out, err := execRunner{}.Run(t.Context(), "qm", "pending", strconv.Itoa(g.Ref.VMID))
		require.NoError(t, err)
		return out
	}
	start := time.Now()
	live, err := c.AddNIC(t.Context(), g, index, spec)
	require.NoError(t, err)
	t.Logf("qm set and qm pending took %s; live %v; qm pending:\n%s", time.Since(start), live, pending())
	require.Equal(t, hotplug, live, "the pending list tells whether the VM took the card")
	require.Contains(t, nodeConfig(t.Context(), t, g)[key], "06:50:43:4f:76:06")

	err = c.SetLinkDown(t.Context(), g, index)
	if hotplug {
		require.NoError(t, err)
	} else {
		require.ErrorIs(t, err, managednet.ErrPending, "link_down waits with the card")
	}
	require.Contains(t, nodeConfig(t.Context(), t, g)[key], "link_down=1")

	start = time.Now()
	require.NoError(t, c.RemoveNIC(t.Context(), g, index))
	t.Logf("qm set --delete took %s; qm pending:\n%s", time.Since(start), pending())
	require.Empty(t, nodeConfig(t.Context(), t, g)[key], "a card whose addition waited is gone from the pending changes too")
}
