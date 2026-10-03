package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// saveGateTag stores settings with another gate tag, as an admin does who
// changes it.
func (e *testEnv) saveGateTag(tag string) {
	e.t.Helper()
	require.NoError(e.t, e.st.Init())
	settings, err := e.st.Settings()
	require.NoError(e.t, err)
	settings.GateTag = tag
	require.NoError(e.t, e.st.SaveSettings(settings))
}

func TestSetupRegistersTheGateTagOfTheSettings(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	e.saveGateTag("edge")

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	require.Equal(t, []string{"a", "cf-tunnel", "cf-tunnel-managed", "edge"}, h.tags)
	require.Equal(t, []string{"cf-tunnel", "cf-tunnel-managed", "edge"}, e.manifest().RegisteredTags)
	e.requireShown("registered tags: added cf-tunnel, cf-tunnel-managed, edge")
	e.requireShown("tag a guest with edge")
}

func TestSetupRegistersAGateTagChangedAfterwards(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	require.Equal(t, []string{"a", "cf-tunnel", "cf-tunnel-managed"}, h.tags)

	e.saveGateTag("edge")
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	require.Equal(t, []string{"a", "cf-tunnel", "cf-tunnel-managed", "edge"}, h.tags)
	require.Equal(t, []string{"cf-tunnel", "cf-tunnel-managed", "edge"}, e.manifest().RegisteredTags)
	e.requireShown("registered tags: added edge")

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.requireShown("registered tags: nothing needed")
}

func TestRepairRegistersTheGateTagOfTheSettings(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	t.Run("a gate tag changed since", func(t *testing.T) {
		e.saveGateTag("edge")
		require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}))

		require.Equal(t, []string{"a", "cf-tunnel", "cf-tunnel-managed", "edge"}, h.tags)
		require.Equal(t, []string{"cf-tunnel", "cf-tunnel-managed", "edge"}, e.manifest().RegisteredTags)
	})
	t.Run("registered tags a restore took away", func(t *testing.T) {
		h.tags = []string{"a"}
		require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}))

		require.Equal(t, []string{"a", "cf-tunnel", "cf-tunnel-managed", "edge"}, h.tags)
		require.Equal(t, []string{"cf-tunnel", "cf-tunnel-managed", "edge"}, e.manifest().RegisteredTags)
	})
}

func TestDecliningTagsSaysWhatItLeavesOpen(t *testing.T) {
	for _, tt := range []struct {
		name       string
		gate       string
		registered []string
		open       bool
	}{
		{"the default gate tag", "", []string{"a"}, true},
		{"a gate tag of the settings", "edge", []string{"a", "cf-tunnel", "cf-tunnel-managed"}, true},
		{"a gate tag the admin registered", "edge", []string{"a", "edge"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			h := newFakeHost(t)
			h.tags = tt.registered
			e.onHost(h)
			if tt.gate != "" {
				e.saveGateTag(tt.gate)
			}
			gate := tt.gate
			if gate == "" {
				gate = "cf-tunnel"
			}

			require.NoError(t, e.setup(Options{Yes: true, Node: testNode, RegisterTags: ptr(false)}))

			require.Equal(t, tt.registered, h.tags, "nothing is registered")
			require.Empty(t, e.manifest().RegisteredTags)
			if tt.open {
				e.requireShown("warn: registered tags: skipped, and the gate tag " + gate + " is not registered")
				e.requireShown("VM.Config.Options")
				e.requireShown("pco setup --repair")
			} else {
				e.requireShown("info: registered tags: skipped; the gate tag " + gate + " is registered already")
				require.NotContains(t, e.ask.text(), "warn: registered tags")
			}
		})
	}
}

func TestUninstallRemovesOnlyTheTagsSetupRegistered(t *testing.T) {
	for _, tt := range []struct {
		name   string
		before []string
		gate   string
		added  []string
	}{
		{"setup registered the gate tag", []string{"a"}, "edge", []string{"cf-tunnel", "cf-tunnel-managed", "edge"}},
		{"the admin registered the gate tag", []string{"a", "edge"}, "edge", []string{"cf-tunnel", "cf-tunnel-managed"}},
		{"the admin registered a default tag", []string{"a", "cf-tunnel-managed"}, "edge", []string{"cf-tunnel", "edge"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			h := newFakeHost(t)
			h.tags = tt.before
			e.onHost(h)
			e.saveGateTag(tt.gate)
			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			require.Equal(t, tt.added, e.manifest().RegisteredTags)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))

			require.Equal(t, tt.before, h.tags, "what the admin registered stays")
		})
	}
}

func TestRepairDoesNotGuessTheGateTag(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	settings := filepath.Join(e.paths.Cluster, "meta", "settings.json")
	require.NoError(t, os.WriteFile(settings, []byte(`{"gateTag":"Not A Tag"}`), 0o644))
	h.tags = []string{"a"}

	err := e.setup(Options{Yes: true, Repair: true, Node: testNode})

	require.ErrorContains(t, err, "step tags")
	require.ErrorContains(t, err, "reading the settings")
	require.Equal(t, []string{"a"}, h.tags, "no tag is registered on a guess")
}
