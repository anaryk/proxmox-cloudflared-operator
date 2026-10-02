package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const restartLine = "settings gateTag changed since pco started and are read only at start; " +
	"restart pco (systemctl restart pco) for them to take effect"

func TestASettingReadOnlyAtStartThatChangedIsAProblemUntilARestart(t *testing.T) {
	e := newEnv(t)
	started := store.DefaultSettings()
	e.startOnly = func(s store.Settings) []string {
		if s.GateTag != started.GateTag {
			return []string{"gateTag"}
		}
		return nil
	}
	e.eng = e.newEngine()
	st := e.cycle()
	require.NotContains(t, st.Problems, restartLine)

	e.settings(func(s *store.Settings) { s.GateTag = "publish" })
	for range 2 {
		st = e.cycle()
		require.Contains(t, st.Problems, restartLine)
	}

	e.settings(func(s *store.Settings) { s.GateTag = started.GateTag })
	st = e.cycle()
	require.NotContains(t, st.Problems, restartLine)
}
