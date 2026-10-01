package connector

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

const secretToken = "tok-Sup3rS3cret-value"

func TestTokenReadsWhatEnsureWrote(t *testing.T) {
	m, _, _ := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, secretToken))

	got, found, err := m.Token(idA)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, secretToken, got)

	require.NoError(t, m.Ensure(t.Context(), idA, "rotated"))
	got, _, err = m.Token(idA)
	require.NoError(t, err)
	require.Equal(t, "rotated", got)
}

func TestTokenOfAConnectorWithoutOneIsNotFound(t *testing.T) {
	m, _, dir := newTestManager(t)

	got, found, err := m.Token(idA)
	require.NoError(t, err, "no directory yet")
	require.False(t, found)
	require.Empty(t, got)

	require.NoError(t, m.Ensure(t.Context(), idA, secretToken))
	_, found, err = m.Token(idB)
	require.NoError(t, err)
	require.False(t, found)

	writeFile(t, dir, idB+".token", " \n\t")
	got, found, err = m.Token(idB)
	require.NoError(t, err)
	require.False(t, found, "a blank file holds no token")
	require.Empty(t, got)
}

func TestTokenIsTrimmed(t *testing.T) {
	m, _, dir := newTestManager(t)
	writeFile(t, dir, idA+".token", "  "+secretToken+"\r\n")

	got, found, err := m.Token(idA)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, secretToken, got)
}

func TestTokenRefusesAnythingButATunnelID(t *testing.T) {
	m, _, dir := newTestManager(t)
	writeFile(t, dir, idA+".token", secretToken)
	for _, id := range []string{"", "..", "../" + idA, idUpper, idA + "x", idA[:35], idA + ".token"} {
		got, found, err := m.Token(id)
		require.Error(t, err, id)
		require.False(t, found, id)
		require.Empty(t, got, id)
		require.NotContains(t, err.Error(), secretToken)
	}
}

func TestTokenErrorsNeverCarryTheToken(t *testing.T) {
	m, _, dir := newTestManager(t)
	// A directory in the place of the file cannot be read as one.
	writeFile(t, filepath.Join(dir, idA+".token"), "inner", secretToken)

	got, found, err := m.Token(idA)
	require.Error(t, err)
	require.False(t, found)
	require.Empty(t, got)
	require.NotContains(t, err.Error(), secretToken)
	require.Contains(t, err.Error(), idA)

	if os.Geteuid() != 0 {
		path := filepath.Join(dir, idB+".token")
		writeFile(t, dir, idB+".token", secretToken)
		require.NoError(t, os.Chmod(path, 0))
		t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
		_, _, err = m.Token(idB)
		require.Error(t, err)
		require.NotContains(t, err.Error(), secretToken)
	}
}

// A test cannot prove a goroutine is blocked; this one can pass by chance, but
// fails whenever Token ignores the lock.
func TestTokenWaitsForTheManagersLock(t *testing.T) {
	m, _, dir := newTestManager(t)
	writeFile(t, dir, idA+".token", secretToken)

	m.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = m.Token(idA)
	}()
	for range 200 {
		runtime.Gosched()
	}
	select {
	case <-done:
		t.Fatal("Token returned while the manager held its lock")
	default:
	}
	m.mu.Unlock()
	<-done
}
