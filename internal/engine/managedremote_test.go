package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

const farDigest = "0123456789abcdef0123456789abcdef01234567"

// guestReader is the Proxmox API as the configurer reads the guests of the
// other nodes through it: one configuration, its pending keys, or an error.
type guestReader struct {
	mu      sync.Mutex
	cfg     pve.GuestConfig
	pending []string
	err     error
	reads   int
}

func (r *guestReader) NextConfig(_ context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	if r.err != nil {
		return pve.GuestConfig{}, r.err
	}
	if node != "pve2" || ref.VMID != 104 {
		return pve.GuestConfig{}, fmt.Errorf("%s of node %s is not in this test", ref, node)
	}
	return pve.GuestConfig{Values: maps.Clone(r.cfg.Values), Digest: r.cfg.Digest}, nil
}

func (r *guestReader) PendingChanges(context.Context, string, int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pending), r.err
}

func (r *guestReader) set(f func(r *guestReader)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

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
	runner := &hostRunner{answers: map[string]string{}}
	api := &guestReader{cfg: pve.GuestConfig{Digest: farDigest, Values: map[string]string{"net0": "virtio=BC:24:11:00:04:01,bridge=vmbr1"}}}
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
	host.SetRemote(w.eng.PVEWriter(), api)
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
	require.False(t, runner.ran("pvesh "), "nor pvesh, which reaches another node through ssh")
	st := w.tick(5 * time.Second)
	require.Equal(t, AttachmentAttached, attachmentOf(st, mgd.Ref.String()).State, "a guest of a node with an agent has not migrated")
	require.Contains(t, w.eventMessages(), "the card net1 of qemu/104 (mgd) on the managed network is written by the agent of node pve2")

	api.set(func(r *guestReader) {
		r.cfg.Values["net1"] = fmt.Sprintf("virtio=%s,bridge=pconet,firewall=0", strings.ToUpper(lease.MAC))
	})
	w.acts["pve2"].edit = func(st *agentproto.Status) { st.Actions = []agentproto.ActionResult{{ID: id, OK: true}} }
	snap = w.inv.snapshot()
	for i := range snap.Guests {
		if snap.Guests[i].Ref == mgd.Ref {
			snap.Guests[i].NICs = append(snap.Guests[i].NICs, model.NIC{Index: 1, MAC: lease.MAC, Bridge: "pconet"})
		}
	}
	w.inv.set(snap)

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
	api := &guestReader{}
	snap := w.inv.snapshot()
	mgd := guest(105, "lone")
	mgd.Node, mgd.Tags = "pve3", []string{"cf-tunnel", managednet.Tag}
	snap.Guests = append(snap.Guests, mgd)
	w.inv.set(snap)
	w.eng = w.newEngine()
	host.SetRemote(w.eng.PVEWriter(), api)
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

// A configuration of a guest of another node that cannot be read is a
// problem of the card, which is tried again each cycle and written once it
// can be read; the attachment is not taken for a migration meanwhile.
func TestACardOfAGuestOfAnotherNodeIsTriedAgainAfterAFailedRead(t *testing.T) {
	w := threeNodes(t)
	facts := &managedFacts{
		registered: []string{"cf-tunnel", managednet.Tag},
		vnet:       ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true},
		cluster:    pve.ClusterFirewallOptions{Enabled: true},
		service:    "running",
		guestFW:    map[string]bool{},
		guestFWErr: map[string]error{},
	}
	api := &guestReader{err: errors.New("595 Errors during connection establishment"),
		cfg: pve.GuestConfig{Digest: farDigest, Values: map[string]string{"net0": "virtio=BC:24:11:00:04:01,bridge=vmbr1"}}}
	host := managednet.NewHostConfigurer(&hostRunner{answers: map[string]string{}}, testNode)
	w.guests, w.managed = host, facts.deps
	saveManagedNetwork(t, w.store, 1)
	snap := w.inv.snapshot()
	mgd := guest(104, "mgd")
	mgd.Node, mgd.Tags = "pve2", []string{"cf-tunnel", managednet.Tag}
	mgd.NICs = []model.NIC{{Index: 0, MAC: "bc:24:11:00:04:01", Bridge: "vmbr1"}}
	snap.Guests = append(snap.Guests, mgd)
	w.inv.set(snap)
	w.eng = w.newEngine()
	host.SetRemote(w.eng.PVEWriter(), api)
	w.watchHere(t)

	var st State
	for range 6 {
		st = w.tick(5 * time.Second)
	}

	att := attachmentOf(st, mgd.Ref.String())
	require.Equal(t, AttachmentUnverified, att.State)
	require.Contains(t, att.Detail, "595 Errors during connection establishment")
	s, _ := w.r.lastOf("pve2")
	require.Empty(t, s.Actions)
	before := api.reads
	require.Greater(t, before, 1, "the read is tried again")

	api.set(func(r *guestReader) { r.err = nil })
	for range 4 {
		w.tick(5 * time.Second)
		if s, _ := w.r.lastOf("pve2"); len(s.Actions) > 0 {
			require.Equal(t, agentproto.ActionPVEWrite, s.Actions[0].Kind)
			return
		}
	}
	t.Fatal("the card was not handed to the agent of pve2 once its configuration could be read")
}
