package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testOwner is whom the service addresses of the tests are made for.
var testOwner = ServiceOwner{Owner: "qemu/101", Guest: "qemu/101", Identity: "uuid:101"}

// A mapping is made for one owner, guest and identity of that guest: it is
// given to them again, and refused to anyone else, so that the address it
// forwards to is never handed to the next one there.
func TestAServiceAddrIsGivenOnlyToTheOwnerItWasMadeFor(t *testing.T) {
	s, p := openStore(t)
	m := allocate(t, s, "vmbr1", "10.20.0.5")
	require.Equal(t, testOwner, m.ServiceOwner)
	data, err := os.ReadFile(filepath.Join(p.Cluster, "svcaddrs", "vmbr1-10.20.0.5.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), `"owner": "qemu/101"`)
	require.Contains(t, string(data), `"identity": "uuid:101"`)
	_, err = s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), testOwner, t0)
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		owner ServiceOwner
		says  string
	}{
		{"another owner", ServiceOwner{Owner: "qemu/102", Guest: "qemu/102", Identity: "uuid:102"}, "qemu/102"},
		{"a new guest with the same VMID", ServiceOwner{Owner: "qemu/101", Guest: "qemu/101", Identity: "uuid:999"}, "qemu/101"},
		{"a manual route to the same guest", ServiceOwner{Owner: "manual/a", Guest: "qemu/101", Identity: "uuid:101"}, "manual/a (guest qemu/101)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AllocateServiceAddr("vmbr1", addr("10.20.0.5"), tc.owner, t0)
			require.EqualError(t, err, "service address 198.18.1.0 of 10.20.0.5 on vmbr1 was made for qemu/101, not for "+tc.says)
			_, err = s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), tc.owner, t0)
			require.ErrorContains(t, err, "was made for qemu/101, not for "+tc.says)
		})
	}
	_, err = s.AllocateServiceAddr("vmbr1", addr("10.20.0.6"), ServiceOwner{}, t0)
	require.ErrorContains(t, err, "the owner is empty")
	_, err = s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.6"), ServiceOwner{}, t0)
	require.ErrorContains(t, err, "the owner is empty")
	requirePool(t, s, "198.18.1.2", 2)

	again, err := s.AllocateServiceAddr("vmbr1", addr("10.20.0.5"), testOwner, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, m, again)

	moved, err := s.MoveServiceAddr("vmbr1", addr("10.20.0.5"), "vmbr2", addr("10.20.0.9"), t0.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, testOwner, moved.ServiceOwner, "a mapping that follows its target keeps its owner")
}

// A mapping stored before owners were kept is read as it is, and handed back
// without one: the engine judges whom it was made for.
func TestAServiceAddrStoredWithoutAnOwnerIsRead(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "svcaddrs", "vmbr1-10.20.0.5.json"), envelopeJSON("vmbr1-10.20.0.5",
		`{"network":"vmbr1","addr":"10.20.0.5","service":"198.18.1.0","allocatedAt":"2026-10-01T12:00:00Z"}`))
	writeFile(t, filepath.Join(p.Cluster, "nodesvcaddrs", "vmbr9-pve-a-10.30.0.5.json"), envelopeJSON("vmbr9-pve-a-10.30.0.5",
		`{"network":"vmbr9","node":"pve-a","addr":"10.30.0.5","service":"198.18.1.1","allocatedAt":"2026-10-01T12:00:00Z"}`))

	all, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []ServiceAddr{{Network: "vmbr1", Addr: addr("10.20.0.5"), Service: addr("198.18.1.0"), AllocatedAt: t0}}, all)
	require.False(t, all[0].Known())
	other := ServiceOwner{Owner: "qemu/102", Guest: "qemu/102", Identity: "uuid:102"}
	got, err := s.AllocateServiceAddr("vmbr1", addr("10.20.0.5"), other, t0)
	require.NoError(t, err)
	require.Equal(t, all[0], got)
	onNode, err := s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), other, t0)
	require.NoError(t, err)
	require.False(t, onNode.Known())
	requirePool(t, s, "198.18.1.2", 2)
}
