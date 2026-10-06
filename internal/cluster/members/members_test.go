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
			file: "members-standalone.json",
			want: Members{Self: "node1", Standalone: true, Quorate: true},
		},
		{
			file: "members-standalone-empty-cluster.json",
			want: Members{
				Self: "node1", Standalone: true, Quorate: true,
				Nodes: []Member{{Name: "node1", ID: 0, Online: true, IP: addr("192.168.4.209")}},
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
		{"cluster without quorate", `{"nodename": "pve1", "cluster": {"name": "c", "nodes": 2}}`},
		{"cluster of other fields only", `{"nodename": "pve1", "cluster": {"nodes": 2}}`},
		{"cluster name as a number", `{"nodename": "pve1", "cluster": {"name": 7, "quorate": 1}}`},
		{"cluster as text", `{"nodename": "pve1", "cluster": "yes"}`},
		{"quorate as text", `{"nodename": "pve1", "cluster": {"name": "c", "quorate": "yes"}}`},
		{"node without online", `{"nodename": "pve1", "cluster": {"name": "c", "quorate": 1},
			"nodelist": {"pve1": {"id": 1, "ip": "10.0.0.1"}}}`},
		{"node without a name", `{"nodename": "pve1", "cluster": {"name": "c", "quorate": 1},
			"nodelist": {"": {"id": 1, "online": 1}}}`},
		{"id as text", `{"nodename": "pve1", "cluster": {"name": "c", "quorate": 1},
			"nodelist": {"pve1": {"id": "one", "online": 1}}}`},
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
	in := `{"nodename": "pve1", "cluster": {"name": "c", "quorate": 1}, "future": {"x": 1},
		"nodelist": {
			"pve1": {"id": 1, "online": 1, "ip": "10.0.0.1", "more": true},
			"pve2": {"id": 2, "online": 0},
			"pve3": {"id": 3, "online": 1, "ip": "not an address"}
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
	m, err := Parse([]byte(`{"nodename": "pve1", "cluster": {"quorate": 0}}`), t0)
	require.NoError(t, err)
	require.False(t, m.Standalone)
	require.False(t, m.Quorate)
	require.Empty(t, m.Cluster)
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
