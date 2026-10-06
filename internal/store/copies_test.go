package store

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// copyStores opens the roots twice: as this release does on node pve1, and as
// the previous release does, which knows nothing of the cluster copies and
// writes the old objects alone.
func copyStores(t *testing.T) (now, previous *Store, p Paths) {
	t.Helper()
	now, p = openStore(t)
	now.SetNode("pve1")
	previous, err := Open(p)
	require.NoError(t, err)
	return now, previous, p
}

// readOnlyDir makes dir refuse new files for the rest of the test, as the
// cluster filesystem does without quorum.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	skipAsRoot(t)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// copyOn reads the cluster copy of id as it is on disk.
func copyOn(t *testing.T, p Paths, id string) (from DerivedFrom, value json.RawMessage) {
	t.Helper()
	var c struct {
		DerivedFrom DerivedFrom     `json:"derivedFrom"`
		Value       json.RawMessage `json:"value"`
	}
	require.NoError(t, json.Unmarshal(readRaw(t, filepath.Join(p.Cluster, "meta", id+".json")).Data, &c))
	return c.DerivedFrom, c.Value
}

func memoryOf(zone string, egress ...string) EngineMemory {
	m := EngineMemory{
		Version:   MemoryVersion,
		InstallID: "0123456789ab",
		Served:    []RememberedZone{{ID: "z-" + zone, Name: zone, AccountID: "acc", CredentialID: "cred"}},
	}
	for _, e := range egress {
		m.Egress = append(m.Egress, netip.MustParseAddrPort(e))
	}
	return m
}

func TestTheEngineMemoryIsWrittenBesideTheOldOneWithoutTheEgressSet(t *testing.T) {
	now, _, p := copyStores(t)
	m := memoryOf("example.com", "10.0.0.5:80")

	require.NoError(t, now.SaveEngineMemory(m))

	require.Equal(t, []string{"meta/engine-memory.json"}, stored(t, p.Local))
	require.Equal(t, []string{"meta/engine-memory.json"}, stored(t, p.Cluster))
	from, value := copyOn(t, p, "engine-memory")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 1}, from)
	require.NotContains(t, string(value), "egress", "the egress set is the node's")
	require.Contains(t, string(value), "example.com")

	got, notes, err := now.LoadEngineMemory()
	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, m.sorted(), got, "the egress set comes from the node")

	require.NoError(t, now.SaveEngineMemory(m))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Cluster, "meta", "engine-memory.json")), "an unchanged memory is not written")
}

func TestAnEngineMemoryThePreviousReleaseWroteWinsOverTheCopy(t *testing.T) {
	now, previous, p := copyStores(t)
	require.NoError(t, now.SaveEngineMemory(memoryOf("example.com")))
	newer := memoryOf("example.org", "10.0.0.6:443")
	require.NoError(t, previous.SaveEngineMemory(newer), "a rollback: the old release served meanwhile")
	from, _ := copyOn(t, p, "engine-memory")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 1}, from, "the previous release leaves the copy alone")

	got, notes, err := now.LoadEngineMemory()
	require.NoError(t, err)
	require.Equal(t, newer.sorted(), got)
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "engine memory")

	from, value := copyOn(t, p, "engine-memory")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 2}, from, "the copy is made again from the old object")
	require.Contains(t, string(value), "example.org")
	require.NotContains(t, string(value), "egress")

	got, notes, err = now.LoadEngineMemory()
	require.NoError(t, err)
	require.Equal(t, newer.sorted(), got)
	require.Empty(t, notes, "once made again, the copy is taken")

	plain, err := now.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, newer.sorted(), plain)
}

func TestTheCopyOfAnotherNodeIsTakenWhateverThisNodeHas(t *testing.T) {
	now, _, p := copyStores(t)
	writeFile(t, filepath.Join(p.Local, "meta", "engine-memory.json"),
		`{"schemaVersion":1,"rev":9,"id":"engine-memory","data":{"version":1,"served":[{"id":"z-old","name":"old.example","accountId":"acc","credentialId":"cred"}],"egress":["10.0.0.7:22"]}}`)
	writeFile(t, filepath.Join(p.Cluster, "meta", "engine-memory.json"),
		`{"schemaVersion":1,"rev":4,"id":"engine-memory","data":{"derivedFrom":{"node":"pve2","rev":2},"value":{"version":1,"served":[{"id":"z-new","name":"new.example","accountId":"acc","credentialId":"cred"}]}}}`)

	got, notes, err := now.LoadEngineMemory()

	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, []RememberedZone{{ID: "z-new", Name: "new.example", AccountID: "acc", CredentialID: "cred"}}, got.Served,
		"the controller was on pve2: what pve1 kept is older")
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.7:22")}, got.Egress)
	require.EqualValues(t, 4, revOf(t, filepath.Join(p.Cluster, "meta", "engine-memory.json")), "nothing is written")

	require.NoError(t, now.SaveEngineMemory(got))
	from, _ := copyOn(t, p, "engine-memory")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 10}, from, "a write names the node that wrote it")
}

func TestTheFirstCopyIsMadeFromTheOldObjectWithoutANote(t *testing.T) {
	now, previous, p := copyStores(t)
	got, notes, err := now.LoadEngineMemory()
	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, EngineMemory{}, got)
	require.Empty(t, stored(t, p.Cluster), "nothing to copy is not copied")

	require.NoError(t, previous.SaveEngineMemory(memoryOf("example.com")))
	got, notes, err = now.LoadEngineMemory()
	require.NoError(t, err)
	require.Empty(t, notes, "an upgrade from the previous release is no surprise")
	require.Equal(t, memoryOf("example.com").sorted(), got)
	from, _ := copyOn(t, p, "engine-memory")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 1}, from)
}

func TestAStoreThatKnowsNoNodeKeepsNoCopy(t *testing.T) {
	_, previous, p := copyStores(t)
	require.NoError(t, previous.SaveEngineMemory(memoryOf("example.com")))
	require.NoError(t, previous.SaveNodeAddrs([]netip.Addr{netip.MustParseAddr("192.168.4.10")}))
	require.NoError(t, previous.SaveSoftDeny(SoftDeny{Entries: []SoftEntry{softEntry("192.168.4.1", "gateway of node pve1", t0)}}))
	_, _, err := previous.LoadEngineMemory()
	require.NoError(t, err)

	require.Empty(t, stored(t, p.Cluster))
	require.Empty(t, previous.Node())
}

func TestNodeAddrsThePreviousReleaseWroteWinOverTheCopy(t *testing.T) {
	now, previous, p := copyStores(t)
	first := []netip.Addr{netip.MustParseAddr("192.168.4.10")}
	require.NoError(t, now.SaveNodeAddrs(first))
	from, _ := copyOn(t, p, "nodeaddrs")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 1}, from)
	got, notes, err := now.LoadNodeAddrs()
	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, first, got)

	more := []netip.Addr{netip.MustParseAddr("192.168.4.10"), netip.MustParseAddr("192.168.4.11")}
	require.NoError(t, previous.SaveNodeAddrs(more))

	got, notes, err = now.LoadNodeAddrs()
	require.NoError(t, err)
	require.Equal(t, more, got)
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "node addresses")
	from, value := copyOn(t, p, "nodeaddrs")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 2}, from)
	require.JSONEq(t, `["192.168.4.10","192.168.4.11"]`, string(value))

	plain, err := now.NodeAddrs()
	require.NoError(t, err)
	require.Equal(t, more, plain)
}

func TestTheSoftDenyListThePreviousReleaseWroteWinsOverTheCopy(t *testing.T) {
	now, previous, p := copyStores(t)
	first := SoftDeny{Entries: []SoftEntry{softEntry("192.168.4.1", "gateway of node pve1", t0)}}
	require.NoError(t, now.SaveSoftDeny(first))
	from, _ := copyOn(t, p, "softdeny")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 1}, from)

	later := SoftDeny{Entries: []SoftEntry{
		softEntry("192.168.4.1", "gateway of node pve1", t0.Add(time.Hour)),
		softEntry("1.1.1.1", "resolver of node pve1", t0.Add(time.Hour)),
	}}
	require.NoError(t, previous.SaveSoftDeny(later))

	got, notes, err := now.LoadSoftDeny()
	require.NoError(t, err)
	require.Equal(t, SoftDeny{Entries: compactSoft(later.Entries)}, got)
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "soft deny")
	from, _ = copyOn(t, p, "softdeny")
	require.Equal(t, DerivedFrom{Node: "pve1", Rev: 2}, from)

	plain, err := now.SoftDeny()
	require.NoError(t, err)
	require.Equal(t, got, plain)
}

// A save that kept the old object and not the copy kept what it saved: it
// says so with a note the caller can tell from a failure.
func TestASaveWhoseCopyFailsKeepsTheOldObjectAndSaysSo(t *testing.T) {
	now, _, p := copyStores(t)
	require.NoError(t, now.SaveSoftDeny(SoftDeny{Entries: []SoftEntry{softEntry("192.168.4.1", "gateway of node pve1", t0)}}))
	readOnlyDir(t, filepath.Join(p.Cluster, "meta"))
	later := SoftDeny{Entries: []SoftEntry{softEntry("192.168.4.1", "gateway of node pve1", t0.Add(time.Hour))}}

	err := now.SaveSoftDeny(later)

	var note *CopyNote
	require.ErrorAs(t, err, &note)
	require.Equal(t, "soft deny list", note.What)
	require.Contains(t, err.Error(), "soft deny list")
	got, notes, err := now.LoadSoftDeny()
	require.NoError(t, err)
	require.Equal(t, later, got, "this node's own is the newer one, and is read")
	require.Len(t, notes, 1)
}

// Until the upgrade is finished the old object is the authority: a copy
// that is damaged is no reason to fail the read.
func TestADamagedCopyFallsBackToTheOldObject(t *testing.T) {
	now, _, p := copyStores(t)
	m := memoryOf("example.com", "10.0.0.5:80")
	require.NoError(t, now.SaveEngineMemory(m))
	writeFile(t, filepath.Join(p.Cluster, "meta", "engine-memory.json"), "{ not json")

	got, notes, err := now.LoadEngineMemory()

	require.NoError(t, err)
	require.Equal(t, m.sorted(), got)
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "cannot be read")

	writeFile(t, filepath.Join(p.Cluster, "meta", "engine-memory.json"), `{"schemaVersion":99,"rev":1,"id":"engine-memory","data":{}}`)
	_, _, err = now.LoadEngineMemory()
	require.ErrorContains(t, err, "schema version 99", "a copy of a newer release is no damage to step over")
}

// A copy that cannot be written again is no reason to act on the older one:
// the read returns what the old object holds, and says the copy is behind.
func TestACopyThatCannotBeRewrittenStillReadsTheNewerObject(t *testing.T) {
	now, previous, p := copyStores(t)
	require.NoError(t, now.SaveNodeAddrs([]netip.Addr{netip.MustParseAddr("192.168.4.10")}))
	more := []netip.Addr{netip.MustParseAddr("192.168.4.10"), netip.MustParseAddr("192.168.4.12")}
	require.NoError(t, previous.SaveNodeAddrs(more))
	readOnlyDir(t, filepath.Join(p.Cluster, "meta"))

	got, notes, err := now.LoadNodeAddrs()

	require.NoError(t, err)
	require.Equal(t, more, got)
	require.Len(t, notes, 1)
	require.Contains(t, notes[0], "could not")
}
