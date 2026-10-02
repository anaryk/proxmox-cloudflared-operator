package egress

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeOverride(t *testing.T, ov *Overrides, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(ov.dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ov.dir, name), []byte(content), 0o600))
}

func jsonEqual(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

func TestTheBlockListStartsEmpty(t *testing.T) {
	ov := NewOverrides(filepath.Join(t.TempDir(), "not-yet"))

	blocked, err := ov.Blocked()

	require.NoError(t, err)
	require.Empty(t, blocked)
}

func TestBlockKeepsASortedListWithoutDuplicates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	ov := NewOverrides(dir)

	for _, a := range []string{"10.0.0.9", "fd00::9", "10.0.0.10", "::ffff:10.0.0.9"} {
		_, err := ov.Block(addr(a))
		require.NoError(t, err)
	}
	added, err := ov.Block(addr("10.0.0.10"))
	require.NoError(t, err)
	require.False(t, added, "already blocked")

	blocked, err := ov.Blocked()
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{addr("10.0.0.9"), addr("10.0.0.10"), addr("fd00::9")}, blocked)

	b, err := os.ReadFile(filepath.Join(dir, blockedFile))
	require.NoError(t, err)
	require.JSONEq(t, `{"addresses":["10.0.0.9","10.0.0.10","fd00::9"]}`, string(b))
	require.Equal(t, os.FileMode(0o600), mode(t, filepath.Join(dir, blockedFile)))
	require.Equal(t, os.FileMode(0o700), mode(t, dir))
	require.Equal(t, []string{blockedFile}, names(t, dir), "no temporary file is left behind")
}

func TestUnblockTakesAnAddressOut(t *testing.T) {
	ov := NewOverrides(t.TempDir())
	for _, a := range []string{"10.0.0.9", "10.0.0.10"} {
		_, err := ov.Block(addr(a))
		require.NoError(t, err)
	}

	removed, err := ov.Unblock(addr("10.0.0.9"))
	require.NoError(t, err)
	require.True(t, removed)
	removed, err = ov.Unblock(addr("10.0.0.9"))
	require.NoError(t, err)
	require.False(t, removed)

	blocked, err := ov.Blocked()
	require.NoError(t, err)
	require.Equal(t, []netip.Addr{addr("10.0.0.10")}, blocked)
}

func TestBlockRefusesWhatIsNoAddress(t *testing.T) {
	ov := NewOverrides(t.TempDir())

	_, err := ov.Block(netip.Addr{})

	require.Error(t, err)
}

func TestABlockListThatCannotBeReadIsAnError(t *testing.T) {
	for name, content := range map[string]string{
		"not json":       "{",
		"not an address": `{"addresses":["10.0.0.9","nonsense"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			ov := NewOverrides(t.TempDir())
			writeOverride(t, ov, blockedFile, content)

			_, err := ov.Blocked()
			require.ErrorContains(t, err, blockedFile)
			_, err = ov.Block(addr("10.0.0.1"))
			require.Error(t, err, "a list that cannot be read is not replaced")
		})
	}
}

func TestTheSwitchIsOnUntilItIsSwitchedOff(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local")
	ov := NewOverrides(dir)
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60))

	_, off, err := ov.Off()
	require.NoError(t, err)
	require.False(t, off)

	require.NoError(t, ov.SwitchOff(at))
	since, off, err := ov.Off()
	require.NoError(t, err)
	require.True(t, off)
	require.True(t, since.Equal(at))
	require.Equal(t, os.FileMode(0o600), mode(t, filepath.Join(dir, offFile)))
	b, err := os.ReadFile(filepath.Join(dir, offFile))
	require.NoError(t, err)
	require.JSONEq(t, `{"since":"2026-10-01T10:00:00Z"}`, string(b))

	was, err := ov.SwitchOn()
	require.NoError(t, err)
	require.True(t, was)
	_, off, err = ov.Off()
	require.NoError(t, err)
	require.False(t, off)
	was, err = ov.SwitchOn()
	require.NoError(t, err)
	require.False(t, was)
}

func TestTheSwitchIsOffWhileItsFileIsThereWhateverItHolds(t *testing.T) {
	ov := NewOverrides(t.TempDir())
	writeOverride(t, ov, offFile, "garbage")

	since, off, err := ov.Off()

	require.NoError(t, err)
	require.True(t, off)
	require.True(t, since.IsZero())
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
