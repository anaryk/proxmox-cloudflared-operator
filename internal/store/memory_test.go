package store

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func memorySample() EngineMemory {
	return EngineMemory{
		InstallID: "abc123",
		Served: []RememberedZone{
			{ID: "zone3", Name: "example.org", AccountID: "acc2", CredentialID: "cred2"},
			{ID: "zone1", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"},
		},
		Stale: []RememberedZone{
			{ID: "zone2", Name: "example.net", AccountID: "acc1", CredentialID: "cred1"},
			{ID: "zone4", Name: "example.info", AccountID: "acc1", CredentialID: "cred1"},
		},
		EverServed: []RememberedZone{
			{ID: "zone4", Name: "example.info", AccountID: "acc1", CredentialID: "cred1"},
			{ID: "zone1", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"},
		},
		Tunnels: []SeenTunnel{
			{ID: "00000000-0000-4000-8000-000000000002", Name: "pco-abc", AccountID: "acc2", CredentialID: "cred2"},
			{ID: "00000000-0000-4000-8000-000000000001", Name: "pco-abc", AccountID: "acc1", CredentialID: "cred1"},
		},
		GoneGuests: []model.GuestRef{{Kind: model.KindLXC, VMID: 200}, {Kind: model.KindQEMU, VMID: 101}},
		Egress:     targetsOf("10.0.0.12:443", "10.0.0.11:8080", "10.0.0.11:80", "10.0.0.12:443"),
		Verified: []VerifiedTargets{
			{AccountID: "acc2", Targets: targetsOf("10.0.0.12:443")},
			{AccountID: "acc1", Targets: targetsOf("10.0.0.11:8080", "10.0.0.11:80")},
			{AccountID: "acc3"},
		},
	}
}

func targetsOf(ss ...string) []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddrPort(s))
	}
	return out
}

func TestEngineMemoryIsEmptyUntilSaved(t *testing.T) {
	s, _ := openStore(t)

	got, err := s.EngineMemory()

	require.NoError(t, err)
	require.Equal(t, EngineMemory{}, got)
}

func TestEngineMemoryComesBackInOrder(t *testing.T) {
	s, p := openStore(t)

	require.NoError(t, s.SaveEngineMemory(memorySample()))
	got, err := s.EngineMemory()

	require.NoError(t, err)
	require.Equal(t, EngineMemory{
		InstallID: "abc123",
		Served: []RememberedZone{
			{ID: "zone1", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"},
			{ID: "zone3", Name: "example.org", AccountID: "acc2", CredentialID: "cred2"},
		},
		Stale: []RememberedZone{
			{ID: "zone4", Name: "example.info", AccountID: "acc1", CredentialID: "cred1"},
			{ID: "zone2", Name: "example.net", AccountID: "acc1", CredentialID: "cred1"},
		},
		EverServed: []RememberedZone{
			{ID: "zone1", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"},
			{ID: "zone4", Name: "example.info", AccountID: "acc1", CredentialID: "cred1"},
		},
		Tunnels: []SeenTunnel{
			{ID: "00000000-0000-4000-8000-000000000001", Name: "pco-abc", AccountID: "acc1", CredentialID: "cred1"},
			{ID: "00000000-0000-4000-8000-000000000002", Name: "pco-abc", AccountID: "acc2", CredentialID: "cred2"},
		},
		GoneGuests: []model.GuestRef{{Kind: model.KindQEMU, VMID: 101}, {Kind: model.KindLXC, VMID: 200}},
		Egress:     targetsOf("10.0.0.11:80", "10.0.0.11:8080", "10.0.0.12:443"),
		Verified: []VerifiedTargets{
			{AccountID: "acc1", Targets: targetsOf("10.0.0.11:80", "10.0.0.11:8080")},
			{AccountID: "acc2", Targets: targetsOf("10.0.0.12:443")},
			{AccountID: "acc3", Targets: []netip.AddrPort{}},
		},
	}, got)
	require.Equal(t, []string{"meta/engine-memory.json"}, stored(t, p.Local))
	require.Empty(t, stored(t, p.Cluster), "the memory is this node's own")
}

func TestSaveEngineMemoryWritesOnlyOnChange(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Local, "meta", "engine-memory.json")

	require.NoError(t, s.SaveEngineMemory(memorySample()))
	again := memorySample()
	again.Tunnels[0], again.Tunnels[1] = again.Tunnels[1], again.Tunnels[0]
	require.NoError(t, s.SaveEngineMemory(again))
	require.EqualValues(t, 1, revOf(t, path), "the same memory in another order is no change")

	again.GoneGuests = nil
	require.NoError(t, s.SaveEngineMemory(again))
	require.EqualValues(t, 2, revOf(t, path))
}

func TestEngineMemoryThatCannotBeReadIsAnError(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Local, "meta", "engine-memory.json"), `{"schemaVersion":1,"rev":1,"id":"engine-memory","data":{`)

	_, err := s.EngineMemory()

	require.Error(t, err)
}

func TestEngineMemoryWorksWhileTheClusterFilesystemIsNotMounted(t *testing.T) {
	p, marker := mountedPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, os.Remove(marker))

	require.NoError(t, s.SaveEngineMemory(memorySample()))
	got, err := s.EngineMemory()

	require.NoError(t, err)
	require.Len(t, got.Tunnels, 2)
}
