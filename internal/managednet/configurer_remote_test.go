package managednet_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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

const (
	digest40   = "0123456789abcdef0123456789abcdef01234567"
	farConfig  = "pvesh get /nodes/pve2/qemu/9340/config --output-format json"
	farPending = "pvesh get /nodes/pve2/qemu/9340/pending --output-format json"
)

// A guest of another node gets its card from the agent of its node: the
// configuration is read through the API of the cluster, and the change goes
// to the agent as a typed request under the digest read, never as a command.
func TestAGuestOfAnotherNodeIsChangedByItsAgent(t *testing.T) {
	r := run(t, step{cmd: farConfig, out: config(digest40, `"net0":"virtio=BC:24:11:00:00:09,bridge=vmbr0"`)})
	remote := &fakeRemote{agents: map[string]bool{"pve2": true}}
	h := managednet.NewHostConfigurer(r, thisNode)
	h.SetRemote(remote)

	live, err := h.AddNIC(t.Context(), farVM, 1, vmSpec)

	require.ErrorIs(t, err, managednet.ErrSent)
	require.False(t, live)
	off := false
	require.Equal(t, []agentproto.PVEWrite{{Guest: farVM.Ref, NIC: 1, Bridge: "pconet", MAC: "06:50:43:4f:76:06", Firewall: &off, Digest: digest40}},
		remote.writes)
	require.Equal(t, []string{"pve2"}, remote.nodes)

	t.Run("once the card is there, it is read live through the API", func(t *testing.T) {
		r := run(t,
			step{cmd: farConfig, out: config(digest40, `"net1":"virtio=06:50:43:4F:76:06,bridge=pconet,firewall=0"`)},
			step{cmd: farPending, out: `[{"key":"net1","value":"virtio=06:50:43:4F:76:06,bridge=pconet,firewall=0"}]`},
		)
		h := managednet.NewHostConfigurer(r, thisNode)
		h.SetRemote(remote)
		live, err := h.AddNIC(t.Context(), farVM, 1, vmSpec)
		require.NoError(t, err)
		require.True(t, live)
	})

	t.Run("a container's card names its interface and address", func(t *testing.T) {
		r := run(t, step{cmd: "pvesh get /nodes/pve2/lxc/9341/config --output-format json", out: config(digest40, ctNet0)})
		remote := &fakeRemote{agents: map[string]bool{"pve2": true}}
		h := managednet.NewHostConfigurer(r, thisNode)
		h.SetRemote(remote)
		_, err := h.AddNIC(t.Context(), farCT, 1, ctSpec)
		require.ErrorIs(t, err, managednet.ErrSent)
		require.Equal(t, []agentproto.PVEWrite{{Guest: farCT.Ref, NIC: 1, Name: "eth1", Bridge: "pconet", MAC: "06:50:43:4f:76:05",
			IP: ctSpec.Addr, Firewall: &off, Digest: digest40}}, remote.writes)
	})

	t.Run("a removal names its card only", func(t *testing.T) {
		r := run(t, step{cmd: farConfig, out: config(digest40, `"net1":"virtio=06:50:43:4F:76:06,bridge=pconet,firewall=0"`)})
		remote := &fakeRemote{agents: map[string]bool{"pve2": true}}
		h := managednet.NewHostConfigurer(r, thisNode)
		h.SetRemote(remote)
		require.ErrorIs(t, h.RemoveNIC(t.Context(), farVM, 1), managednet.ErrSent)
		require.Equal(t, []agentproto.PVEWrite{{Guest: farVM.Ref, NIC: 1, Remove: true, Digest: digest40}}, remote.writes)
	})

	t.Run("an agent's refusal is the error", func(t *testing.T) {
		r := run(t, step{cmd: farConfig, out: config(digest40, `"net0":"virtio=BC:24:11:00:00:09,bridge=vmbr0"`)})
		remote := &fakeRemote{agents: map[string]bool{"pve2": true}, answer: errors.New("qm set: exit status 2")}
		h := managednet.NewHostConfigurer(r, thisNode)
		h.SetRemote(remote)
		_, err := h.AddNIC(t.Context(), farVM, 1, vmSpec)
		require.EqualError(t, err, "adding net1 to qemu/9340: qm set: exit status 2")
	})
}

// A guest of a node without an agent is not changed, and that is why.
func TestAGuestOfANodeWithoutAnAgentIsNotChanged(t *testing.T) {
	h := managednet.NewHostConfigurer(run(t), thisNode)
	h.SetRemote(&fakeRemote{agents: map[string]bool{"pve3": true}})

	ok, why, err := h.CanWrite(t.Context(), farVM)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "the guest is on node pve2, which has no pco agent to change it", why)
	_, err = h.AddNIC(t.Context(), farVM, 1, vmSpec)
	require.EqualError(t, err, "adding net1 to qemu/9340: the guest is on node pve2, which has no pco agent to change it")
}
