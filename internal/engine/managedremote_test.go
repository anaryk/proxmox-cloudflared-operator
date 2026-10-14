package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// hostRunner answers the commands of the host configurer from a table, and
// keeps every command it was asked to run.
type hostRunner struct {
	mu      sync.Mutex
	answers map[string]string
	calls   []string
}

func (r *hostRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, cmd)
	out, ok := r.answers[cmd]
	if !ok {
		return "", fmt.Errorf("%s: not in this test", cmd)
	}
	return out, nil
}

func (r *hostRunner) answer(cmd, out string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answers[cmd] = out
}

func (r *hostRunner) ran(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

const (
	farDigest  = "0123456789abcdef0123456789abcdef01234567"
	farConfig  = "pvesh get /nodes/pve2/qemu/104/config --output-format json"
	farPending = "pvesh get /nodes/pve2/qemu/104/pending --output-format json"
)

// A consenting guest of another node is attached by the agent of its node:
// the controller leases its address, reads the guest's configuration through
// the API of the cluster and hands the agent a typed write of the card, under
// the digest it read; it runs no qm itself. Once the agent answered and the
// card is there, the write is gone from the snapshot and the card is live.
func TestAManagedGuestOfAnotherNodeIsAttachedByItsAgent(t *testing.T) {
	w := threeNodes(t)
	facts := &managedFacts{
		registered: []string{"cf-tunnel", managednet.Tag},
		vnet:       ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true},
		cluster:    pve.ClusterFirewallOptions{Enabled: true},
		service:    "running",
		guestFW:    map[string]bool{},
		guestFWErr: map[string]error{},
	}
	runner := &hostRunner{answers: map[string]string{
		farConfig: `{"digest":"` + farDigest + `","net0":"virtio=BC:24:11:00:04:01,bridge=vmbr1"}`,
	}}
	host := managednet.NewHostConfigurer(runner, testNode)
	w.guests, w.managed = host, facts.deps
	saveManagedNetwork(t, w.store, 1)
	snap := w.inv.snapshot()
	mgd := guest(104, "mgd")
	mgd.Node, mgd.Tags = "pve2", []string{"cf-tunnel", managednet.Tag}
	mgd.NICs = []model.NIC{{Index: 0, MAC: "bc:24:11:00:04:01", Bridge: "vmbr1"}}
	snap.Guests = append(snap.Guests, mgd)
	w.inv.set(snap)
	w.eng = w.newEngine()
	host.SetRemote(w.eng.PVEWriter())
	w.watchHere(t)

	var write agentproto.PVEWrite
	var id string
	for range 10 {
		w.tick(5 * time.Second)
		s, ok := w.r.lastOf("pve2")
		if ok && len(s.Actions) > 0 {
			require.Len(t, s.Actions, 1)
			require.Equal(t, agentproto.ActionPVEWrite, s.Actions[0].Kind)
			id = s.Actions[0].ID
			require.NoError(t, json.Unmarshal(s.Actions[0].Args, &write))
			require.True(t, slices.ContainsFunc(s.Guests, func(g model.Guest) bool { return g.Ref == mgd.Ref }),
				"the snapshot gives the agent the guest it writes, which has no route")
			break
		}
	}
	require.NotEmpty(t, id, "the agent of pve2 was handed the write")
	leases, err := w.store.Leases()
	require.NoError(t, err)
	require.Len(t, leases, 1)
	var lease store.Lease
	for _, l := range leases {
		lease = l
	}
	off := false
	require.Equal(t, agentproto.PVEWrite{Guest: mgd.Ref, NIC: 1, Bridge: "pconet", MAC: lease.MAC, Firewall: &off, Digest: farDigest}, write)
	require.False(t, runner.ran("qm "), "the controller runs no qm for a guest of another node")
	require.Contains(t, w.eventMessages(), "the card net1 of qemu/104 (mgd) on the managed network is written by the agent of node pve2")

	card := fmt.Sprintf(`"net1":"virtio=%s,bridge=pconet,firewall=0"`, strings.ToUpper(lease.MAC))
	runner.answer(farConfig, `{"digest":"`+farDigest+`","net0":"virtio=BC:24:11:00:04:01,bridge=vmbr1",`+card+`}`)
	runner.answer(farPending, `[{"key":"net1","value":"virtio=`+lease.MAC+`,bridge=pconet,firewall=0"}]`)
	w.acts["pve2"].edit = func(st *agentproto.Status) { st.Actions = []agentproto.ActionResult{{ID: id, OK: true}} }
	snap = w.inv.snapshot()
	for i := range snap.Guests {
		if snap.Guests[i].Ref == mgd.Ref {
			snap.Guests[i].NICs = append(snap.Guests[i].NICs, model.NIC{Index: 1, MAC: lease.MAC, Bridge: "pconet"})
		}
	}
	w.inv.set(snap)

	var st State
	for range 3 {
		st = w.tick(5 * time.Second)
	}

	s, _ := w.r.lastOf("pve2")
	require.Empty(t, s.Actions, "the answered write is no longer listed")
	att := attachmentOf(st, mgd.Ref.String())
	require.Equal(t, lease.Addr, att.Addr)
}

// A consenting guest of a node without an agent waits, and says so.
func TestAManagedGuestOfANodeWithoutAnAgentWaits(t *testing.T) {
	w := threeNodes(t)
	w.acts["pve3"].silent = true
	facts := &managedFacts{
		registered: []string{"cf-tunnel", managednet.Tag},
		vnet:       ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true},
		cluster:    pve.ClusterFirewallOptions{Enabled: true},
		service:    "running",
		guestFW:    map[string]bool{},
		guestFWErr: map[string]error{},
	}
	host := managednet.NewHostConfigurer(&hostRunner{answers: map[string]string{}}, testNode)
	w.guests, w.managed = host, facts.deps
	saveManagedNetwork(t, w.store, 2)
	snap := w.inv.snapshot()
	mgd := guest(105, "lone")
	mgd.Node, mgd.Tags = "pve3", []string{"cf-tunnel", managednet.Tag}
	snap.Guests = append(snap.Guests, mgd)
	w.inv.set(snap)
	w.eng = w.newEngine()
	host.SetRemote(w.eng.PVEWriter())
	w.watchHere(t)

	var st State
	for range 14 {
		st = w.tick(5 * time.Second)
	}

	require.Contains(t, st.Problems, "guest qemu/105 is on node pve3, which has no pco agent; its managed card waits")
	leases, err := w.store.Leases()
	require.NoError(t, err)
	require.Empty(t, leases)
}
