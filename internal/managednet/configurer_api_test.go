package managednet_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

const applianceToken = "pco@pve!vm900"

var (
	_ managednet.Configurer = (*managednet.APIConfigurer)(nil)
	_ managednet.GuestAPI   = (*pve.Client)(nil)
)

type write struct {
	node   string
	ref    model.GuestRef
	values map[string]string
	delete []string
	digest string
}

// fakeAPI answers the configuration reads in turn and records the writes.
type fakeAPI struct {
	configs []pve.GuestConfig // the pending views, in turn
	errs    []error           // the answers to the writes, in turn; nil when they run out
	writes  []write
	pending []string
	asked   int // how often the pending list was read
}

func (f *fakeAPI) NextConfig(_ context.Context, _ string, ref model.GuestRef) (pve.GuestConfig, error) {
	if len(f.configs) == 0 {
		return pve.GuestConfig{}, fmt.Errorf("fetching config of %s: no more answers", ref)
	}
	c := f.configs[0]
	f.configs = f.configs[1:]
	return c, nil
}

func (f *fakeAPI) answer() error {
	if len(f.errs) == 0 {
		return nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return err
}

func (f *fakeAPI) SetGuestConfig(_ context.Context, node string, ref model.GuestRef, values map[string]string, digest string) error {
	f.writes = append(f.writes, write{node: node, ref: ref, values: values, digest: digest})
	return f.answer()
}

func (f *fakeAPI) DeleteGuestConfigKeys(_ context.Context, node string, ref model.GuestRef, keys []string, digest string) error {
	f.writes = append(f.writes, write{node: node, ref: ref, delete: keys, digest: digest})
	return f.answer()
}

func (f *fakeAPI) PendingChanges(context.Context, string, int) ([]string, error) {
	f.asked++
	return f.pending, nil
}

func guestConfig(digest string, values map[string]string) pve.GuestConfig {
	return pve.GuestConfig{Values: values, Digest: digest}
}

var noAccess = func(context.Context) (access.Data, error) { return access.Data{}, nil }

func TestAPIAddNICToAContainer(t *testing.T) {
	api := &fakeAPI{configs: []pve.GuestConfig{guestConfig("d1", map[string]string{
		"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=dhcp", "hostname": "ct",
	})}}
	spec := ctSpec
	spec.Firewall = true
	live, err := managednet.NewAPIConfigurer(api, noAccess, applianceToken).AddNIC(t.Context(), container, 1, spec)
	require.NoError(t, err)
	require.True(t, live)
	require.Equal(t, []write{{
		node: thisNode, ref: container.Ref, digest: "d1",
		values: map[string]string{"net1": "name=eth1,bridge=pconet,hwaddr=06:50:43:4f:76:05,ip=10.203.118.5/24,firewall=1"},
	}}, api.writes)
	require.Zero(t, api.asked, "a container takes its card live")
}

func TestAPIAddNICToAVM(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pending []string
		live    bool
	}{
		{name: "with network hotplug", live: true},
		{name: "without network hotplug", pending: []string{"net1"}, live: false},
		{name: "another key pending", pending: []string{"memory"}, live: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{
				configs: []pve.GuestConfig{guestConfig("d1", map[string]string{"net0": "virtio=BC:24:11:00:00:02,bridge=vmbr0"})},
				pending: tt.pending,
			}
			spec := vmSpec
			spec.Firewall = true
			live, err := managednet.NewAPIConfigurer(api, noAccess, applianceToken).AddNIC(t.Context(), vm, 1, spec)
			require.NoError(t, err)
			require.Equal(t, tt.live, live)
			require.Equal(t, map[string]string{"net1": "virtio=06:50:43:4f:76:06,bridge=pconet,firewall=1"}, api.writes[0].values)
			require.Equal(t, "d1", api.writes[0].digest)
			require.Equal(t, 1, api.asked)
		})
	}
}

func TestAPIAddNICRetriesAStaleDigestOnce(t *testing.T) {
	staleErr := fmt.Errorf("changing the config of lxc/9320: %w", pve.ErrStaleDigest)
	cfg := map[string]string{"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01"}
	api := &fakeAPI{
		configs: []pve.GuestConfig{guestConfig("d1", cfg), guestConfig("d2", cfg)},
		errs:    []error{staleErr},
	}
	_, err := managednet.NewAPIConfigurer(api, noAccess, applianceToken).AddNIC(t.Context(), container, 1, ctSpec)
	require.NoError(t, err)
	require.Len(t, api.writes, 2)
	require.Equal(t, "d1", api.writes[0].digest)
	require.Equal(t, "d2", api.writes[1].digest)

	api = &fakeAPI{
		configs: []pve.GuestConfig{guestConfig("d1", cfg), guestConfig("d2", cfg)},
		errs:    []error{staleErr, staleErr},
	}
	_, err = managednet.NewAPIConfigurer(api, noAccess, applianceToken).AddNIC(t.Context(), container, 1, ctSpec)
	require.ErrorIs(t, err, pve.ErrStaleDigest)
	require.Len(t, api.writes, 2, "retried once only")
}

func TestAPIRemoveNIC(t *testing.T) {
	api := &fakeAPI{configs: []pve.GuestConfig{guestConfig("d1", map[string]string{
		"net0": "virtio=BC:24:11:00:00:02,bridge=vmbr0", "net1": "virtio=06:50:43:4f:76:06,bridge=pconet",
	})}}
	require.NoError(t, managednet.NewAPIConfigurer(api, noAccess, applianceToken).RemoveNIC(t.Context(), vm, 1))
	require.Equal(t, []write{{node: thisNode, ref: vm.Ref, delete: []string{"net1"}, digest: "d1"}}, api.writes)
}

// The configurer reads the pending view, in which a card whose addition
// waits for the restart of a VM is there although the VM does not run with
// it yet: it is removed from the pending changes like any other. A card whose
// deletion waits is gone from that view, and nothing more is asked of
// Proxmox.
func TestAPIRemoveNICReadsThePendingView(t *testing.T) {
	api := &fakeAPI{configs: []pve.GuestConfig{guestConfig("d1", map[string]string{
		"net0": "virtio=BC:24:11:00:00:02,bridge=vmbr0", "net1": "virtio=06:50:43:4f:76:06,bridge=pconet",
	})}}
	require.NoError(t, managednet.NewAPIConfigurer(api, noAccess, applianceToken).RemoveNIC(t.Context(), vm, 1))
	require.Equal(t, []write{{node: thisNode, ref: vm.Ref, delete: []string{"net1"}, digest: "d1"}}, api.writes)
	require.Empty(t, api.configs, "one read of the configuration")

	api = &fakeAPI{configs: []pve.GuestConfig{guestConfig("d1", map[string]string{"net0": "virtio=BC:24:11:00:00:02,bridge=vmbr0"})}}
	require.NoError(t, managednet.NewAPIConfigurer(api, noAccess, applianceToken).RemoveNIC(t.Context(), vm, 1))
	require.Empty(t, api.writes)
}

func TestAPISetLinkDown(t *testing.T) {
	cfg := map[string]string{"net1": "virtio=BC:24:11:AA:BB:CC,bridge=pconet,firewall=1"}
	api := &fakeAPI{configs: []pve.GuestConfig{guestConfig("d1", cfg)}}
	require.NoError(t, managednet.NewAPIConfigurer(api, noAccess, applianceToken).SetLinkDown(t.Context(), vm, 1))
	require.Equal(t, map[string]string{"net1": "virtio=BC:24:11:AA:BB:CC,bridge=pconet,firewall=1,link_down=1"}, api.writes[0].values)

	api = &fakeAPI{configs: []pve.GuestConfig{guestConfig("d1", cfg)}, pending: []string{"net1"}}
	err := managednet.NewAPIConfigurer(api, noAccess, applianceToken).SetLinkDown(t.Context(), vm, 1)
	require.ErrorIs(t, err, managednet.ErrPending)
}

// A card of the appliance goes on with its link down, and up once its
// firewall is enforced.
func TestAPIAddNICWithItsLinkDownAndSetItUp(t *testing.T) {
	api := &fakeAPI{configs: []pve.GuestConfig{guestConfig("d1", map[string]string{"net0": "virtio=BC:24:11:00:00:02,bridge=vmbr0"})}}
	spec := vmSpec
	spec.Firewall, spec.LinkDown = true, true
	_, err := managednet.NewAPIConfigurer(api, noAccess, applianceToken).AddNIC(t.Context(), vm, 1, spec)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"net1": "virtio=06:50:43:4f:76:06,bridge=pconet,firewall=1,link_down=1"}, api.writes[0].values)

	down := map[string]string{"net1": "virtio=06:50:43:4f:76:06,bridge=pconet,firewall=1,link_down=1"}
	api = &fakeAPI{configs: []pve.GuestConfig{guestConfig("d2", down)}}
	require.NoError(t, managednet.NewAPIConfigurer(api, noAccess, applianceToken).SetLinkUp(t.Context(), vm, 1))
	require.Equal(t, []write{{node: thisNode, ref: vm.Ref, digest: "d2", values: map[string]string{"net1": "virtio=06:50:43:4f:76:06,bridge=pconet,firewall=1"}}}, api.writes)

	api = &fakeAPI{configs: []pve.GuestConfig{guestConfig("d3", map[string]string{"net1": "virtio=06:50:43:4f:76:06,bridge=pconet,firewall=1"})}}
	require.NoError(t, managednet.NewAPIConfigurer(api, noAccess, applianceToken).SetLinkUp(t.Context(), vm, 1))
	require.Empty(t, api.writes, "a card that is up is left")

	api = &fakeAPI{configs: []pve.GuestConfig{guestConfig("d4", map[string]string{"net1": "virtio=06:50:43:4f:76:06,bridge=vmbr0,link_down=1"})}}
	require.ErrorContains(t, managednet.NewAPIConfigurer(api, noAccess, applianceToken).SetLinkUp(t.Context(), vm, 1), "vmbr0")
	require.Empty(t, api.writes, "a card on another bridge is not pco's to bring up")
}

// accessData is a cluster where pco's user and its privilege-separated token
// of the appliance read everything, with the ACL entries given.
func accessData(acl ...pve.ACLEntry) access.Data {
	return access.Data{
		ACL: append([]pve.ACLEntry{
			{Path: "/", Type: "user", UGID: "pco@pve", RoleID: "PCO", Propagate: true},
			{Path: "/", Type: "token", UGID: applianceToken, RoleID: "PCO", Propagate: true},
		}, acl...),
		Users: []pve.User{{ID: "pco@pve", Enabled: true, Tokens: map[string]bool{"vm900": true}}},
		Roles: []pve.Role{
			{ID: "PCO", Privs: []string{"Pool.Audit", "SDN.Audit", "Sys.Audit", "VM.Audit"}},
			{ID: "PCOManaged", Privs: []string{"VM.Config.Network"}},
		},
	}
}

func managedGrant(path string) []pve.ACLEntry {
	return []pve.ACLEntry{
		{Path: path, Type: "user", UGID: "pco@pve", RoleID: "PCOManaged", Propagate: true},
		{Path: path, Type: "token", UGID: applianceToken, RoleID: "PCOManaged", Propagate: true},
	}
}

func TestAPICanWrite(t *testing.T) {
	line := "pveum acl modify /vms/9320 --users pco@pve --tokens 'pco@pve!vm900' --roles PCOManaged"
	pooled := container
	pooled.Pool = "tenants"
	tests := []struct {
		name  string
		guest model.Guest
		data  access.Data
		ok    bool
		why   string
	}{
		{name: "a grant on the guest", guest: container, data: accessData(managedGrant("/vms/9320")...), ok: true},
		{name: "a grant on the guest's pool", guest: pooled, data: accessData(managedGrant("/pool/tenants")...), ok: true},
		{name: "a grant on another pool", guest: pooled, data: accessData(managedGrant("/pool/other")...), why: line},
		{name: "a grant on another guest", guest: container, data: accessData(managedGrant("/vms/9321")...), why: line},
		{name: "no grant", guest: container, data: accessData(), why: line},
		{name: "a grant to the user alone", guest: container, data: accessData(managedGrant("/vms/9320")[0]), why: line},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.data
			read := func(context.Context) (access.Data, error) { return data, nil }
			ok, why, err := managednet.NewAPIConfigurer(&fakeAPI{}, read, applianceToken).CanWrite(t.Context(), tt.guest)
			require.NoError(t, err)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.why, why)
		})
	}
}

// The pool of the guest comes from the snapshot, as the access control does
// not list it: what was read is not changed by the check.
func TestAPICanWriteLeavesTheAccessDataAlone(t *testing.T) {
	data := accessData(managedGrant("/pool/tenants")...)
	data.Pools = map[model.GuestRef]string{}
	pooled := container
	pooled.Pool = "tenants"
	read := func(context.Context) (access.Data, error) { return data, nil }
	ok, _, err := managednet.NewAPIConfigurer(&fakeAPI{}, read, applianceToken).CanWrite(t.Context(), pooled)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, data.Pools)
}

func TestAPICanWriteWithoutTheAccessControl(t *testing.T) {
	read := func(context.Context) (access.Data, error) {
		return access.Data{}, errors.New("the access control could not be read")
	}
	ok, why, err := managednet.NewAPIConfigurer(&fakeAPI{}, read, applianceToken).CanWrite(t.Context(), container)
	require.EqualError(t, err, "whether pco may change lxc/9320 is not known: the access control could not be read")
	require.False(t, ok)
	require.Empty(t, why)
}
