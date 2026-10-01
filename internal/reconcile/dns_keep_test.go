package reconcile

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// keptNames seeds six records of ours at names whose rule is the 503 block,
// pointing elsewhere, half of them with an overdue tombstone, one of those of
// another writer, and four wanted ones. It returns the input of a run that
// keeps the six, one of them spelt in upper case.
func keptNames(t *testing.T, store *memStore, seed func(name string)) DNSInput {
	t.Helper()
	in := dnsIn()
	in.Keep = map[string]bool{}
	for i := range 6 {
		name := fmt.Sprintf("held%d.example.com", i)
		seed(name)
		if i%2 == 0 {
			store.m[stoneKey(zone1.ID, name)] = overdue
		}
		in.Keep[name] = true
	}
	other := overdue
	other.Generation = ours.Generation - 1
	store.m[stoneKey(zone1.ID, "held2.example.com")] = other
	delete(in.Keep, "held1.example.com")
	in.Keep["HELD1.Example.com"] = true
	for i := range 4 {
		name := fmt.Sprintf("h%d.example.com", i)
		seed(name)
		in.Records = append(in.Records, wantRecord(zone1, name))
	}
	return in
}

func TestDNSKeptNamesAreNeitherWantedNorUnwanted(t *testing.T) {
	for _, mode := range []Mode{Enforce, Observe} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{m: map[string]Tombstone{}}
			in := keptNames(t, store, func(name string) {
				target := testTunnelID
				if strings.HasPrefix(name, "held") {
					target = "old"
				}
				f.SeedRecord(zone1.ID, ourCNAME(name, target))
			})

			res := newDNS(f, store, t0).Run(context.Background(), in, mode)

			require.Empty(t, res.Problems, "no mass delete guard")
			require.Empty(t, res.Actions, "not retargeted, not deleted, not held")
			require.Empty(t, dnsWrites(f))
			if mode == Enforce {
				require.Empty(t, store.m, "their tombstones are dropped, none is started")
			}
		})
	}
}

func TestDNSKeptNameThatIsReleasedStartsAFreshGrace(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("held.example.com", testTunnelID))
	key := stoneKey(zone1.ID, "held.example.com")
	store := &memStore{m: map[string]Tombstone{key: overdue}, saveErrs: map[int]error{1: errDisk, 2: errDisk}}
	c := &clock{t0}
	r := newDNSAt(f, store, writerOf(ours, ours), c)

	in := dnsIn()
	in.Keep = map[string]bool{"held.example.com": true}
	res := r.Run(ctx, in, Enforce)
	require.Equal(t, []string{"saving dns tombstones: disk full", "saving dns tombstones: disk full"}, res.Problems)
	require.Equal(t, overdue, store.m[key], "the drop was not saved")
	require.Equal(t, map[string]bool{key: true}, r.wantedSinceSave, "remembered like a wanted name")

	c.t = t0.Add(20 * time.Second)
	res = r.Run(ctx, dnsIn(), Enforce)
	require.Empty(t, dnsWrites(f))
	requireHeld(t, res.Actions, "grace period: 1m0s left")
	require.Equal(t, watched(c.t, c.t), store.m[key])
}

func TestDNSKeptNameIsRememberedOnlyInItsZone(t *testing.T) {
	r := newDNS(newDNSFake(), &memStore{}, t0)
	in := dnsIn()
	in.Keep = map[string]bool{"held.example.com": true, "shop.cz": true, "elsewhere.example.org": true}

	r.Run(context.Background(), in, Observe)

	require.Equal(t, map[string]bool{
		stoneKey(zone1.ID, "held.example.com"): true,
		stoneKey(zone2.ID, "shop.cz"):          true,
	}, r.wantedSinceSave)
}
