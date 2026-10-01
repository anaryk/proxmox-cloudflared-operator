package store

import (
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// hashed is the file name of an id whose safe form is longer than 120 characters.
func hashed(id, safe string) string {
	sum := sha256.Sum256([]byte(id))
	return safe[:100] + "_h_" + hex.EncodeToString(sum[:])[:16]
}

// longHost returns a valid hostname of exactly n characters whose labels are
// made of the letter given, so that two of them can share a prefix.
func longHost(t *testing.T, n int, letter byte) string {
	t.Helper()
	var b strings.Builder
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strings.Repeat(string(letter), min(63, n-b.Len())))
	}
	require.Equal(t, n, b.Len())
	_, err := hostname.Normalize(b.String())
	require.NoError(t, err)
	return b.String()
}

func TestFileNameIsSafe(t *testing.T) {
	valid := []struct{ name, id, want string }{
		{"plain", "shop.cz", "shop.cz"},
		{"upper case", "Shop.CZ", "shop.cz"},
		{"wildcard", "*.shop.cz", "_wildcard.shop.cz"},
		{"wildcard upper case", "*.Shop.CZ", "_wildcard.shop.cz"},
		{"owner", "qemu/101", "qemu_101"},
		{"slash", "a/b", "a_b"},
		{"leading underscore", "_x", "_x"},
		{"hyphen and digits", "pve-1", "pve-1"},
		{"trailing dot", "a.", "a."},
		{"longest that is kept", strings.Repeat("a", 120), strings.Repeat("a", 120)},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FileName(tc.id)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	invalid := []struct{ name, id string }{
		{"empty", ""},
		{"dot dot", ".."},
		{"dot dot slash", "../x"},
		{"dot dot inside", "a..b"},
		{"dot dot after slash", "x/.."},
		{"dot dot far into a long id", strings.Repeat("a", 300) + ".." + "b"},
		{"single dot", "."},
		{"hidden", ".hidden"},
		{"leading hyphen", "-x"},
		{"space", "a b"},
		{"bare star", "*"},
		{"star inside", "a*b"},
		{"star without dot", "*shop.cz"},
		{"backslash", `a\b`},
		{"nul", "a\x00b"},
		{"newline", "a\nb"},
		{"non ascii", "café.cz"},
		{"non ascii far into a long id", strings.Repeat("a", 300) + "é"},
		{"kelvin sign", "Key"},
	}
	for _, tc := range invalid {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			got, err := FileName(tc.id)
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}

func TestFileNameKeepsAWildcardApartFromItsTwin(t *testing.T) {
	wild, err := FileName("*.shop.cz")
	require.NoError(t, err)
	plain, err := FileName("shop.cz")
	require.NoError(t, err)
	require.NotEqual(t, wild, plain)
}

func TestFileNameIsIdempotent(t *testing.T) {
	for _, id := range []string{"*.Shop.CZ", "qemu/101", "A.b", strings.Repeat("a", 121), strings.Repeat("b", 400)} {
		once, err := FileName(id)
		require.NoError(t, err)
		twice, err := FileName(once)
		require.NoError(t, err)
		require.Equal(t, once, twice, id)
	}
}

func TestFileNameOfALongIDIsAPrefixAndAHash(t *testing.T) {
	long121 := strings.Repeat("a", 121)
	got, err := FileName(long121)
	require.NoError(t, err)
	require.Equal(t, hashed(long121, long121), got)
	require.Len(t, got, 100+len("_h_")+16)

	host := longHost(t, 253, 'a')
	got, err = FileName(host)
	require.NoError(t, err)
	require.Equal(t, hashed(host, host), got)
	require.Regexp(t, `^[a-z0-9_][a-z0-9._-]{0,200}$`, got)
	require.NotContains(t, got, "..")

	wild := "*." + longHost(t, 243, 'a')
	require.Len(t, wild, 245)
	got, err = FileName(wild)
	require.NoError(t, err)
	safe := "_wildcard." + wild[2:]
	require.Equal(t, hashed(wild, safe), got, "the hash is of the id, the prefix of its safe form")
}

func TestFileNameOfLongIDsThatShareAPrefixDiffers(t *testing.T) {
	prefix := strings.Repeat("a", 100)
	one, err := FileName(prefix + strings.Repeat("x", 60))
	require.NoError(t, err)
	two, err := FileName(prefix + strings.Repeat("y", 60))
	require.NoError(t, err)
	require.NotEqual(t, one, two)
	require.Equal(t, one[:100], two[:100])

	again, err := FileName(prefix + strings.Repeat("x", 60))
	require.NoError(t, err)
	require.Equal(t, one, again, "the name of an id does not change")

	upper, err := FileName(strings.ToUpper(prefix) + strings.Repeat("x", 60))
	require.NoError(t, err)
	require.NotEqual(t, one, upper, "ids that differ only by case do not share a long name")
}

func TestDirKeepsLongIDsApart(t *testing.T) {
	d := NewDir(t.TempDir())
	prefix := strings.Repeat("a", 100)
	one, two := prefix+strings.Repeat("x", 60), prefix+strings.Repeat("y", 60)
	require.NoError(t, d.Put("things", one, sample{Name: "one"}))
	require.NoError(t, d.Put("things", two, sample{Name: "two"}))

	var got sample
	_, err := d.Get("things", one, &got)
	require.NoError(t, err)
	require.Equal(t, "one", got.Name)
	_, err = d.Get("things", two, &got)
	require.NoError(t, err)
	require.Equal(t, "two", got.Name)
	ids, err := d.List("things")
	require.NoError(t, err)
	require.Len(t, ids, 2)

	require.NoError(t, d.Delete("things", one))
	_, err = d.Get("things", two, &got)
	require.NoError(t, err)
}

func TestLongHostnamesRoundTripThroughClaims(t *testing.T) {
	s, p := openStore(t)
	long := longHost(t, 253, 'a')
	longTwin := longHost(t, 253, 'b')
	wild := "*." + longHost(t, 243, 'a')
	require.Len(t, long, 253)
	require.Len(t, wild, 245)
	next := map[string]planner.Claim{
		long:                  claimOf(long, "qemu/101"),
		longTwin:              claimOf(longTwin, "qemu/102"),
		wild:                  claimOf(wild, "qemu/103"),
		"short.example.com":   claimOf("short.example.com", "qemu/104"),
		"*.short.example.com": claimOf("*.short.example.com", "qemu/105"),
	}

	require.NoError(t, s.SaveClaims(next))
	got, err := s.Claims()
	require.NoError(t, err)
	require.Equal(t, next, got)

	files := stored(t, p.Cluster)
	require.Len(t, files, 5)
	for _, f := range files {
		require.LessOrEqual(t, len(filepath.Base(f)), 125, f)
	}

	delete(next, long)
	require.NoError(t, s.SaveClaims(next))
	got, err = s.Claims()
	require.NoError(t, err)
	require.Equal(t, next, got)
	require.Len(t, stored(t, p.Cluster), 4)
}

func TestOneLongHostnameDoesNotBlockTheOthers(t *testing.T) {
	s, _ := openStore(t)
	wild := "*." + longHost(t, 243, 'c')
	next := map[string]planner.Claim{
		"a.example.com": claimOf("a.example.com", "qemu/101"),
		wild:            claimOf(wild, "qemu/102"),
	}
	require.NoError(t, s.SaveClaims(next))
	got, err := s.Claims()
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestLongHostnamesRoundTripThroughBindings(t *testing.T) {
	s, p := openStore(t)
	long := longHost(t, 253, 'a')
	wild := "*." + longHost(t, 243, 'a')
	mk := func(host string) resolve.Binding {
		b := bindingOf(host, "qemu/101")
		b.Addr = netip.MustParseAddr("10.0.0.7")
		return b
	}
	next := map[string]resolve.Binding{long: mk(long), wild: mk(wild)}

	require.NoError(t, s.SaveBindings(next))
	got, err := s.Bindings()
	require.NoError(t, err)
	require.Equal(t, next, got)
	require.Len(t, stored(t, p.Local), 2)

	require.NoError(t, s.SaveBindings(nil))
	require.Empty(t, stored(t, p.Local))
}
