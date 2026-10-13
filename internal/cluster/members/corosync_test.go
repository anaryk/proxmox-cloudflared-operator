package members

import (
	"bytes"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func boolp(b bool) *bool { return &b }

func TestReadCorosyncFixtures(t *testing.T) {
	tests := []struct {
		file string
		want Corosync
	}{
		{
			file: "corosync-lab-one-node.conf",
			want: Corosync{
				Nodes: []string{"pco-lab-1"},
				Links: map[string][]netip.Addr{"pco-lab-1": {addr("10.254.77.11")}},
			},
		},
		{
			file: "corosync-lab-three-nodes.conf",
			want: Corosync{
				Nodes: []string{"pco-lab-1", "pco-lab-2", "pco-lab-3"},
				Links: map[string][]netip.Addr{
					"pco-lab-1": {addr("10.254.77.11")},
					"pco-lab-2": {addr("10.254.77.12")},
					"pco-lab-3": {addr("10.254.77.13")},
				},
			},
		},
		{
			file: "corosync-lab-qdevice.conf",
			want: Corosync{
				Nodes:        []string{"pco-lab-1", "pco-lab-2"},
				QuorumDevice: true,
				Links: map[string][]netip.Addr{
					"pco-lab-1": {addr("10.254.77.11")},
					"pco-lab-2": {addr("10.254.77.12")},
				},
			},
		},
		{
			file: "corosync-two-node.conf",
			want: Corosync{
				Nodes:   []string{"pve1", "pve2"},
				TwoNode: true,
				Links: map[string][]netip.Addr{
					"pve1": {addr("10.77.0.1")},
					"pve2": {addr("10.77.0.2")},
				},
			},
		},
		{
			file: "corosync-overrides.conf",
			want: Corosync{
				Nodes:         []string{"pve1", "pve2"},
				TwoNode:       true,
				ExpectedVotes: 1,
				WaitForAll:    boolp(false),
				Links: map[string][]netip.Addr{
					"pve1": {addr("10.77.0.1")},
					"pve2": {addr("10.77.0.2")},
				},
			},
		},
		{
			file: "corosync-two-links.conf",
			want: Corosync{
				Nodes: []string{"pve1", "pve2", "pve3"},
				Links: map[string][]netip.Addr{
					"pve1": {addr("10.77.0.1"), addr("fd00:4::1")},
					"pve2": {addr("10.77.0.2"), addr("fd00:4::2")},
					"pve3": {addr("10.77.0.3"), addr("fd00:4::3")},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got, err := ReadCorosync(fixture(t, tt.file))
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestEveryCorosyncFixtureIsReadOrRefusedAsItsNameSays(t *testing.T) {
	files, err := filepath.Glob(fixture(t, "corosync-*.conf"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			c, err := ReadCorosync(f)
			if strings.Contains(filepath.Base(f), "-bad-") {
				require.Error(t, err)
				require.Equal(t, Corosync{}, c)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, c.Nodes)
			require.True(t, slices.IsSorted(c.Nodes))
			for node := range c.Links {
				require.Contains(t, c.Nodes, node)
			}
		})
	}
}

func TestReadCorosyncOfAMissingFileIsAnErrorTheCallerCanTell(t *testing.T) {
	_, err := ReadCorosync(filepath.Join(t.TempDir(), "corosync.conf"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestReadCorosyncNamesTheFileItRefused(t *testing.T) {
	_, err := ReadCorosync(fixture(t, "corosync-bad-truncated.conf"))
	require.ErrorContains(t, err, "corosync-bad-truncated.conf")
}

// A file with one node, and the totem section every corosync.conf ends with.
const (
	nodelist = `
nodelist {
	node {
		name: a
		ring0_addr: 10.0.0.1
	}
}
`
	totem = `
totem {
  version: 2
}
`
	nodes = nodelist + totem
)

func TestParseCorosync(t *testing.T) {
	tests := []struct {
		name, in string
		want     Corosync
	}{
		{
			name: "comments, tabs, blank lines and carriage returns",
			in: "# a comment\r\nnodelist {\r\n\r\n\tnode {\r\n\t\t# name: z\r\n\t\tname:   a  \r\n" +
				"\t\tring0_addr:\t10.0.0.1\r\n\t}\r\n}\r\ntotem {\r\n}\r\n",
			want: Corosync{Nodes: []string{"a"}, Links: map[string][]netip.Addr{"a": {addr("10.0.0.1")}}},
		},
		{
			name: "a number sign inside a value is part of it",
			in:   nodes + "logging {\n  to_file: lab#1\n}\n",
			want: Corosync{Nodes: []string{"a"}, Links: map[string][]netip.Addr{"a": {addr("10.0.0.1")}}},
		},
		{
			name: "links by number and not by position in the file",
			in: `nodelist {
  node {
    name: a
    ring10_addr: 10.0.10.1
    ring2_addr: 10.0.2.1
    ring0_addr: 10.0.0.1
  }
}` + totem,
			want: Corosync{Nodes: []string{"a"}, Links: map[string][]netip.Addr{
				"a": {addr("10.0.0.1"), addr("10.0.2.1"), addr("10.0.10.1")},
			}},
		},
		{
			name: "a link that is a name and no address is left out",
			in: `nodelist {
  node {
    name: a
    ring0_addr: a.example.com
    ring1_addr: 10.0.1.1
  }
  node {
    name: b
    ring0_addr: b.example.com
  }
}` + totem,
			want: Corosync{Nodes: []string{"a", "b"}, Links: map[string][]netip.Addr{"a": {addr("10.0.1.1")}}},
		},
		{
			name: "nodes by name",
			in: `nodelist {
  node {
    name: c
  }
  node {
    name: a
  }
  node {
    name: b
  }
}` + totem,
			want: Corosync{Nodes: []string{"a", "b", "c"}, Links: map[string][]netip.Addr{}},
		},
		{
			name: "votequorum flags as the file sets them",
			in: nodes + `quorum {
  two_node: 0
  wait_for_all: 1
  expected_votes: 5
}`,
			want: Corosync{
				Nodes: []string{"a"}, ExpectedVotes: 5, WaitForAll: boolp(true),
				Links: map[string][]netip.Addr{"a": {addr("10.0.0.1")}},
			},
		},
		{
			name: "a device outside the quorum section is no quorum device",
			in: nodes + `logging {
  device {
    model: net
  }
}`,
			want: Corosync{Nodes: []string{"a"}, Links: map[string][]netip.Addr{"a": {addr("10.0.0.1")}}},
		},
		{
			name: "a node of another section is no node of the cluster",
			in: nodes + `quorum {
  node {
    name: x
  }
}`,
			want: Corosync{Nodes: []string{"a"}, Links: map[string][]netip.Addr{"a": {addr("10.0.0.1")}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCorosync([]byte(tt.in))
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseCorosyncRefuses(t *testing.T) {
	tests := []struct {
		name, in string
	}{
		{"empty", ""},
		{"comments only", "# nothing\n"},
		{"no node", totem},
		{"no totem section", nodelist},
		{"a totem section of another section", nodelist + "quorum {\n  totem {\n  }\n}\n"},
		{"a section left open", "nodelist {\n  node {\n    name: a\n  }\n"},
		{"a section opened at the end", nodes + "quorum {\n"},
		{"a brace that closes nothing", nodes + "}\n"},
		{"a line that is no key and no section", nodes + "quorum\n"},
		{"a section without a name", nodes + "{\n}\n"},
		{"a key without a name", nodes + ": 1\n"},
		{"a node without a name", "nodelist {\n  node {\n    ring0_addr: 10.0.0.1\n  }\n}\n" + totem},
		{"a node with an empty name", "nodelist {\n  node {\n    name:\n  }\n}\n" + totem},
		{"expected votes that are no number", nodes + "quorum {\n  expected_votes: many\n}\n"},
		{"expected votes below zero", nodes + "quorum {\n  expected_votes: -1\n}\n"},
		{"two_node that is no number", nodes + "quorum {\n  two_node: maybe\n}\n"},
		{"wait_for_all that is no number", nodes + "quorum {\n  wait_for_all: sometimes\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := parseCorosync([]byte(tt.in))
			require.Error(t, err)
			require.Equal(t, Corosync{}, c)
		})
	}
}

func TestACorosyncFileCutMidWriteIsAnErrorAndNotAClusterWithoutADevice(t *testing.T) {
	whole, err := os.ReadFile(fixture(t, "corosync-lab-qdevice.conf"))
	require.NoError(t, err)
	// The file ends with its totem section, so a cut between two sections, which
	// no brace gives away, still lacks it. A cut that leaves out only what
	// follows the last brace is the whole file.
	end := bytes.LastIndexByte(whole, '}') + 1
	for cut := 1; cut < end; cut++ {
		_, err := parseCorosync(whole[:cut])
		require.Error(t, err, "a file cut at %d of %d bytes", cut, len(whole))
	}
	_, err = parseCorosync(whole[:end])
	require.NoError(t, err)
}
