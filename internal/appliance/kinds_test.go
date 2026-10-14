package appliance_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// asAgent makes a bootstrap that of an agent joining install installID.
func asAgent(f map[string]any) {
	f["kind"], f["installId"], f["cloudflareToken"] = store.ApplianceAgent, installID, ""
}

func TestAnAgentsBootstrapNamesTheInstallItJoins(t *testing.T) {
	path := writeJSON(t, filepath.Join(t.TempDir(), "bootstrap.json"), with(bootstrapFields("install"), asAgent), 0o600)

	b, err := readAsOwner(path)

	require.NoError(t, err)
	require.Equal(t, store.ApplianceAgent, b.Kind)
	require.Equal(t, installID, b.InstallID)
}

func TestABootstrapOfAKindThatIsNotRightIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(map[string]any)
		want   string
	}{
		{"an unknown kind", func(f map[string]any) { f["kind"] = "worker" }, `kind "worker": want controller or agent`},
		{"the combined kind by name", func(f map[string]any) { f["kind"] = "combined" }, `kind "combined": want controller or agent`},
		{"an agent without an install", func(f map[string]any) { asAgent(f); f["installId"] = "" },
			"an agent joins an install: installId names it"},
		{"an agent with a Cloudflare token", func(f map[string]any) { asAgent(f); f["cloudflareToken"] = cfToken },
			"an agent holds no Cloudflare token"},
		{"an agent recovering", func(f map[string]any) { asAgent(f); f["mode"] = "recover" },
			"an agent is installed or repaired, never recovered"},
		{"an agent with a storage", func(f map[string]any) { asAgent(f); f["storage"] = map[string]any{"id": "local-zfs"} },
			"only a controller records its storage"},
		{"a controller on HA without a storage", func(f map[string]any) {
			f["kind"], f["storage"] = store.ApplianceController, map[string]any{"ha": true}
		}, "storage names no storage"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeJSON(t, filepath.Join(t.TempDir(), "bootstrap.json"), with(bootstrapFields("install"), tt.change), 0o600)

			_, err := readAsOwner(path)

			require.ErrorContains(t, err, tt.want)
			requireNoSecret(t, err.Error())
		})
	}
}

func TestInitOfAnAgentStoresItsPartAndNothingOfAController(t *testing.T) {
	e := newInitEnv(t)

	require.NoError(t, e.init(e.bootstrap("install", asAgent)))

	inst := e.install()
	block := e.block(mac0)
	block.Kind = store.ApplianceAgent
	require.Equal(t, store.Install{ID: installID, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: block}, inst)
	tok, found, err := e.st.PVEToken()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, tokenID, tok.TokenID)
	_, found, err = e.st.Writer()
	require.NoError(t, err)
	require.False(t, found, "an agent draws no writer: it follows the controller's epoch")
	nodes, err := e.st.Nodes()
	require.NoError(t, err)
	require.Empty(t, nodes, "the registry is the controller's")
	require.Empty(t, e.credentials())
	require.FileExists(t, filepath.Join(e.local, "manifest.json"))
	require.Equal(t, []string{
		"check pco@pve!vm9250 at 192.168.4.208:8006 as pve1 with " + filepath.Join(e.local, "pve-ca.pem"),
		"restart pco.service",
	}, e.order)
	e.said("agent: lxc/9250 joins install 0123456789ab")
}

func TestInitOfAnAgentRefusesAVolumeOfAnotherInstall(t *testing.T) {
	e := newInitEnv(t)
	require.NoError(t, e.init(e.bootstrap("install")))
	e.order, e.lines = nil, nil

	err := e.init(e.bootstrap("install", asAgent))

	require.ErrorContains(t, err, "the volume holds install")
	require.Empty(t, e.order)
}

func TestInitOfAControllerRecordsItsKindAndStorage(t *testing.T) {
	e := newInitEnv(t)

	require.NoError(t, e.init(e.bootstrap("install", func(f map[string]any) {
		f["kind"] = store.ApplianceController
		f["storage"] = map[string]any{"id": "ceph-ct", "shared": true, "ha": true}
	})))

	a := e.install().Appliance
	require.Equal(t, store.ApplianceController, a.Kind)
	require.Equal(t, &store.ApplianceStorage{ID: "ceph-ct", Shared: true, HA: true}, a.Storage)
	_, found, err := e.st.Writer()
	require.NoError(t, err)
	require.True(t, found, "the controller writes as a combined appliance does")
}

func TestAKindEncodesAsItIsRead(t *testing.T) {
	dir := t.TempDir()
	b, err := readAsOwner(writeJSON(t, filepath.Join(dir, "one.json"), with(bootstrapFields("install"), func(f map[string]any) {
		f["kind"] = store.ApplianceController
		f["storage"] = map[string]any{"id": "local-zfs", "replication": []string{"9250-0"}}
	}), 0o600))
	require.NoError(t, err)

	data, err := b.Encode()
	require.NoError(t, err)
	again, err := readAsOwner(writeJSON(t, filepath.Join(dir, "two.json"), json.RawMessage(data), 0o600))

	require.NoError(t, err)
	require.Equal(t, b.Kind, again.Kind)
	require.Equal(t, b.Storage, again.Storage)
	require.Equal(t, &store.ApplianceStorage{ID: "local-zfs", Replication: []string{"9250-0"}}, again.Storage)

	plain, err := appliance.Bootstrap{}.Encode()
	require.Error(t, err)
	require.Nil(t, plain)
}

func with(f map[string]any, change func(map[string]any)) map[string]any {
	change(f)
	return f
}
