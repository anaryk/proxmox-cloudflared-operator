package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
