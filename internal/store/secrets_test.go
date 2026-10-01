package store

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	secretToken = "tok-Sup3rS3cret-value"
	tunnelA     = "6a1f3c52-9d0e-4b7a-8c11-2f5e7d9a0b34"
	tunnelB     = "0b9d7e21-4c63-4a58-9f02-71a3c5e8d6f1"
)

// requireNoSecretOnDisk fails when any file below dir holds secret.
func requireNoSecretOnDisk(t *testing.T, dir, secret string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NotContains(t, string(b), secret, path)
		return nil
	})
	require.NoError(t, err)
}

func credentialOf(id string) Credential {
	return Credential{ID: id, Label: "Shop account", Kind: "scoped", Token: secretToken, AddedAt: t0}
}

func TestCredentialsLiveOnThePrivateRootOnly(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Credentials()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	b, a := credentialOf("cred-b"), credentialOf("cred-a")
	b.Label = "Second"
	require.NoError(t, s.SaveCredential(b))
	require.NoError(t, s.SaveCredential(a))

	got, err = s.Credentials()
	require.NoError(t, err)
	require.Equal(t, []Credential{a, b}, got)
	require.Equal(t, []string{"credentials/cred-a.json", "credentials/cred-b.json"}, stored(t, p.Private))
	require.Empty(t, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Local))
	requireNoSecretOnDisk(t, p.Cluster, secretToken)
	requireNoSecretOnDisk(t, p.Local, secretToken)

	require.NoError(t, s.SaveCredential(a))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Private, "credentials", "cred-a.json")))

	require.NoError(t, s.DeleteCredential("cred-a"))
	require.NoError(t, s.DeleteCredential("cred-a"))
	got, err = s.Credentials()
	require.NoError(t, err)
	require.Equal(t, []Credential{b}, got)
}

func TestSaveCredentialRefusesIncompleteCredentials(t *testing.T) {
	s, p := openStore(t)
	noID := credentialOf("")
	noToken := credentialOf("cred-a")
	noToken.Token = ""
	badID := credentialOf("../x")
	for name, c := range map[string]Credential{"no id": noID, "no token": noToken, "unsafe id": badID} {
		err := s.SaveCredential(c)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), secretToken, name)
	}
	require.Empty(t, stored(t, p.Private))
	require.Error(t, s.DeleteCredential(""))
}

func TestCredentialErrorsNeverCarryTheToken(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveCredential(credentialOf("cred-a")))
	path := filepath.Join(p.Private, "credentials", "cred-b.json")

	for name, body := range map[string]string{
		"syntax error after the token": `{"schemaVersion":1,"rev":1,"data":{"id":"cred-b","token":"` + secretToken + `" x}}`,
		"token of the wrong type":      `{"schemaVersion":1,"rev":1,"data":{"id":"cred-b","token":["` + secretToken + `"]}}`,
		"label of the wrong type":      `{"schemaVersion":1,"rev":1,"data":{"id":"cred-b","label":123,"token":"` + secretToken + `"}}`,
		"cut off":                      `{"schemaVersion":1,"rev":1,"data":{"id":"cred-b","token":"` + secretToken,
	} {
		writeFile(t, path, body)
		_, err := s.Credentials()
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), secretToken, name)
		require.Contains(t, err.Error(), "cred-b.json", name)
	}
}

func TestCredentialDoesNotPrintItsToken(t *testing.T) {
	c := credentialOf("cred-a")
	ptr := &c
	wrapped := struct{ Cred Credential }{c}
	for name, got := range map[string]string{
		"%v":        fmt.Sprintf("%v", c),
		"%+v":       fmt.Sprintf("%+v", c),
		"%s":        fmt.Sprintf("[%s]", c),
		"%#v":       fmt.Sprintf("%#v", c),
		"pointer":   fmt.Sprintf("%v", ptr),
		"slice":     fmt.Sprintf("%v", []Credential{c}),
		"nested":    fmt.Sprintf("%+v", wrapped),
		"String":    c.String(),
		"Sprint":    fmt.Sprint(c),
		"map value": fmt.Sprintf("%v", map[string]Credential{"a": c}),
	} {
		require.NotContains(t, got, secretToken, name)
		require.Contains(t, got, "cred-a", name)
	}
	require.Contains(t, c.String(), "redacted")
}

func TestPVETokenRoundTrip(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.PVEToken()
	require.NoError(t, err)
	require.False(t, found)

	want := PVEToken{TokenID: "pco@pve!pco", Secret: "3f1c0a9e-1111-2222-3333-444455556666"}
	require.NoError(t, s.SavePVEToken(want))
	got, found, err := s.PVEToken()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)

	require.Equal(t, []string{"meta/pve-token.json"}, stored(t, p.Private))
	require.Empty(t, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Local))
	require.NoError(t, s.SavePVEToken(want))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Private, "meta", "pve-token.json")))
}

func TestSavePVETokenRefusesIncompleteTokens(t *testing.T) {
	s, p := openStore(t)
	require.Error(t, s.SavePVEToken(PVEToken{Secret: "s"}))
	require.Error(t, s.SavePVEToken(PVEToken{TokenID: "pco@pve!pco"}))
	require.Empty(t, stored(t, p.Private))
}

func TestPVETokenDoesNotPrintItsSecret(t *testing.T) {
	tok := PVEToken{TokenID: "pco@pve!pco", Secret: "3f1c0a9e-1111-2222-3333-444455556666"}
	for _, got := range []string{
		fmt.Sprintf("%v", tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok), fmt.Sprintf("%v", &tok), tok.String(),
	} {
		require.NotContains(t, got, tok.Secret)
		require.Contains(t, got, "pco@pve!pco")
	}
}

func tunnelFile(p Paths, id string) string {
	return filepath.Join(p.Local, "tunnels", id+".token")
}

func TestTunnelTokenFile(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.TunnelToken(tunnelA)
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, s.SaveTunnelToken(tunnelA, secretToken))
	got, found, err := s.TunnelToken(tunnelA)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, secretToken, got)

	b, err := os.ReadFile(tunnelFile(p, tunnelA))
	require.NoError(t, err)
	require.Equal(t, secretToken, string(b), "the file holds the token as it is, as the connector manager writes it")
	info, err := os.Stat(tunnelFile(p, tunnelA))
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	dir, err := os.Stat(filepath.Join(p.Local, "tunnels"))
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o700), dir.Mode().Perm())
	require.Equal(t, []string{"tunnels/" + tunnelA + ".token"}, stored(t, p.Local))
	require.Empty(t, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Private))

	require.NoError(t, s.SaveTunnelToken(tunnelA, "second-token"))
	got, _, err = s.TunnelToken(tunnelA)
	require.NoError(t, err)
	require.Equal(t, "second-token", got)
	info, err = os.Stat(tunnelFile(p, tunnelA))
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	require.Equal(t, []string{"tunnels/" + tunnelA + ".token"}, stored(t, p.Local), "no temporary file is left")
}

func TestTunnelTokenReadsAFileTheConnectorWrote(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, tunnelFile(p, tunnelB), "  "+secretToken+"\n")
	got, found, err := s.TunnelToken(tunnelB)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, secretToken, got)

	writeFile(t, tunnelFile(p, tunnelA), " \n")
	_, found, err = s.TunnelToken(tunnelA)
	require.NoError(t, err)
	require.False(t, found, "an empty token file holds no token")
}

func TestDeleteTunnelToken(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.DeleteTunnelToken(tunnelA), "nothing to delete")

	require.NoError(t, s.SaveTunnelToken(tunnelA, secretToken))
	require.NoError(t, s.SaveTunnelToken(tunnelB, "other"))
	writeFile(t, filepath.Join(p.Local, "tunnels", tunnelA+".env"), "METRICS_ADDR=127.0.0.1:20300\n")
	require.NoError(t, s.DeleteTunnelToken(tunnelA))
	require.NoError(t, s.DeleteTunnelToken(tunnelA))

	_, found, err := s.TunnelToken(tunnelA)
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = s.TunnelToken(tunnelB)
	require.NoError(t, err)
	require.True(t, found)
	_, err = os.Stat(filepath.Join(p.Local, "tunnels", tunnelA+".env"))
	require.NoError(t, err, "only the token goes")
}

func TestTunnelTokenRefusesAnythingButATunnelID(t *testing.T) {
	s, p := openStore(t)
	for _, id := range []string{
		"", "..", "../x", "abc", strings.ToUpper(tunnelA), tunnelA + "x", tunnelA[:35],
		"6a1f3c52/9d0e-4b7a-8c11-2f5e7d9a0b34", "../../etc/6a1f3c52-9d0e-4b7a-8c11-2f5e7d9a0b3", "6a1f3c52-9d0e-4b7a-8c11-2f5e7d9a0b3g",
	} {
		_, found, err := s.TunnelToken(id)
		require.Error(t, err, id)
		require.False(t, found, id)
		require.Error(t, s.SaveTunnelToken(id, secretToken), id)
		require.Error(t, s.DeleteTunnelToken(id), id)
	}
	require.Empty(t, stored(t, p.Local))
}

func TestSaveTunnelTokenRefusesTokensItCannotKeepAsTheyAre(t *testing.T) {
	s, p := openStore(t)
	for name, token := range map[string]string{
		"empty": "", "blank": "  ", "trailing newline": secretToken + "\n", "leading space": " " + secretToken,
	} {
		err := s.SaveTunnelToken(tunnelA, token)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), secretToken, name)
	}
	require.Empty(t, stored(t, p.Local))
}

func TestTunnelTokenErrorsNeverCarryTheToken(t *testing.T) {
	skipAsRoot(t)
	s, p := openStore(t)
	require.NoError(t, s.SaveTunnelToken(tunnelA, secretToken))
	require.NoError(t, os.Chmod(tunnelFile(p, tunnelA), 0))
	t.Cleanup(func() { _ = os.Chmod(tunnelFile(p, tunnelA), 0o600) })

	_, _, err := s.TunnelToken(tunnelA)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secretToken)

	dir := filepath.Join(p.Local, "tunnels")
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err = s.SaveTunnelToken(tunnelA, "another-"+secretToken)
	require.ErrorIs(t, err, fs.ErrPermission)
	require.NotContains(t, err.Error(), secretToken)
}

func TestTunnelTokenWriteLeavesNoTempFileWhenItFails(t *testing.T) {
	s, p := openStore(t)
	// A non-empty directory in the way makes the final rename fail.
	writeFile(t, filepath.Join(tunnelFile(p, tunnelA), "inner"), "x")

	require.Error(t, s.SaveTunnelToken(tunnelA, secretToken))
	require.Equal(t, []string{"tunnels/" + tunnelA + ".token/inner"}, stored(t, p.Local))
}

func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	s, _ := openStore(t)
	done := make(chan error, 8)
	for i := range 4 {
		go func() {
			done <- s.SaveNode(NodeEntry{Name: "pve1", Version: fmt.Sprint(i), Since: t0})
		}()
		go func() {
			done <- s.AppendAdopted(t0.Add(time.Duration(i)*time.Second), "zone1", adoptedSample(i))
		}()
	}
	for range 8 {
		require.NoError(t, <-done)
	}
	nodes, err := s.Nodes()
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	lines := adoptedLines(t, s)
	require.Len(t, lines, 4)
}
