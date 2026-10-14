package managednet_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// fakeRemote records what the configurer hands the agents of other nodes.
type fakeRemote struct {
	agents map[string]bool
	writes []agentproto.PVEWrite
	nodes  []string
	answer error
}

func (f *fakeRemote) Agent(node string) bool { return f.agents[node] }

func (f *fakeRemote) Write(_ context.Context, node string, w agentproto.PVEWrite) error {
	f.writes, f.nodes = append(f.writes, w), append(f.nodes, node)
	if f.answer != nil {
		return f.answer
	}
	return managednet.ErrSent
}

var (
	farVM = model.Guest{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 9340}, Name: "far", Node: "pve2", Running: true}
	farCT = model.Guest{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9341}, Name: "farct", Node: "pve2", Running: true}
)

const digest40 = "0123456789abcdef0123456789abcdef01234567"

// A guest of another node gets its card from the agent of its node: the
// configuration is read through the Proxmox API, which reaches every node
// without a shell on this one, and the change goes to the agent as a typed
// request under the digest read, never as a command. No pvesh runs here:
// the daemon's unit keeps root's home, and with it the keys pvesh proxies
// to another node with, out of reach.
func TestAGuestOfAnotherNodeIsChangedByItsAgent(t *testing.T) {
	api := &fakeAPI{configs: []pve.GuestConfig{guestConfig(digest40, map[string]string{"net0": "virtio=BC:24:11:00:00:09,bridge=vmbr0"})}}
	remote := &fakeRemote{agents: map[string]bool{"pve2": true}}
	h := managednet.NewHostConfigurer(run(t), thisNode)
	h.SetRemote(remote, api)

	live, err := h.AddNIC(t.Context(), farVM, 1, vmSpec)

	require.ErrorIs(t, err, managednet.ErrSent)
	require.False(t, live)
	off := false
	require.Equal(t, []agentproto.PVEWrite{{Guest: farVM.Ref, NIC: 1, Bridge: "pconet", MAC: "06:50:43:4f:76:06", Firewall: &off, Digest: digest40}},
		remote.writes)
	require.Equal(t, []string{"pve2"}, remote.nodes)

	t.Run("once the card is there, it is read live through the API", func(t *testing.T) {
		api := &fakeAPI{configs: []pve.GuestConfig{guestConfig(digest40, map[string]string{"net1": "virtio=06:50:43:4F:76:06,bridge=pconet,firewall=0"})}}
		h := managednet.NewHostConfigurer(run(t), thisNode)
		h.SetRemote(remote, api)
		live, err := h.AddNIC(t.Context(), farVM, 1, vmSpec)
		require.NoError(t, err)
		require.True(t, live)
		require.Equal(t, 1, api.asked, "the pending changes were read through the API")
	})

	t.Run("a container's card names its interface and address", func(t *testing.T) {
		api := &fakeAPI{configs: []pve.GuestConfig{guestConfig(digest40, map[string]string{"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp,type=veth"})}}
		remote := &fakeRemote{agents: map[string]bool{"pve2": true}}
		h := managednet.NewHostConfigurer(run(t), thisNode)
		h.SetRemote(remote, api)
		_, err := h.AddNIC(t.Context(), farCT, 1, ctSpec)
		require.ErrorIs(t, err, managednet.ErrSent)
		require.Equal(t, []agentproto.PVEWrite{{Guest: farCT.Ref, NIC: 1, Name: "eth1", Bridge: "pconet", MAC: "06:50:43:4f:76:05",
			IP: ctSpec.Addr, Firewall: &off, Digest: digest40}}, remote.writes)
	})

	t.Run("a removal names its card only", func(t *testing.T) {
		api := &fakeAPI{configs: []pve.GuestConfig{guestConfig(digest40, map[string]string{"net1": "virtio=06:50:43:4F:76:06,bridge=pconet,firewall=0"})}}
		remote := &fakeRemote{agents: map[string]bool{"pve2": true}}
		h := managednet.NewHostConfigurer(run(t), thisNode)
		h.SetRemote(remote, api)
		require.ErrorIs(t, h.RemoveNIC(t.Context(), farVM, 1), managednet.ErrSent)
		require.Equal(t, []agentproto.PVEWrite{{Guest: farVM.Ref, NIC: 1, Remove: true, Digest: digest40}}, remote.writes)
	})

	t.Run("an agent's refusal is the error", func(t *testing.T) {
		api := &fakeAPI{configs: []pve.GuestConfig{guestConfig(digest40, map[string]string{"net0": "virtio=BC:24:11:00:00:09,bridge=vmbr0"})}}
		remote := &fakeRemote{agents: map[string]bool{"pve2": true}, answer: errors.New("qm set: exit status 2")}
		h := managednet.NewHostConfigurer(run(t), thisNode)
		h.SetRemote(remote, api)
		_, err := h.AddNIC(t.Context(), farVM, 1, vmSpec)
		require.EqualError(t, err, "adding net1 to qemu/9340: qm set: exit status 2")
	})

	t.Run("a read that fails is the error, and nothing is written", func(t *testing.T) {
		remote := &fakeRemote{agents: map[string]bool{"pve2": true}}
		h := managednet.NewHostConfigurer(run(t), thisNode)
		h.SetRemote(remote, &fakeAPI{})
		_, err := h.AddNIC(t.Context(), farVM, 1, vmSpec)
		require.ErrorContains(t, err, "adding net1 to qemu/9340: reading the configuration")
		require.Empty(t, remote.writes)
	})
}

// A guest of a node without an agent is not changed, and that is why.
func TestAGuestOfANodeWithoutAnAgentIsNotChanged(t *testing.T) {
	h := managednet.NewHostConfigurer(run(t), thisNode)
	h.SetRemote(&fakeRemote{agents: map[string]bool{"pve3": true}}, &fakeAPI{})

	ok, why, err := h.CanWrite(t.Context(), farVM)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "the guest is on node pve2, which has no pco agent to change it", why)
	_, err = h.AddNIC(t.Context(), farVM, 1, vmSpec)
	require.EqualError(t, err, "adding net1 to qemu/9340: the guest is on node pve2, which has no pco agent to change it")
}
