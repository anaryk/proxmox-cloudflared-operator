package members

import (
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestReadFixtures(t *testing.T) {
	tests := []struct {
		file string
		want Members
	}{
		{
			file: "members-lab-standalone.json",
			want: Members{
				Self: "pco-test-1", Standalone: true, Quorate: true,
				Nodes: []Member{{Name: "pco-test-1", Online: true}},
			},
		},
		{
			file: "members-lab-one-node.json",
			want: Members{
				Self: "pco-lab-1", Cluster: "pco-lab", Quorate: true,
				Nodes: []Member{{Name: "pco-lab-1", ID: 1, Online: true, IP: addr("10.254.77.11")}},
			},
		},
		{
			file: "members-lab-two-nodes.json",
			want: Members{
				Self: "pco-lab-2", Cluster: "pco-lab", Quorate: true,
				Nodes: []Member{
					{Name: "pco-lab-1", ID: 1, Online: true, IP: addr("10.254.77.11")},
					{Name: "pco-lab-2", ID: 2, Online: true, IP: addr("10.254.77.12")},
				},
			},
		},
		{
			file: "members-lab-three-nodes.json",
			want: Members{
				Self: "pco-lab-3", Cluster: "pco-lab", Quorate: true,
				Nodes: []Member{
					{Name: "pco-lab-1", ID: 1, Online: true, IP: addr("10.254.77.11")},
					{Name: "pco-lab-2", ID: 2, Online: true, IP: addr("10.254.77.12")},
					{Name: "pco-lab-3", ID: 3, Online: true, IP: addr("10.254.77.13")},
				},
			},
		},
		{
			file: "members-quorate.json",
			want: Members{
				Self: "pve2", Cluster: "homelab", Quorate: true,
				Nodes: []Member{
					{Name: "pve1", ID: 1, Online: true, IP: addr("10.77.0.1")},
					{Name: "pve2", ID: 2, Online: true, IP: addr("10.77.0.2")},
					{Name: "pve3", ID: 3, Online: true, IP: addr("10.77.0.3")},
				},
			},
		},
		{
			file: "members-not-quorate.json",
			want: Members{
				Self: "pve2", Cluster: "homelab", Quorate: false,
				Nodes: []Member{
					{Name: "pve1", ID: 1, Online: false, IP: addr("10.77.0.1")},
					{Name: "pve2", ID: 2, Online: true, IP: addr("10.77.0.2")},
					{Name: "pve3", ID: 3, Online: false, IP: addr("10.77.0.3")},
				},
			},
		},
		{
			file: "members-one-offline.json",
			want: Members{
				Self: "pve1", Cluster: "homelab", Quorate: true,
				Nodes: []Member{
					{Name: "pve1", ID: 1, Online: true, IP: addr("10.77.0.1")},
					{Name: "pve2", ID: 2, Online: true, IP: addr("10.77.0.2")},
					{Name: "pve3", ID: 3, Online: false, IP: addr("10.77.0.3")},
				},
			},
		},
		{
			file: "members-ipv6.json",
			want: Members{
				Self: "pve1", Cluster: "v6lab", Quorate: true,
				Nodes: []Member{
					{Name: "pve1", ID: 1, Online: true, IP: addr("fd00:4::1")},
					{Name: "pve2", ID: 2, Online: true, IP: addr("fd00:4::2")},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := Read(fixture(t, tt.file), t0)
			require.NoError(t, err)
			tt.want.ReadAt = t0
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEveryFixtureIsReadOrRefusedAsItsNameSays(t *testing.T) {
	files, err := filepath.Glob(fixture(t, "members-*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			m, err := Read(f, t0)
			if strings.Contains(filepath.Base(f), "-bad-") {
				require.Error(t, err)
				require.Equal(t, Members{}, m)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, m.Self)
			require.Equal(t, t0, m.ReadAt)
			if m.Standalone {
				require.True(t, m.Quorate, "a standalone node is quorate")
				require.Empty(t, m.Cluster)
				require.Equal(t, []Member{{Name: m.Self, Online: true}}, m.Nodes, "a standalone node is its own list")
			} else {
				require.True(t, slices.ContainsFunc(m.Nodes, func(n Member) bool { return n.Name == m.Self }),
					"a node of a cluster is in its list")
			}
			require.True(t, slices.IsSortedFunc(m.Nodes, func(a, b Member) int {
				return strings.Compare(a.Name, b.Name)
			}), "nodes are by name")
		})
	}
}

func TestAFileCutMidWriteIsAnErrorAndNotAQuorumLoss(t *testing.T) {
	whole, err := os.ReadFile(fixture(t, "members-quorate.json"))
	require.NoError(t, err)
	// The last cut that is no whole file leaves the closing brace out; one byte
	// more is the file without its final newline.
	for cut := 1; cut < len(whole)-1; cut++ {
		m, err := Parse(whole[:cut], t0)
		require.Error(t, err, "a file cut at %d of %d bytes", cut, len(whole))
		require.Equal(t, Members{}, m, "cut at %d", cut)
	}
}

// inCluster is the file of node pve1 with the cluster object and the nodes a
// case puts into it.
func inCluster(cluster, nodes string) string {
	return `{"nodename": "pve1", "cluster": ` + cluster + `, "nodelist": {` + nodes + `}}`
}

const (
	selfOnline  = `"pve1": {"id": 1, "online": 1, "ip": "10.0.0.1"}`
	otherOnline = `"pve2": {"id": 2, "online": 1, "ip": "10.0.0.2"}`
	quorateOne  = `{"name": "c", "quorate": 1}`
)

func TestParseRefuses(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"empty", ""},
		{"blank", "  \n"},
		{"null", "null"},
		{"an array", "[]"},
		{"no node name", `{"version": 1}`},
		{"empty node name", `{"nodename": "", "version": 1}`},
		{"trailing data", `{"nodename": "pve1", "version": 1}}`},
		{"cluster without quorate", inCluster(`{"name": "c", "nodes": 2}`, selfOnline)},
		{"cluster of other fields only", inCluster(`{"nodes": 2}`, selfOnline)},
		{"cluster name as a number", inCluster(`{"name": 7, "quorate": 1}`, selfOnline)},
		{"cluster as text", inCluster(`"yes"`, selfOnline)},
		{"quorate as text", inCluster(`{"name": "c", "quorate": "yes"}`, selfOnline)},
		{"quorate as null", inCluster(`{"name": "c", "quorate": null}`, selfOnline)},
		{"quorate above one", inCluster(`{"name": "c", "quorate": 2}`, selfOnline)},
		{"quorate below zero", inCluster(`{"name": "c", "quorate": -1}`, selfOnline)},
		{"node without online", inCluster(quorateOne, `"pve1": {"id": 1, "ip": "10.0.0.1"}`)},
		{"node with online as null", inCluster(quorateOne, `"pve1": {"id": 1, "online": null}`)},
		{"node with online above one", inCluster(quorateOne, `"pve1": {"id": 1, "online": 2}`)},
		{"node without a name", inCluster(quorateOne, selfOnline+`, "": {"id": 4, "online": 1}`)},
		{"id as text", inCluster(quorateOne, `"pve1": {"id": "one", "online": 1}`)},
		{"an address that is none", inCluster(quorateOne, `"pve1": {"id": 1, "online": 1, "ip": "not an address"}`)},
		{"an address with a port", inCluster(quorateOne, `"pve1": {"id": 1, "online": 1, "ip": "10.0.0.1:8006"}`)},
		{"a cluster without a node list", `{"nodename": "pve1", "cluster": ` + quorateOne + `}`},
		{"a cluster with an empty node list", inCluster(quorateOne, ``)},
		{"a cluster whose list lacks this node", inCluster(quorateOne, otherOnline)},
		{"a node without an id", inCluster(quorateOne, selfOnline+`, "pve2": {"online": 1, "ip": "10.0.0.2"}`)},
		{"a node with id zero", inCluster(quorateOne, selfOnline+`, "pve2": {"id": 0, "online": 0}`)},
		{"a node with an id below zero", inCluster(quorateOne, `"pve1": {"id": -1, "online": 1, "ip": "10.0.0.1"}`)},
		{"an id with a fraction", inCluster(quorateOne, `"pve1": {"id": 1.0, "online": 1, "ip": "10.0.0.1"}`)},
		{"an id with an exponent", inCluster(quorateOne, `"pve1": {"id": 1e0, "online": 1, "ip": "10.0.0.1"}`)},
		{"this node without an address", inCluster(quorateOne, `"pve1": {"id": 1, "online": 1}`)},
		{"this node with an empty address", inCluster(quorateOne, `"pve1": {"id": 1, "online": 1, "ip": ""}`)},
		{"a count of nodes that is not the list's", inCluster(`{"name": "c", "quorate": 1, "nodes": 3}`, selfOnline+`, `+otherOnline)},
		{"a count of nodes below one", inCluster(`{"name": "c", "quorate": 1, "nodes": 0}`, selfOnline)},
		{"a count of nodes as text", inCluster(`{"name": "c", "quorate": 1, "nodes": "two"}`, selfOnline)},
		{"no cluster, other nodes listed",
			`{"nodename": "pve1", "nodelist": {` + selfOnline + `, ` + otherOnline + `}}`},
		{"no cluster, this node listed", `{"nodename": "pve1", "nodelist": {` + selfOnline + `}}`},
		{"no cluster, an empty list", `{"nodename": "pve1", "nodelist": {}}`},
		{"a null cluster", `{"nodename": "pve1", "cluster": null}`},
		{"an empty cluster", `{"nodename": "pve1", "cluster": {}}`},
		{"an empty cluster, this node listed", `{"nodename": "pve1", "cluster": {}, "nodelist": {` + selfOnline + `}}`},
		{"an empty cluster, other nodes listed",
			`{"nodename": "pve1", "cluster": {}, "nodelist": {` + selfOnline + `, "pve2": {"id": 2, "online": 0}}}`},
		{"a null cluster, only other nodes listed",
			`{"nodename": "pve1", "cluster": null, "nodelist": {` + otherOnline + `}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Parse([]byte(tt.in), t0)
			require.Error(t, err)
			require.Equal(t, Members{}, m)
		})
	}
}

func TestParseDoesNotInventWhatTheFileLeavesOut(t *testing.T) {
	// Another node's address is what corosync last knew of it, and a node it
	// never saw has none.
	in := `{"nodename": "pve1", "cluster": {"name": "c", "quorate": 1, "nodes": 3}, "future": {"x": 1},
		"nodelist": {
			"pve1": {"id": 1, "online": 1, "ip": "10.0.0.1", "more": true},
			"pve2": {"id": 2, "online": 0},
			"pve3": {"id": 3, "online": 1, "ip": ""}
		}}`
	m, err := Parse([]byte(in), t0)
	require.NoError(t, err)
	require.Equal(t, []Member{
		{Name: "pve1", ID: 1, Online: true, IP: addr("10.0.0.1")},
		{Name: "pve2", ID: 2, Online: false},
		{Name: "pve3", ID: 3, Online: true},
	}, m.Nodes)
	require.False(t, m.Nodes[1].IP.IsValid())
	require.False(t, m.Nodes[2].IP.IsValid())
}

func TestAClusterWithoutANameIsStillACluster(t *testing.T) {
	m, err := Parse([]byte(inCluster(`{"quorate": 0}`, selfOnline)), t0)
	require.NoError(t, err)
	require.False(t, m.Standalone)
	require.False(t, m.Quorate)
	require.Empty(t, m.Cluster)
}

// pmxcfs prints the cluster and the list of nodes together, or neither: a
// node that stands alone has only its name and a version of 0, and is the
// one node of its own list.
func TestAFileWithNeitherAClusterNorAListIsAStandaloneNode(t *testing.T) {
	for name, in := range map[string]string{
		"the name only":     `{"nodename": "pve1"}`,
		"with a version":    `{"nodename": "pve1", "version": 0}`,
		"with other fields": `{"nodename": "pve1", "version": 0, "future": {"x": 1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			m, err := Parse([]byte(in), t0)
			require.NoError(t, err)
			require.Equal(t, Members{
				Self: "pve1", Standalone: true, Quorate: true,
				Nodes:  []Member{{Name: "pve1", Online: true}},
				ReadAt: t0,
			}, m)
		})
	}
}

// writeBeside puts the fixture where a members file is, in a directory of its
// own, so that a corosync.conf can be put beside it.
func writeBeside(t *testing.T, name string) (dir, path string) {
	t.Helper()
	b, err := os.ReadFile(fixture(t, name))
	require.NoError(t, err)
	dir = t.TempDir()
	path = filepath.Join(dir, ".members")
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return dir, path
}

func writeCorosyncBeside(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "corosync.conf"), []byte("totem {\n}\n"), 0o600))
}

// pmxcfs prints no cluster before corosync has told it about one, and every
// member has a corosync.conf: that file is what tells a node that was never in
// a cluster from one that has not heard from its cluster yet.
func TestAFileWithoutAClusterIsStandaloneOnlyWhileThereIsNoCorosyncConfig(t *testing.T) {
	dir, p := writeBeside(t, "members-lab-standalone.json")
	m, err := Read(p, t0)
	require.NoError(t, err)
	require.True(t, m.Standalone)

	writeCorosyncBeside(t, dir)
	m, err = Read(p, t0)
	require.ErrorContains(t, err, "corosync.conf")
	require.Equal(t, Members{}, m)
}

// The node's own copy of corosync.conf, which corosync starts from, is
// there as well on every member, and is what is left when the cluster
// filesystem has nothing to show yet.
func TestAFileWithoutAClusterIsStandaloneOnlyWhileTheNodeHasNoCorosyncConfigOfItsOwn(t *testing.T) {
	_, p := writeBeside(t, "members-lab-standalone.json")
	local := filepath.Join(t.TempDir(), "corosync.conf")
	files := Files{Members: p, Corosync: filepath.Join(filepath.Dir(p), "corosync.conf"), LocalCorosync: local}
	m, err := ReadFiles(files, t0)
	require.NoError(t, err)
	require.True(t, m.Standalone)

	require.NoError(t, os.WriteFile(local, []byte("totem {\n}\n"), 0o600))
	m, err = ReadFiles(files, t0)
	require.ErrorContains(t, err, local)
	require.Equal(t, Members{}, m)

	q := NewQuorumFiles(files, (&clock{t: t0}).now)
	_, err = q.Read()
	require.Error(t, err)
}

func TestDefaultFilesAreWhereProxmoxKeepsThem(t *testing.T) {
	require.Equal(t, Files{
		Members: "/etc/pve/.members", Corosync: "/etc/pve/corosync.conf", LocalCorosync: "/etc/corosync/corosync.conf",
	}, DefaultFiles())
}

func TestAFileWithAClusterDoesNotDependOnACorosyncConfig(t *testing.T) {
	dir, p := writeBeside(t, "members-quorate.json")
	m, err := Read(p, t0)
	require.NoError(t, err)
	require.False(t, m.Standalone)

	writeCorosyncBeside(t, dir)
	m, err = Read(p, t0)
	require.NoError(t, err)
	require.False(t, m.Standalone)
}

func TestReadOfAMissingFileIsAnErrorTheCallerCanTell(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), ".members"), t0)
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestReadNamesTheFileItRefused(t *testing.T) {
	_, err := Read(fixture(t, "members-bad-truncated.json"), t0)
	require.ErrorContains(t, err, "members-bad-truncated.json")
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func copyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(fixture(t, name))
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), ".members")
	require.NoError(t, os.WriteFile(p, b, 0o600))
	return p
}

func TestQuorumHasNoLastReadBeforeTheFirst(t *testing.T) {
	q := NewQuorum(fixture(t, "members-quorate.json"), (&clock{t: t0}).now)
	m, at := q.Last()
	require.Equal(t, Members{}, m)
	require.True(t, at.IsZero())
}

func TestQuorumKeepsWhatItRead(t *testing.T) {
	c := &clock{t: t0}
	q := NewQuorum(fixture(t, "members-quorate.json"), c.now)

	m, err := q.Read()
	require.NoError(t, err)
	require.True(t, m.Quorate)
	require.Equal(t, t0, m.ReadAt)

	c.advance(5 * time.Second)
	last, at := q.Last()
	require.Equal(t, m, last)
	require.Equal(t, t0, at, "the time of the read, not of the question")
}

func TestQuorumKeepsTheLastGoodReadWhenTheNextFails(t *testing.T) {
	c := &clock{t: t0}
	p := copyFixture(t, "members-quorate.json")
	q := NewQuorum(p, c.now)
	good, err := q.Read()
	require.NoError(t, err)

	c.advance(2 * time.Second)
	cut, err := os.ReadFile(fixture(t, "members-bad-truncated.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, cut, 0o600))
	m, err := q.Read()
	require.Error(t, err)
	require.Equal(t, Members{}, m)

	last, at := q.Last()
	require.Equal(t, good, last)
	require.Equal(t, t0, at, "the failed read does not renew the time")

	c.advance(2 * time.Second)
	require.NoError(t, os.Remove(p))
	_, err = q.Read()
	require.ErrorIs(t, err, fs.ErrNotExist)
	last, at = q.Last()
	require.Equal(t, good, last)
	require.Equal(t, t0, at)
}

func TestQuorumKeepsTheStandaloneReadWhenACorosyncConfigAppears(t *testing.T) {
	c := &clock{t: t0}
	dir, p := writeBeside(t, "members-lab-standalone.json")
	q := NewQuorum(p, c.now)
	good, err := q.Read()
	require.NoError(t, err)
	require.True(t, good.Standalone)

	c.advance(2 * time.Second)
	writeCorosyncBeside(t, dir)
	_, err = q.Read()
	require.Error(t, err)

	last, at := q.Last()
	require.Equal(t, good, last)
	require.Equal(t, t0, at)
}

func TestQuorumFollowsTheFileOnceItReadsAgain(t *testing.T) {
	c := &clock{t: t0}
	p := copyFixture(t, "members-quorate.json")
	q := NewQuorum(p, c.now)
	_, err := q.Read()
	require.NoError(t, err)

	c.advance(2 * time.Second)
	lost, err := os.ReadFile(fixture(t, "members-not-quorate.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, lost, 0o600))
	m, err := q.Read()
	require.NoError(t, err)
	require.False(t, m.Quorate)

	last, at := q.Last()
	require.False(t, last.Quorate)
	require.Equal(t, t0.Add(2*time.Second), at)
}

func TestQuorumLastCannotBeUsedToChangeWhatItKeeps(t *testing.T) {
	q := NewQuorum(fixture(t, "members-quorate.json"), (&clock{t: t0}).now)
	_, err := q.Read()
	require.NoError(t, err)

	last, _ := q.Last()
	last.Nodes[0].Name = "changed"
	again, _ := q.Last()
	require.Equal(t, "pve1", again.Nodes[0].Name)
}

func TestQuorumIsSafeToUseFromTwoGoroutines(t *testing.T) {
	q := NewQuorum(fixture(t, "members-quorate.json"), (&clock{t: t0}).now)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				_, err := q.Read()
				assert.NoError(t, err)
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				q.Last()
			}
		}()
	}
	wg.Wait()
}
