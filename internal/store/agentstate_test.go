package store

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTheAgentsSnapshotIsKeptOnTheLocalRoot(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.AgentSnapshot()
	require.NoError(t, err)
	require.False(t, found)

	raw := json.RawMessage(`{"protocol":1,"seq":4,"tunnels":[{"token":"run-token"}]}`)
	require.NoError(t, s.SaveAgentSnapshot(raw))
	got, found, err := s.AgentSnapshot()
	require.NoError(t, err)
	require.True(t, found)
	require.JSONEq(t, string(raw), string(got))
	require.Equal(t, []string{"agent/desired.json"}, stored(t, p.Local))
	require.Empty(t, stored(t, p.Cluster))
	info, err := os.Stat(filepath.Join(p.Local, "agent", "desired.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "it holds run tokens")

	require.NoError(t, s.SaveAgentSnapshot(json.RawMessage(`{"protocol": 1, "seq": 4, "tunnels": [{"token": "run-token"}]}`)))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Local, "agent", "desired.json")), "the same content is not written again")

	require.Error(t, s.SaveAgentSnapshot(json.RawMessage(`{`)))
	require.Error(t, s.SaveAgentSnapshot(json.RawMessage(`null`)))
	require.Error(t, s.SaveAgentSnapshot(nil))
}

func TestTheAgentsEpochIsKeptAndForgotten(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.AgentEpoch()
	require.NoError(t, err)
	require.False(t, found)

	e := Epoch{InstallID: "0123456789ab", Generation: 7, Nonce: "n0nce"}
	require.NoError(t, s.SaveAgentEpoch(e))
	got, found, err := s.AgentEpoch()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, e, got)
	require.Equal(t, []string{"agent/epoch.json"}, stored(t, p.Local))
	var wire map[string]any
	require.NoError(t, json.Unmarshal(readRaw(t, filepath.Join(p.Local, "agent", "epoch.json")).Data, &wire))
	require.Equal(t, map[string]any{"installId": "0123456789ab", "generation": float64(7), "nonce": "n0nce"}, wire)

	for _, bad := range []Epoch{
		{Generation: 1, Nonce: "n"},
		{InstallID: "0123456789ab", Generation: 1},
		{InstallID: "0123456789ab", Generation: -1, Nonce: "n"},
		{InstallID: "0123456789ab", Generation: 1<<53 + 1, Nonce: "n"},
	} {
		require.Error(t, s.SaveAgentEpoch(bad), "%+v", bad)
	}
	require.NoError(t, s.SaveAgentEpoch(Epoch{InstallID: "0123456789ab", Generation: 1 << 53, Nonce: "n"}), "2^53 itself is a generation")

	require.NoError(t, s.DeleteAgentEpoch())
	_, found, err = s.AgentEpoch()
	require.NoError(t, err)
	require.False(t, found)
}

func TestTheAgentsActionsAreKeptSoThatNoneRunsTwice(t *testing.T) {
	s, p := openStore(t)
	got, err := s.AgentActions()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	done := map[string]ActionDone{
		"a1": {Kind: "probe", At: t0},
		"a2": {Kind: "pveWrite", At: t0, Error: "the digest changed"},
	}
	require.NoError(t, s.SaveAgentActions(done))
	got, err = s.AgentActions()
	require.NoError(t, err)
	require.Equal(t, done, got)
	require.Equal(t, []string{"agent/actions.json"}, stored(t, p.Local))

	again, err := Open(p)
	require.NoError(t, err)
	got, err = again.AgentActions()
	require.NoError(t, err)
	require.Equal(t, done, got, "a restart knows them")

	require.Error(t, s.SaveAgentActions(map[string]ActionDone{"": {Kind: "probe", At: t0}}))
	require.Error(t, s.SaveAgentActions(map[string]ActionDone{"a3": {At: t0}}))
}

// The epoch the agent remembers is what tells a store that went back: it
// never moves down but through DeleteAgentEpoch.
func TestTheAgentsEpochNeverGoesBack(t *testing.T) {
	s, _ := openStore(t)
	e := Epoch{InstallID: "0123456789ab", Generation: 7, Nonce: "n0nce"}
	require.NoError(t, s.SaveAgentEpoch(e))
	require.NoError(t, s.SaveAgentEpoch(e), "the same epoch again")

	for name, bad := range map[string]Epoch{
		"a lower generation":           {InstallID: "0123456789ab", Generation: 6, Nonce: "other"},
		"the same generation, another": {InstallID: "0123456789ab", Generation: 7, Nonce: "other"},
		"another install":              {InstallID: "ba9876543210", Generation: 9, Nonce: "n0nce"},
	} {
		err := s.SaveAgentEpoch(bad)
		require.ErrorIs(t, err, ErrEpochBack, name)
	}
	got, _, err := s.AgentEpoch()
	require.NoError(t, err)
	require.Equal(t, e, got)

	higher := Epoch{InstallID: "0123456789ab", Generation: 8, Nonce: "next"}
	require.NoError(t, s.SaveAgentEpoch(higher))
	require.NoError(t, s.DeleteAgentEpoch())
	require.NoError(t, s.SaveAgentEpoch(Epoch{InstallID: "ba9876543210", Generation: 1, Nonce: "n"}), "after a reset any epoch")
}

func TestTheAgentsActionsKeepTheNewestThousand(t *testing.T) {
	s, _ := openStore(t)
	done := map[string]ActionDone{}
	for i := range MaxAgentActions + 5 {
		done[fmt.Sprintf("a%04d", i)] = ActionDone{Kind: "probe", At: t0.Add(time.Duration(i) * time.Second)}
	}
	require.NoError(t, s.SaveAgentActions(done))

	got, err := s.AgentActions()

	require.NoError(t, err)
	require.Len(t, got, MaxAgentActions)
	require.NotContains(t, got, "a0004", "the oldest go")
	require.Contains(t, got, "a0005")
	require.Contains(t, got, fmt.Sprintf("a%04d", MaxAgentActions+4))
}

// The local root is the node's own disk: a snapshot of a large install is
// not held to the 1 MiB of the cluster filesystem.
func TestALocalObjectMayBeLargerThanAFileOfTheClusterFilesystem(t *testing.T) {
	s, p := openStore(t)
	big := json.RawMessage(`{"pad":"` + strings.Repeat("x", 2<<20) + `"}`)

	require.NoError(t, s.SaveAgentSnapshot(big))
	got, found, err := s.AgentSnapshot()
	require.NoError(t, err)
	require.True(t, found)
	var back struct{ Pad string }
	require.NoError(t, json.Unmarshal(got, &back))
	require.Equal(t, 2<<20, len(back.Pad))

	version, _, err := NewestSchema(p)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion(), version, "a large object still tells its schema")

	require.ErrorContains(t, s.AppendSecurityEvent("pve1", SecurityEvent{At: t0, Kind: "enrol", Message: strings.Repeat("y", 2<<20)}),
		"limit", "the cluster root keeps the limit of the cluster filesystem")
}

func TestTheAgentsSnapshotCanBeForgotten(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.DeleteAgentSnapshot(), "a missing one is no error")
	require.NoError(t, s.SaveAgentSnapshot(json.RawMessage(`{"seq":1}`)))

	require.NoError(t, s.DeleteAgentSnapshot())

	_, found, err := s.AgentSnapshot()
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, stored(t, p.Local))
}

func TestTheAgentsEgressSetIsKeptInOrderAndWrittenOnlyWhenItChanges(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.AgentEgress()
	require.NoError(t, err)
	require.False(t, found)

	set := []netip.AddrPort{netip.MustParseAddrPort("10.0.0.12:443"), netip.MustParseAddrPort("10.0.0.11:8080"), netip.MustParseAddrPort("10.0.0.12:443")}
	require.NoError(t, s.SaveAgentEgress(set))
	got, found, err := s.AgentEgress()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:8080"), netip.MustParseAddrPort("10.0.0.12:443")}, got)
	require.Equal(t, []string{"agent/egress.json"}, stored(t, p.Local))

	require.NoError(t, s.SaveAgentEgress(got))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Local, "agent", "egress.json")), "the same set is not written again")

	require.NoError(t, s.SaveAgentEgress(nil))
	got, found, err = s.AgentEgress()
	require.NoError(t, err)
	require.True(t, found, "an empty set is a set")
	require.Empty(t, got)
	require.NotNil(t, got)
}
