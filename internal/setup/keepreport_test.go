package setup

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func memoryRun(t *testing.T) (*testEnv, *run) {
	t.Helper()
	e := newTestEnv(t)
	require.NoError(t, e.st.Init())
	return e, &run{Setup: e.s, install: store.Install{ID: testInstall}}
}

func zoneOf(id string) store.RememberedZone {
	return store.RememberedZone{ID: id, Name: id + ".example.com", AccountID: testAccount, CredentialID: "c0ffee00"}
}

func checkedAt(id string) store.CheckedCredential {
	return store.CheckedCredential{CredentialID: id, Report: credentials.Report{Usable: true, CheckedAt: t0}}
}

func TestKeepReportAddsTheReportToTheMemory(t *testing.T) {
	e, r := memoryRun(t)
	require.NoError(t, e.st.SaveEngineMemory(store.EngineMemory{
		InstallID: testInstall, Served: []store.RememberedZone{zoneOf("zone1")}, Reports: []store.CheckedCredential{checkedAt("c0ffee00")},
	}))
	reads := 0

	r.keepReportAfter("c0ffee01", checkedAt("c0ffee01").Report, func() { reads++ })

	m, err := e.st.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, 1, reads)
	require.Equal(t, []store.RememberedZone{zoneOf("zone1")}, m.Served)
	require.Equal(t, []store.CheckedCredential{checkedAt("c0ffee00"), checkedAt("c0ffee01")}, m.Reports)
}

// The daemon may save its memory while setup keeps the report of its check:
// saving what setup read before would put the older memory back.
func TestKeepReportDoesNotPutBackAMemoryThatChangedBeforeTheSave(t *testing.T) {
	e, r := memoryRun(t)
	require.NoError(t, e.st.SaveEngineMemory(store.EngineMemory{InstallID: testInstall, Served: []store.RememberedZone{zoneOf("zone1")}}))
	reads := 0

	r.keepReportAfter("c0ffee01", checkedAt("c0ffee01").Report, func() {
		reads++
		if reads == 1 {
			require.NoError(t, e.st.SaveEngineMemory(store.EngineMemory{
				InstallID: testInstall, Served: []store.RememberedZone{zoneOf("zone1"), zoneOf("zone2")}, Reports: []store.CheckedCredential{checkedAt("c0ffee00")},
			}))
		}
	})

	m, err := e.st.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, 2, reads, "the memory was read once more after it changed")
	require.Equal(t, []store.RememberedZone{zoneOf("zone1"), zoneOf("zone2")}, m.Served, "what the daemon saved stays")
	require.Equal(t, []store.CheckedCredential{checkedAt("c0ffee00"), checkedAt("c0ffee01")}, m.Reports)
	require.Empty(t, e.ask.lines, "nothing to say")
}

// A memory that changes at every read is left as the daemon has it, and the
// daemon checks the token itself.
func TestKeepReportGivesUpOnAMemoryThatKeepsChanging(t *testing.T) {
	e, r := memoryRun(t)
	reads := 0

	r.keepReportAfter("c0ffee01", checkedAt("c0ffee01").Report, func() {
		reads++
		require.NoError(t, e.st.SaveEngineMemory(store.EngineMemory{
			InstallID: testInstall, Served: []store.RememberedZone{zoneOf(fmt.Sprintf("zone%d", reads))},
		}))
	})

	m, err := e.st.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, memoryTries, reads)
	require.Equal(t, []store.RememberedZone{zoneOf("zone3")}, m.Served)
	require.Empty(t, m.Reports)
	require.Contains(t, e.ask.text(), "kept changing")
}
