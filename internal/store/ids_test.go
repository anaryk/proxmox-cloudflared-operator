package store

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// envelopeJSON is a stored file as it is written, for tests that write one by hand.
func envelopeJSON(id, data string) string {
	return `{"schemaVersion":1,"rev":1,"id":"` + id + `","data":` + data + `}`
}

func TestEnvelopeCarriesTheID(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, NewDir(root).Put("things", "NAS", sample{N: 1}))

	b, err := os.ReadFile(filepath.Join(root, "things", "nas.json"))
	require.NoError(t, err)
	require.Regexp(t, `^\{\s*"schemaVersion": 1,\s*"rev": 1,\s*"id": "NAS",\s*"data": \{`, string(b))
}

func TestPutRefusesToReplaceTheObjectOfAnotherID(t *testing.T) {
	tests := []struct{ name, first, second string }{
		{"case", "NAS", "nas"},
		{"case the other way", "nas", "NAS"},
		{"slash and underscore", "a/b", "a_b"},
		{"underscore and slash", "a_b", "a/b"},
		{"wildcard and its file name", "*.example.com", "_wildcard.example.com"},
		{"file name and wildcard", "_wildcard.example.com", "*.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDir(t.TempDir())
			require.NoError(t, d.Put("things", tc.first, sample{Name: "first"}))

			err := d.Put("things", tc.second, sample{Name: "second"})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.first)
			require.Contains(t, err.Error(), tc.second)

			var got sample
			found, err := d.Get("things", tc.first, &got)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "first", got.Name, "the first object is untouched")
		})
	}
}

func TestPutOfTheSameIDStillReplaces(t *testing.T) {
	d := NewDir(t.TempDir())
	require.NoError(t, d.Put("things", "NAS", sample{N: 1}))
	require.NoError(t, d.Put("things", "NAS", sample{N: 2}))
	var got sample
	_, err := d.Get("things", "NAS", &got)
	require.NoError(t, err)
	require.Equal(t, 2, got.N)
}

func TestGetAndDeleteRefuseTheFileOfAnotherID(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "nas.json")
	require.NoError(t, d.Put("things", "NAS", sample{N: 1}))

	var got sample
	found, err := d.Get("things", "nas", &got)
	require.Error(t, err, "an id that only maps to the file is not found, it is a mistake")
	require.False(t, found)
	require.Contains(t, err.Error(), "NAS")
	require.Contains(t, err.Error(), "nas")

	err = d.Delete("things", "nas")
	require.Error(t, err)
	require.Contains(t, err.Error(), "NAS")
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "the file is still there")

	require.NoError(t, d.Delete("things", "NAS"))
	requireMissing(t, path)
}

func TestAFileWithoutAnIDIsAnErrorOnGet(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, `{"schemaVersion":1,"rev":1,"data":{"name":"x"}}`)

	var got sample
	found, err := d.Get("things", "a", &got)
	require.Error(t, err)
	require.False(t, found)
	require.Contains(t, err.Error(), path)
	require.Contains(t, err.Error(), "id")

	require.NoError(t, d.Put("things", "a", sample{Name: "y"}), "a file that cannot be read as an object is replaced")
	found, err = d.Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "y", got.Name)
}

func TestDeleteRemovesAFileThatCannotBeRead(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, `{"schemaVersion":1,"rev":1,"id":"a","data":`)
	require.NoError(t, d.Delete("things", "a"))
	requireMissing(t, path)
}

func TestDeleteKeepsAFileOfANewerVersion(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, `{"schemaVersion":2,"rev":1,"id":"a","data":{}}`)
	require.Error(t, d.Delete("things", "a"))
	_, err := os.Stat(path)
	require.NoError(t, err)
}

func TestTrailingDotIDsAreObjectsOfTheirOwn(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	require.NoError(t, d.Put("things", "a", sample{Name: "plain"}))
	require.NoError(t, d.Put("things", "a.", sample{Name: "dotted"}))

	ids, err := d.List("things")
	require.NoError(t, err)
	require.Equal(t, []string{"a", "a."}, ids)
	var got sample
	_, err = d.Get("things", "a", &got)
	require.NoError(t, err)
	require.Equal(t, "plain", got.Name)
	_, err = d.Get("things", "a.", &got)
	require.NoError(t, err)
	require.Equal(t, "dotted", got.Name)

	require.NoError(t, d.Delete("things", "a."))
	_, err = d.Get("things", "a", &got)
	require.NoError(t, err)
	require.Equal(t, "plain", got.Name)
}

func manualRoute(id string) model.Route {
	return model.Route{
		Hostname: "nas.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.0.0.9"), Port: 80},
		Source:   model.SourceManual,
		ManualID: id,
	}
}

func TestStoreIDsThatShareAFileDoNotReplaceEachOther(t *testing.T) {
	t.Run("manual routes", func(t *testing.T) {
		s, _ := openStore(t)
		require.NoError(t, s.SaveManualRoute(manualRoute("nas")))

		err := s.SaveManualRoute(manualRoute("NAS"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "NAS")
		require.Contains(t, err.Error(), "nas")
		require.Error(t, s.DeleteManualRoute("Nas"))

		routes, err := s.ManualRoutes()
		require.NoError(t, err)
		require.Equal(t, []model.Route{manualRoute("nas")}, routes)
		require.NoError(t, s.DeleteManualRoute("nas"))
	})
	t.Run("manual routes and wildcards", func(t *testing.T) {
		s, _ := openStore(t)
		require.NoError(t, s.SaveManualRoute(manualRoute("*.x")))
		require.Error(t, s.SaveManualRoute(manualRoute("_wildcard.x")))
		require.Error(t, s.DeleteManualRoute("_wildcard.x"))
		routes, err := s.ManualRoutes()
		require.NoError(t, err)
		require.Len(t, routes, 1)
		require.Equal(t, "*.x", routes[0].ManualID)
	})
	t.Run("credentials", func(t *testing.T) {
		s, _ := openStore(t)
		shop := credentialOf("shop")
		require.NoError(t, s.SaveCredential(shop))

		other := credentialOf("Shop")
		other.Token = NewSecret("another-token-value")
		err := s.SaveCredential(other)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Shop")
		require.NotContains(t, err.Error(), "another-token-value")
		require.NotContains(t, err.Error(), secretToken)
		require.Error(t, s.DeleteCredential("SHOP"))

		got, err := s.Credentials()
		require.NoError(t, err)
		require.Equal(t, []Credential{shop}, got, "the token of shop is the one it had")
	})
	t.Run("approvals", func(t *testing.T) {
		s, _ := openStore(t)
		require.NoError(t, s.SaveApproval(approvalOf("qemu/101", "ident-a")))
		err := s.SaveApproval(approvalOf("qemu_101", "ident-b"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "qemu/101")
		require.Contains(t, err.Error(), "qemu_101")
		require.Error(t, s.DeleteApproval("qemu_101"))

		got, err := s.Approvals()
		require.NoError(t, err)
		require.Equal(t, map[string]Approval{"qemu/101": approvalOf("qemu/101", "ident-a")}, got)
	})
	t.Run("nodes", func(t *testing.T) {
		s, _ := openStore(t)
		pve1 := NodeEntry{Name: "pve1", Version: "1", Since: t0}
		require.NoError(t, s.SaveNode(pve1))
		require.Error(t, s.SaveNode(NodeEntry{Name: "PVE1", Version: "2", Since: t0}))
		require.Error(t, s.DeleteNode("PVE1"))

		got, err := s.Nodes()
		require.NoError(t, err)
		require.Equal(t, []NodeEntry{pve1}, got)
	})
}
