package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, b, 0o600))
}

func TestACopyUnderAnotherNameFailsTheLoad(t *testing.T) {
	tests := []struct {
		name string
		dir  func(Paths) string
		file string // the file of the object, which is copied
		save func(*Store) error
		load func(*Store) error
		del  func(*Store) error

		// sweeps is set for a set that is saved as a whole: saving it again
		// removes every file that is not in it, the copy included.
		sweeps bool
	}{
		{
			name: "manual routes", dir: func(p Paths) string { return filepath.Join(p.Cluster, "routes") },
			file: "nas.json",
			save: func(s *Store) error { return s.SaveManualRoute(manualRoute("nas")) },
			load: func(s *Store) error { _, err := s.ManualRoutes(); return err },
			del:  func(s *Store) error { return s.DeleteManualRoute("nas") },
		},
		{
			name: "approvals", dir: func(p Paths) string { return filepath.Join(p.Cluster, "approvals") },
			file: "qemu_101.json",
			save: func(s *Store) error { return s.SaveApproval(approvalOf("qemu/101", "ident")) },
			load: func(s *Store) error { _, err := s.Approvals(); return err },
			del:  func(s *Store) error { return s.DeleteApproval("qemu/101") },
		},
		{
			name: "nodes", dir: func(p Paths) string { return filepath.Join(p.Cluster, "nodes") },
			file: "pve1.json",
			save: func(s *Store) error { return s.SaveNode(NodeEntry{Name: "pve1", Version: "1", Since: t0}) },
			load: func(s *Store) error { _, err := s.Nodes(); return err },
			del:  func(s *Store) error { return s.DeleteNode("pve1") },
		},
		{
			name: "credentials", dir: func(p Paths) string { return filepath.Join(p.Private, "credentials") },
			file: "shop.json",
			save: func(s *Store) error { return s.SaveCredential(credentialOf("shop")) },
			load: func(s *Store) error { _, err := s.Credentials(); return err },
			del:  func(s *Store) error { return s.DeleteCredential("shop") },
		},
		{
			name: "claims", dir: func(p Paths) string { return filepath.Join(p.Cluster, "claims") },
			file: "a.example.com.json",
			save: func(s *Store) error {
				return s.SaveClaims(map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101")})
			},
			load: func(s *Store) error { _, err := s.Claims(); return err },
			del:  func(s *Store) error { return s.SaveClaims(nil) }, sweeps: true,
		},
		{
			name: "bindings", dir: func(p Paths) string { return filepath.Join(p.Local, "bindings") },
			file: "a.example.com.json",
			save: func(s *Store) error {
				return s.SaveBindings(map[string]resolve.Binding{"a.example.com": bindingOf("a.example.com", "qemu/101")})
			},
			load: func(s *Store) error { _, err := s.Bindings(); return err },
			del:  func(s *Store) error { return s.SaveBindings(nil) }, sweeps: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, p := openStore(t)
			require.NoError(t, tc.save(s))
			require.NoError(t, tc.load(s))
			dir := tc.dir(p)
			copyFile(t, filepath.Join(dir, tc.file), filepath.Join(dir, "old-copy.json"))

			err := tc.load(s)
			require.Error(t, err, "the copy is no object of its own and no silent twin of the original")
			require.Contains(t, err.Error(), "old-copy.json")
			require.Contains(t, err.Error(), tc.file, "it says where the object belongs")

			require.NoError(t, tc.del(s))
			if tc.sweeps {
				requireMissing(t, filepath.Join(dir, "old-copy.json"))
			} else {
				// Removing the object does not hide the copy: it is still reported.
				require.Error(t, tc.load(s))
				require.NoError(t, os.Remove(filepath.Join(dir, "old-copy.json")))
			}
			require.NoError(t, tc.load(s))
		})
	}
}

func TestACopyOfARouteDoesNotPublishTwoRoutes(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveManualRoute(manualRoute("nas")))
	copyFile(t, filepath.Join(p.Cluster, "routes", "nas.json"), filepath.Join(p.Cluster, "routes", "nas-copy.json"))

	routes, err := s.ManualRoutes()
	require.Error(t, err)
	require.Nil(t, routes)
}

func TestFilesUnderTheNameOfTheirIDStillLoad(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveManualRoute(manualRoute("NAS")))
	require.NoError(t, s.SaveApproval(approvalOf("qemu/101", "ident")))
	require.Equal(t, []string{"approvals/qemu_101.json", "routes/nas.json"}, stored(t, p.Cluster))

	routes, err := s.ManualRoutes()
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, "NAS", routes[0].ManualID)
	approvals, err := s.Approvals()
	require.NoError(t, err)
	require.Equal(t, map[string]Approval{"qemu/101": approvalOf("qemu/101", "ident")}, approvals)

	require.NoError(t, s.DeleteManualRoute("NAS"))
	require.NoError(t, s.DeleteApproval("qemu/101"))
	require.Empty(t, stored(t, p.Cluster))
}

func TestAnIDThatIsNoFileNameFailsTheLoad(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "nodes", "x.json"),
		envelopeJSON("../x", `{"name":"x","version":"1","since":"2026-10-01T12:00:00Z"}`))
	_, err := s.Nodes()
	require.Error(t, err)
	require.Contains(t, err.Error(), "x.json")
}
