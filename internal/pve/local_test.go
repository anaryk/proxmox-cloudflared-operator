package pve

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

type fakeRunner struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	return f.out[key], f.err[key]
}

func TestALocalClientReadsTheNodesOwnWithPvesh(t *testing.T) {
	run := &fakeRunner{out: map[string]string{
		"pvesh get /nodes/pve1/lxc/101/config --output-format json --current 1": `{"net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01","digest":"abc"}`,
		"pvesh get /nodes/pve1/lxc/101/interfaces --output-format json":         `[{"name":"eth0","hwaddr":"bc:24:11:00:00:01","inet":"10.0.0.5/24"}]`,
	}}
	c, err := NewLocal(run, "pve1")
	require.NoError(t, err)

	cfg, err := c.GuestConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindLXC, VMID: 101})
	require.NoError(t, err)
	require.Equal(t, "abc", cfg.Digest)
	require.Contains(t, cfg.Values["net0"], "bridge=vmbr0")

	ifaces, err := c.LXCInterfaces(context.Background(), "pve1", 101)
	require.NoError(t, err)
	require.Len(t, ifaces, 1)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.5")}, ifaces[0].Addrs)
}

func TestALocalClientSaysWhatPveshSaid(t *testing.T) {
	run := &fakeRunner{err: map[string]error{
		"pvesh get /nodes/pve1/qemu/110/agent/network-get-interfaces --output-format json": errors.New("pvesh: exit status 255: No QEMU guest agent configured"),
		"pvesh get /nodes/pve1/network --output-format json":                               errors.New("pvesh: exit status 2: ipcc_send_rec failed"),
	}}
	c, err := NewLocal(run, "pve1")
	require.NoError(t, err)

	_, err = c.AgentInterfaces(context.Background(), "pve1", 110)
	require.ErrorIs(t, err, ErrAgentUnavailable)

	_, err = c.NodeNetwork(context.Background(), "pve1")
	require.ErrorContains(t, err, "ipcc_send_rec failed")
	require.False(t, Unreachable(err), "pvesh answered")
}

func TestALocalClientReadsNothingOfAnotherNode(t *testing.T) {
	run := &fakeRunner{}
	c, err := NewLocal(run, "pve1")
	require.NoError(t, err)

	_, err = c.NodeNetwork(context.Background(), "pve2")
	require.ErrorContains(t, err, "not of this node")
	_, err = c.ClusterStatus(context.Background())
	require.ErrorContains(t, err, "not of this node")
	require.Empty(t, run.calls)

	_, err = NewLocal(run, "../pve1")
	require.Error(t, err)
}

func TestUnreachable(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"pveproxy could not reach the node", fmt.Errorf("fetching: %w", &APIError{Status: 595, Message: "Connection refused"}), true},
		{"a gateway", &APIError{Status: 502, Message: "Bad Gateway"}, true},
		{"no connection", errors.New("dial tcp 127.0.0.1:8006: connect: connection refused"), true},
		{"a rejected token", &APIError{Status: 401, Message: "invalid token"}, false},
		{"an answer of the handler", &APIError{Status: 500, Message: "does not exist"}, false},
		{"the caller gave up", fmt.Errorf("fetching: %w", context.Canceled), false},
	} {
		t.Run(tt.name, func(t *testing.T) { require.Equal(t, tt.want, Unreachable(tt.err)) })
	}
}
