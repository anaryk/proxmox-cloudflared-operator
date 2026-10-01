package store

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

func claimOf(host, owner string) planner.Claim {
	return planner.Claim{Hostname: host, Owner: owner, Identity: "ident-" + owner, Since: t0}
}

// identities returns the files below dir, by name, as they are on disk now.
func identities(t *testing.T, dir string) map[string]os.FileInfo {
	t.Helper()
	out := make(map[string]os.FileInfo)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		require.NoError(t, err)
		out[e.Name()] = info
	}
	return out
}

func TestClaimsRoundTripByHostname(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Claims()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	missing := t0.Add(time.Hour)
	wild := planner.Claim{
		Hostname:     "*.shop.cz",
		Owner:        "qemu/101",
		Identity:     "ident",
		Since:        t0,
		MissingSince: &missing,
		Waiting:      []planner.Waiter{{Owner: "qemu/102", FirstSeen: t0.Add(2 * time.Hour)}},
	}
	plain := claimOf("Shop.cz", "lxc/200")
	plain.Hostname = "shop.cz"
	next := map[string]planner.Claim{"*.shop.cz": wild, "shop.cz": plain}

	require.NoError(t, s.SaveClaims(next))
	got, err = s.Claims()
	require.NoError(t, err)
	require.Equal(t, next, got)
	require.Equal(t, []string{"claims/_wildcard.shop.cz.json", "claims/shop.cz.json"}, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Local))
	require.Empty(t, stored(t, p.Private))
}

func TestSaveClaimsWritesOnlyWhatChanged(t *testing.T) {
	s, p := openStore(t)
	dir := filepath.Join(p.Cluster, "claims")
	next := map[string]planner.Claim{
		"a.example.com": claimOf("a.example.com", "qemu/101"),
		"b.example.com": claimOf("b.example.com", "qemu/102"),
		"c.example.com": claimOf("c.example.com", "qemu/103"),
	}
	require.NoError(t, s.SaveClaims(next))
	before := identities(t, dir)

	require.NoError(t, s.SaveClaims(next))
	after := identities(t, dir)
	require.Len(t, after, 3)
	for name, info := range before {
		require.True(t, os.SameFile(info, after[name]), "%s was written again", name)
		require.EqualValues(t, 1, revOf(t, filepath.Join(dir, name)), name)
	}

	changed := claimOf("b.example.com", "qemu/109")
	next["b.example.com"] = changed
	require.NoError(t, s.SaveClaims(next))
	after = identities(t, dir)
	require.True(t, os.SameFile(before["a.example.com.json"], after["a.example.com.json"]))
	require.True(t, os.SameFile(before["c.example.com.json"], after["c.example.com.json"]))
	require.False(t, os.SameFile(before["b.example.com.json"], after["b.example.com.json"]))
	require.EqualValues(t, 2, revOf(t, filepath.Join(dir, "b.example.com.json")))
	got, err := s.Claims()
	require.NoError(t, err)
	require.Equal(t, next, got)
}

func TestSaveClaimsAfterAReadWritesNothing(t *testing.T) {
	s, p := openStore(t)
	dir := filepath.Join(p.Cluster, "claims")
	missing := t0.Add(time.Minute)
	c := claimOf("a.example.com", "qemu/101")
	c.MissingSince = &missing
	require.NoError(t, s.SaveClaims(map[string]planner.Claim{"a.example.com": c}))
	before := identities(t, dir)

	loaded, err := s.Claims()
	require.NoError(t, err)
	require.NoError(t, s.SaveClaims(loaded))
	require.True(t, os.SameFile(before["a.example.com.json"], identities(t, dir)["a.example.com.json"]))
}

func TestSaveClaimsDeletesVanishedClaims(t *testing.T) {
	s, p := openStore(t)
	next := map[string]planner.Claim{
		"a.example.com": claimOf("a.example.com", "qemu/101"),
		"b.example.com": claimOf("b.example.com", "qemu/102"),
		"*.example.com": claimOf("*.example.com", "qemu/103"),
	}
	require.NoError(t, s.SaveClaims(next))

	delete(next, "b.example.com")
	delete(next, "*.example.com")
	require.NoError(t, s.SaveClaims(next))
	got, err := s.Claims()
	require.NoError(t, err)
	require.Equal(t, next, got)
	require.Equal(t, []string{"claims/a.example.com.json"}, stored(t, p.Cluster))

	require.NoError(t, s.SaveClaims(nil))
	require.Empty(t, stored(t, p.Cluster))
}

func TestSaveClaimsLeavesFilesItDoesNotKnowAlone(t *testing.T) {
	s, p := openStore(t)
	dir := filepath.Join(p.Cluster, "claims")
	writeFile(t, filepath.Join(dir, "notes.txt"), "keep")
	writeFile(t, filepath.Join(dir, "Upper.json"), "keep")
	require.NoError(t, s.SaveClaims(map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101")}))
	require.NoError(t, s.SaveClaims(nil))
	require.ElementsMatch(t, []string{"claims/notes.txt", "claims/Upper.json"}, stored(t, p.Cluster))
}

func TestSaveClaimsFillsAnEmptyHostname(t *testing.T) {
	s, _ := openStore(t)
	c := claimOf("", "qemu/101")
	require.NoError(t, s.SaveClaims(map[string]planner.Claim{"a.example.com": c}))
	got, err := s.Claims()
	require.NoError(t, err)
	require.Equal(t, "a.example.com", got["a.example.com"].Hostname)
}

func TestSaveClaimsRefusesBadInputAndWritesNothing(t *testing.T) {
	good := map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101")}
	tests := []struct {
		name string
		next map[string]planner.Claim
	}{
		{"key and hostname differ", map[string]planner.Claim{"b.example.com": claimOf("a.example.com", "qemu/101")}},
		{"empty key", map[string]planner.Claim{"": claimOf("", "qemu/101")}},
		{"unsafe key", map[string]planner.Claim{"../x": claimOf("../x", "qemu/101")}},
		{"keys share a file", map[string]planner.Claim{
			"Shop.cz": claimOf("Shop.cz", "qemu/101"),
			"shop.cz": claimOf("shop.cz", "qemu/102"),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, p := openStore(t)
			require.NoError(t, s.SaveClaims(good))
			next := map[string]planner.Claim{"z.example.com": claimOf("z.example.com", "qemu/109")}
			for k, v := range tc.next {
				next[k] = v
			}

			require.Error(t, s.SaveClaims(next))
			require.Equal(t, []string{"claims/a.example.com.json"}, stored(t, p.Cluster), "nothing was written or deleted")
		})
	}
}

func TestClaimsOfAnUnreadableStoreAreAnError(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveClaims(map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101")}))
	writeFile(t, filepath.Join(p.Cluster, "claims", "b.example.com.json"), `{"schemaVersion":1,"rev":1,"id":"b.example.com","data":{`)

	got, err := s.Claims()
	require.Error(t, err, "a partial set of claims would hand a hostname to someone else")
	require.Nil(t, got)
	require.Contains(t, err.Error(), "b.example.com.json")
}

func TestClaimsRefuseTwoFilesForOneHostname(t *testing.T) {
	s, p := openStore(t)
	body := envelopeJSON("a.example.com", `{"hostname":"a.example.com","owner":"qemu/101","since":"2026-10-01T12:00:00Z"}`)
	writeFile(t, filepath.Join(p.Cluster, "claims", "a.example.com.json"), body)
	writeFile(t, filepath.Join(p.Cluster, "claims", "other.json"), body)

	_, err := s.Claims()
	require.Error(t, err)
	require.Contains(t, err.Error(), "other.json")
	require.Contains(t, err.Error(), "a.example.com")
}

func TestClaimsRefuseAFileWithoutHostname(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "claims", "a.example.com.json"),
		envelopeJSON("a.example.com", `{"owner":"qemu/101","since":"2026-10-01T12:00:00Z"}`))
	_, err := s.Claims()
	require.Error(t, err)
	require.Contains(t, err.Error(), "a.example.com.json")
}

func TestClaimsRefuseAFileUnderTheNameOfAnotherID(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "claims", "a.example.com.json"),
		envelopeJSON("b.example.com", `{"hostname":"b.example.com","owner":"qemu/101","since":"2026-10-01T12:00:00Z"}`))
	_, err := s.Claims()
	require.Error(t, err)
	require.Contains(t, err.Error(), "a.example.com.json")
	require.Contains(t, err.Error(), "b.example.com")
}

func TestClaimsRefuseAFileWhoseIDIsNotItsHostname(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "claims", "a.example.com.json"),
		envelopeJSON("a.example.com", `{"hostname":"b.example.com","owner":"qemu/101","since":"2026-10-01T12:00:00Z"}`))
	_, err := s.Claims()
	require.Error(t, err)
	require.Contains(t, err.Error(), "a.example.com.json")
	require.Contains(t, err.Error(), "b.example.com")
}

func TestSaveClaimsChecksEverythingBeforeItWritesOrDeletes(t *testing.T) {
	s, p := openStore(t)
	stored1 := map[string]planner.Claim{
		"a.example.com": claimOf("a.example.com", "qemu/101"),
		"b.example.com": claimOf("b.example.com", "qemu/102"),
	}
	require.NoError(t, s.SaveClaims(stored1))
	dir := filepath.Join(p.Cluster, "claims")
	before := identities(t, dir)

	changed := claimOf("a.example.com", "qemu/109")
	tooBig := claimOf("z.example.com", "qemu/103")
	tooBig.Identity = strings.Repeat("x", 1<<20)
	err := s.SaveClaims(map[string]planner.Claim{"a.example.com": changed, "z.example.com": tooBig})
	require.Error(t, err, "the size of z is known before a is written and b is deleted")

	after := identities(t, dir)
	require.Len(t, after, 2)
	for name, info := range before {
		require.True(t, os.SameFile(info, after[name]), "%s was touched", name)
	}
	got, err := s.Claims()
	require.NoError(t, err)
	require.Equal(t, stored1, got)
}

func TestSaveBindingsChecksEverythingBeforeItWrites(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"a.example.com": bindingOf("a.example.com", "qemu/101")}))
	before := identities(t, filepath.Join(p.Local, "bindings"))

	big := bindingOf("z.example.com", "qemu/103")
	big.MAC = strings.Repeat("x", 1<<20)
	moved := bindingOf("a.example.com", "qemu/101")
	moved.Addr = netip.MustParseAddr("10.0.0.9")
	require.Error(t, s.SaveBindings(map[string]resolve.Binding{"a.example.com": moved, "z.example.com": big}))

	after := identities(t, filepath.Join(p.Local, "bindings"))
	require.Len(t, after, 1)
	require.True(t, os.SameFile(before["a.example.com.json"], after["a.example.com.json"]))
}

func bindingOf(host, owner string) resolve.Binding {
	return resolve.Binding{
		Owner:      owner,
		Hostname:   host,
		Guest:      owner,
		Addr:       netip.MustParseAddr("10.0.0.5"),
		MAC:        "bc:24:11:00:00:01",
		VerifiedAt: t0,
	}
}

func TestBindingsLiveOnTheNodeLocalRoot(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Bindings()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	failing := t0.Add(time.Minute)
	wild := bindingOf("*.shop.cz", "qemu/101")
	wild.FailingSince = &failing
	wild.Withdrawn = true
	next := map[string]resolve.Binding{
		"*.shop.cz": wild,
		"shop.cz":   bindingOf("shop.cz", "lxc/200"),
	}
	require.NoError(t, s.SaveBindings(next))

	got, err = s.Bindings()
	require.NoError(t, err)
	require.Equal(t, next, got)
	require.Equal(t, []string{"bindings/_wildcard.shop.cz.json", "bindings/shop.cz.json"}, stored(t, p.Local))
	require.Empty(t, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Private))
}

func TestSaveBindingsWritesOnlyWhatChangedAndDeletesTheRest(t *testing.T) {
	s, p := openStore(t)
	dir := filepath.Join(p.Local, "bindings")
	next := map[string]resolve.Binding{
		"a.example.com": bindingOf("a.example.com", "qemu/101"),
		"b.example.com": bindingOf("b.example.com", "qemu/102"),
	}
	require.NoError(t, s.SaveBindings(next))
	before := identities(t, dir)

	require.NoError(t, s.SaveBindings(next))
	after := identities(t, dir)
	for name, info := range before {
		require.True(t, os.SameFile(info, after[name]), "%s was written again", name)
	}

	moved := bindingOf("a.example.com", "qemu/101")
	moved.Addr = netip.MustParseAddr("10.0.0.6")
	next["a.example.com"] = moved
	delete(next, "b.example.com")
	require.NoError(t, s.SaveBindings(next))
	got, err := s.Bindings()
	require.NoError(t, err)
	require.Equal(t, next, got)
	require.Equal(t, []string{"bindings/a.example.com.json"}, stored(t, p.Local))
	require.EqualValues(t, 2, revOf(t, filepath.Join(dir, "a.example.com.json")))
}

func TestSaveBindingsRefusesAKeyThatIsNotTheHostname(t *testing.T) {
	s, p := openStore(t)
	err := s.SaveBindings(map[string]resolve.Binding{"a.example.com": bindingOf("b.example.com", "qemu/101")})
	require.Error(t, err)
	require.Empty(t, stored(t, p.Local))
}
