package agent

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestAFreshAgentTakesTheFirstEpochOfItsInstall(t *testing.T) {
	e := newEnv(t)

	st := e.apply(e.snapshot("www.example.com -> :8080"))

	require.Equal(t, epoch1, st.EpochSeen)
	require.EqualValues(t, 1, st.SeqApplied)
	got, found, err := e.store.AgentEpoch()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store.Epoch(epoch1), got)
}

func TestASnapshotOfALowerEpochIsRefusedAndReported(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Epoch.Generation = 5
	e.apply(s)
	sets := e.egr.setCount()

	lower := e.snapshot("www.example.com -> :8080", "api.example.com -> :9090")
	lower.Epoch.Generation = 4
	st, err := e.agent.Apply(t.Context(), lower)

	require.ErrorIs(t, err, agentproto.ErrLowerEpoch)
	require.Equal(t, 5, st.EpochSeen.Generation)
	require.EqualValues(t, 1, st.SeqApplied)
	require.Contains(t, st.Problems[0], "refused a desired state")
	require.Contains(t, st.Problems[0], "snapshot of a lower epoch")
	require.Equal(t, sets, e.egr.setCount(), "nothing of it is applied")
	require.Len(t, st.Targets, 1)

	higher := e.snapshot("www.example.com -> :8080")
	higher.Epoch.Generation = 5
	st = e.apply(higher)
	require.NotContains(t, st.Problems, "refused", "the next one it takes clears the refusal")
	require.Empty(t, st.Problems)
}

func TestASeqThatGoesBackIsRefused(t *testing.T) {
	e := newEnv(t)
	e.seq = 7
	e.apply(e.snapshot("www.example.com -> :8080"))

	e.seq = 5
	st, err := e.agent.Apply(t.Context(), e.snapshot("www.example.com -> :8080"))

	require.ErrorIs(t, err, agentproto.ErrSeqBack)
	require.EqualValues(t, 8, st.SeqApplied)

	e.seq = 7
	_, err = e.agent.Apply(t.Context(), e.snapshot("www.example.com -> :8080"))
	require.NoError(t, err, "the same seq again is a re-delivery")
}

func TestASnapshotForAnotherNodeOrInstallIsRefused(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	s.Node = "pve2"
	_, err := e.agent.Apply(t.Context(), s)
	require.ErrorIs(t, err, agentproto.ErrOtherNode)

	s = e.snapshot()
	s.Epoch.InstallID = "zzz999"
	_, err = e.agent.Apply(t.Context(), s)
	require.ErrorIs(t, err, agentproto.ErrOtherInstall)

	s = e.snapshot()
	s.Deny.Reasons = map[string]string{"not an address": "why"}
	_, err = e.agent.Apply(t.Context(), s)
	require.ErrorContains(t, err, "not an address")

	_, found, err := e.store.AgentSnapshot()
	require.NoError(t, err)
	require.False(t, found, "nothing refused is kept")
}

func TestTheDesiredStateAndTheEpochAreKeptAcrossARestart(t *testing.T) {
	e := newEnv(t)
	e.seq = 3
	s := e.snapshot("www.example.com -> :8080")
	s.Epoch.Generation = 2
	e.apply(s)
	raw, found, err := e.store.AgentSnapshot()
	require.NoError(t, err)
	require.True(t, found)
	require.JSONEq(t, encoded(t, s), string(raw), "written as the controller sends it, tokens included")
	info, err := os.Stat(filepath.Join(e.paths.Local, "agent", "desired.json"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	e.agent = e.newAgent()

	v := e.agent.View()
	require.Equal(t, 2, v.Epoch.Generation)
	require.EqualValues(t, 4, v.Seq)
	back := e.snapshot()
	back.Epoch.Generation, back.Seq = 2, 3
	_, err = e.agent.Apply(t.Context(), back)
	require.ErrorIs(t, err, agentproto.ErrSeqBack, "a restart remembers the seq applied")
	lower := e.snapshot()
	lower.Epoch.Generation = 1
	_, err = e.agent.Apply(t.Context(), lower)
	require.ErrorIs(t, err, agentproto.ErrLowerEpoch)
}

func TestANewEpochStartsItsSeqAgain(t *testing.T) {
	e := newEnv(t)
	e.seq = 40
	e.apply(e.snapshot("www.example.com -> :8080"))

	e.seq = 0
	s := e.snapshot("www.example.com -> :8080")
	s.Epoch = agentproto.Epoch{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	st := e.apply(s)

	require.Equal(t, 2, st.EpochSeen.Generation)
	require.EqualValues(t, 1, st.SeqApplied)
}

// The controller sends the same snapshot again while nothing changed, and the
// agent verifies again what it serves: that writes no file, so an idle node
// does not wear its disk.
func TestAnIdleApplyWritesNoFile(t *testing.T) {
	e := newEnv(t)
	s := verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080")
	s.Actions = []agentproto.Action{{ID: "a1", Kind: "doctor", Args: json.RawMessage(`{}`)}}
	e.apply(s)
	before := modTimes(t, e.paths.Local)

	for range 3 {
		e.clock.advance(10 * time.Second)
		e.apply(s)
		_, err := e.agent.Reverify(t.Context())
		require.NoError(t, err)
	}

	require.Equal(t, before, modTimes(t, e.paths.Local))
}

func TestADamagedDesiredStateIsSaidAndTheAgentWaitsForTheController(t *testing.T) {
	e := newEnv(t)
	e.apply(e.snapshot("www.example.com -> :8080"))
	logged, err := json.Marshal(e.snapshot("www.example.com -> :8080"))
	require.NoError(t, err)
	require.NoError(t, e.store.SaveAgentSnapshot(logged), "a snapshot written for a log, its tokens redacted")
	e.conn.reset()

	e.agent = e.newAgent()
	require.NoError(t, e.agent.Boot(t.Context()))

	st := e.agent.Status(t.Context())
	require.Contains(t, st.Problems[0], "the desired state kept on this node cannot be read")
	require.Contains(t, st.Problems[0], "damaged or edited by hand")
	require.Empty(t, e.agent.View().StoodDown, "it waits for the controller and does not stand down")
	require.Empty(t, e.conn.changes(), "nothing is started from it")

	e.apply(e.snapshot("www.example.com -> :8080"))
	require.Empty(t, e.agent.Status(t.Context()).Problems, "the next snapshot replaces it")
}

// modTimes returns the time of every file below dir.
func modTimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[path] = info.ModTime()
		return nil
	}))
	return out
}
