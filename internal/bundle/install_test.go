package bundle

import (
	"crypto/rand"
	"encoding/json"
	"io/fs"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const installID = "0123456789ab"

// openStore is a store with its roots made, as a node has it.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	base := t.TempDir()
	st, err := store.Open(store.Paths{
		Cluster: filepath.Join(base, "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "local"),
	})
	require.NoError(t, err)
	require.NoError(t, st.Init())
	return st
}

// installed is the store of an install that has been in use: it has left
// observe-only mode, has routes, approvals, acknowledgements, a registry of
// two nodes, a CA, a credential and a writer.
func installed(t *testing.T) *store.Store {
	t.Helper()
	st := openStore(t)
	require.NoError(t, st.SaveInstall(store.Install{
		ID: installID, CreatedAt: t0.Add(-72 * time.Hour), Profile: store.ProfileHost,
		Cluster: &store.ClusterIdentity{CASHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "lab"},
	}))
	settings := store.DefaultSettings()
	settings.ObserveOnly = false
	settings.GateTag = "published"
	settings.DenyHosts = []string{"*.internal.example.com"}
	settings.Admission = store.AdmissionApprove
	require.NoError(t, st.SaveSettings(settings))
	require.NoError(t, st.SaveManualRoute(model.Route{
		Hostname: "grafana.example.com", ManualID: "grafana", Source: model.SourceManual,
		Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: netip.MustParseAddr("192.0.2.10"), Port: 3000},
		Options: model.RouteOptions{NoTLSVerify: true, HostHeader: "grafana.lan"},
	}))
	require.NoError(t, st.SaveManualRoute(model.Route{
		Hostname: "wiki.example.com", ManualID: "wiki", Source: model.SourceManual,
		Target: model.Target{Scheme: model.SchemeHTTP, Port: 8080}, Guest: &model.GuestRef{Kind: model.KindQEMU, VMID: 101},
	}))
	require.NoError(t, st.SaveApproval(store.Approval{
		Owner: "qemu/101", Identity: "qemu/101 on pve1", MACs: []string{"bc:24:11:00:aa:b5"},
		Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.20")},
	}))
	require.NoError(t, st.SaveSegment(store.Segment{Bridge: "vmbr0", AcknowledgedAt: t0.Add(-time.Hour), By: "cli"}))
	require.NoError(t, st.SaveNodeSegment(store.NodeSegment{
		Node: "pve2", Segment: store.Segment{Bridge: "vmbr1", VLAN: 20, AcknowledgedAt: t0.Add(-2 * time.Hour), By: "cli"},
	}))
	index := 1
	require.NoError(t, st.SaveNode(store.NodeEntry{Name: "pve1", Version: "0.2.0", Since: t0.Add(-72 * time.Hour)}))
	require.NoError(t, st.SaveNode(store.NodeEntry{
		Name: "pve2", Version: "0.2.0", Since: t0.Add(-48 * time.Hour), Index: &index, Kind: "host",
		AgentAddress: netip.MustParseAddr("192.0.2.12"), Protocol: store.ProtocolRange{Min: 1, Max: 1},
		CertSerial: "1a2b", State: "active", StateSince: t0.Add(-47 * time.Hour),
	}))
	ca, err := pki.NewCA(installID, t0, rand.Reader)
	require.NoError(t, err)
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	require.NoError(t, st.SaveCA(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}, nil, time.Time{}))
	require.NoError(t, st.SaveCredential(store.Credential{
		ID: "main", Label: "main account", Kind: "scoped", Token: store.NewSecret("cf-token-of-main"), AddedAt: t0.Add(-70 * time.Hour),
	}))
	require.NoError(t, st.SaveCredential(store.Credential{
		ID: "spare", Label: "spare", Kind: "scoped", Token: store.NewSecret("cf-token-of-spare"), AddedAt: t0.Add(-60 * time.Hour),
	}))
	require.NoError(t, st.SaveLeader(store.LeaderRecord{
		Writer: planner.Writer{InstallID: installID, Generation: 7, Nonce: "n0nce", Incarnation: "pve1/boot"},
		Node:   "pve1", AcquiredAt: t0.Add(-24 * time.Hour), CASHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}))
	return st
}

func collect(t *testing.T, st *store.Store, credentials bool) Contents {
	t.Helper()
	c, err := Collect(st, credentials, t0, "0.2.0")
	require.NoError(t, err)
	return c
}

// carried seals the contents and opens them again, as a file does.
func carried(t *testing.T, c Contents) Contents {
	t.Helper()
	got, err := Open(seal(t, c), []byte(passphrase))
	require.NoError(t, err)
	return got
}

func TestCollectReadsWhatTheInstallKeeps(t *testing.T) {
	st := installed(t)

	c := collect(t, st, false)

	require.Equal(t, installID, c.InstallID)
	require.Equal(t, t0.Add(-72*time.Hour), c.InstalledAt)
	require.Equal(t, t0, c.MadeAt)
	require.Equal(t, "0.2.0", c.Build)
	require.Len(t, c.ManualRoutes, 2)
	require.Len(t, c.Approvals, 1)
	require.Len(t, c.Segments, 1)
	require.Len(t, c.NodeSegments, 1)
	require.Len(t, c.Nodes, 2)
	require.NotNil(t, c.CA)
	require.JSONEq(t, `{"installId":"`+installID+`","generation":7,"nonce":"n0nce","incarnation":"pve1/boot","node":"pve1",
		"acquiredAt":"2026-10-05T12:00:00Z","caSHA256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`, string(c.Leader))
	var settings store.Settings
	require.NoError(t, json.Unmarshal(c.Settings, &settings))
	require.Equal(t, "published", settings.GateTag)
	require.False(t, settings.ObserveOnly, "the bundle keeps the settings as they were")
}

func TestCollectKeepsTheKeyOfTheCAWhateverIsAsked(t *testing.T) {
	st := installed(t)
	current, _, _, err := st.CA()
	require.NoError(t, err)

	for _, credentials := range []bool{false, true} {
		c := carried(t, collect(t, st, credentials))

		require.Equal(t, current.CertPEM, c.CA.CertPEM)
		require.Equal(t, current.KeyPEM.Reveal(), c.CA.KeyPEM.Reveal(), "the real key, not the word that stands for it")
		_, err := pki.ParseCA([]byte(c.CA.CertPEM), []byte(c.CA.KeyPEM.Reveal()))
		require.NoError(t, err)
	}
}

func TestCollectLeavesTheCredentialsOutUnlessAsked(t *testing.T) {
	st := installed(t)

	without := collect(t, st, false)
	with := collect(t, st, true)

	require.Empty(t, without.Credentials)
	require.Empty(t, carried(t, without).Credentials)
	require.Len(t, with.Credentials, 2)
	text := string(with.Credentials[0]) + string(with.Credentials[1])
	require.Contains(t, text, "cf-token-of-main", "the token itself, not the word that stands for it")
	require.Contains(t, text, "cf-token-of-spare")
	require.NotContains(t, text, "redacted")
}

func TestWhatWasExportedIsRestoredAsItWas(t *testing.T) {
	from := installed(t)
	to := openStore(t)

	require.NoError(t, Restore(to, carried(t, collect(t, from, true))))

	inst, found, err := to.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store.Install{ID: installID, CreatedAt: t0.Add(-72 * time.Hour), Profile: store.ProfileHost}, inst,
		"the cluster it was made in is not carried: a store in another cluster writes nothing before an admin says so")

	wantSettings, err := from.Settings()
	require.NoError(t, err)
	wantSettings.ObserveOnly = true
	gotSettings, err := to.Settings()
	require.NoError(t, err)
	require.Equal(t, wantSettings, gotSettings, "every setting, in observe-only mode")

	wantRoutes, err := from.ManualRoutes()
	require.NoError(t, err)
	gotRoutes, err := to.ManualRoutes()
	require.NoError(t, err)
	require.Equal(t, wantRoutes, gotRoutes)

	wantApprovals, err := from.Approvals()
	require.NoError(t, err)
	gotApprovals, err := to.Approvals()
	require.NoError(t, err)
	require.Equal(t, wantApprovals, gotApprovals)

	wantSegments, err := from.Segments()
	require.NoError(t, err)
	gotSegments, err := to.Segments()
	require.NoError(t, err)
	require.Equal(t, wantSegments, gotSegments)

	wantOwn, err := from.NodeSegments()
	require.NoError(t, err)
	gotOwn, err := to.NodeSegments()
	require.NoError(t, err)
	require.Equal(t, wantOwn, gotOwn)

	wantNodes, err := from.Nodes()
	require.NoError(t, err)
	gotNodes, err := to.Nodes()
	require.NoError(t, err)
	require.Equal(t, wantNodes, gotNodes, "with the index of each node and the serial of its certificate")

	wantLeader, _, _, err := from.Leader()
	require.NoError(t, err)
	gotLeader, _, found, err := to.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, wantLeader, gotLeader, "what the promote that follows draws its epoch above")

	wantCA, _, _, err := from.CA()
	require.NoError(t, err)
	gotCA, previous, _, err := to.CA()
	require.NoError(t, err)
	require.Nil(t, previous)
	require.Equal(t, wantCA.CertPEM, gotCA.CertPEM)
	require.Equal(t, wantCA.KeyPEM.Reveal(), gotCA.KeyPEM.Reveal())

	wantCreds, err := from.Credentials()
	require.NoError(t, err)
	gotCreds, err := to.Credentials()
	require.NoError(t, err)
	require.Len(t, gotCreds, 2)
	for i := range wantCreds {
		require.Equal(t, wantCreds[i].ID, gotCreds[i].ID)
		require.Equal(t, wantCreds[i].Label, gotCreds[i].Label)
		require.Equal(t, wantCreds[i].AddedAt, gotCreds[i].AddedAt)
		require.Equal(t, wantCreds[i].Token.Reveal(), gotCreds[i].Token.Reveal())
	}
}

func TestWithoutTheCredentialsNoneIsRestored(t *testing.T) {
	from := installed(t)
	to := openStore(t)

	require.NoError(t, Restore(to, carried(t, collect(t, from, false))))

	creds, err := to.Credentials()
	require.NoError(t, err)
	require.Empty(t, creds)
}

func TestAnInstallWithoutACAOrALeaderIsRestoredWithoutThem(t *testing.T) {
	from := openStore(t)
	require.NoError(t, from.SaveInstall(store.Install{ID: installID, CreatedAt: t0, Profile: store.ProfileHost}))
	to := openStore(t)

	c := carried(t, collect(t, from, false))
	require.NoError(t, Restore(to, c))

	require.Nil(t, c.CA)
	require.Empty(t, c.Leader)
	_, _, _, err := to.CA()
	require.ErrorIs(t, err, store.ErrNoCA)
	_, _, found, err := to.Leader()
	require.NoError(t, err)
	require.False(t, found)
	settings, err := to.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly)
}

func TestRestoreSetsObserveOnlyWhateverTheBundleSays(t *testing.T) {
	from := installed(t)
	settings, err := from.Settings()
	require.NoError(t, err)
	require.False(t, settings.ObserveOnly)
	to := openStore(t)

	require.NoError(t, Restore(to, carried(t, collect(t, from, false))))

	got, err := to.Settings()
	require.NoError(t, err)
	require.True(t, got.ObserveOnly, "it publishes nothing until pco apply")
}

func TestAStoreThatHoldsAnInstallIsNotRestoredInto(t *testing.T) {
	c := carried(t, collect(t, installed(t), true))
	to := openStore(t)
	require.NoError(t, to.SaveInstall(store.Install{ID: "ba9876543210", CreatedAt: t0, Profile: store.ProfileHost}))

	err := Restore(to, c)

	require.ErrorIs(t, err, ErrInstalled)
	require.ErrorContains(t, err, "ba9876543210")
	routes, err := to.ManualRoutes()
	require.NoError(t, err)
	require.Empty(t, routes, "nothing was written")
	creds, err := to.Credentials()
	require.NoError(t, err)
	require.Empty(t, creds)
	_, _, _, err = to.CA()
	require.ErrorIs(t, err, store.ErrNoCA)
}

func TestTheSameBundleIsNotRestoredTwice(t *testing.T) {
	c := carried(t, collect(t, installed(t), false))
	to := openStore(t)
	require.NoError(t, Restore(to, c))

	require.ErrorIs(t, Restore(to, c), ErrInstalled)
}

func TestCollectOfAStoreWithoutAnInstallSaysSo(t *testing.T) {
	_, err := Collect(openStore(t), false, t0, "0.2.0")

	require.ErrorIs(t, err, ErrNoInstall)
}

func TestARestoreThatCannotReadTheBundleWritesNothing(t *testing.T) {
	good := carried(t, collect(t, installed(t), true))
	for name, damage := range map[string]func(*Contents){
		"a route with a field no build knows": func(c *Contents) {
			c.ManualRoutes = append(c.ManualRoutes, json.RawMessage(`{"hostname":"x.example.com","manualId":"x","surprise":1}`))
		},
		"settings of another shape": func(c *Contents) { c.Settings = json.RawMessage(`{"pollInterval":"never"}`) },
		"a node that is not an object": func(c *Contents) {
			c.Nodes = append(c.Nodes, json.RawMessage(`[1]`))
		},
		"a leader of another install": func(c *Contents) {
			c.Leader = json.RawMessage(`{"installId":"ba9876543210","generation":1,"nonce":"x"}`)
		},
		"a leader that is not a writer": func(c *Contents) { c.Leader = json.RawMessage(`{"installId":"` + installID + `"}`) },
		"a CA without a key":            func(c *Contents) { c.CA = &CAFiles{CertPEM: c.CA.CertPEM} },
		"a credential whose token was redacted": func(c *Contents) {
			c.Credentials = []json.RawMessage{json.RawMessage(`{"id":"main","label":"m","kind":"scoped","token":"[redacted]","addedAt":"2026-10-01T12:00:00Z"}`)}
		},
		"a credential with no token": func(c *Contents) {
			c.Credentials = []json.RawMessage{json.RawMessage(`{"id":"main","label":"m","kind":"scoped","addedAt":"2026-10-01T12:00:00Z"}`)}
		},
		"a credential that is no object": func(c *Contents) { c.Credentials = []json.RawMessage{json.RawMessage(`"cf-token-in-a-string"`)} },
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			c.ManualRoutes = slices.Clone(good.ManualRoutes)
			damage(&c)
			to := openStore(t)

			err := Restore(to, c)

			require.Error(t, err)
			require.NotErrorIs(t, err, ErrInstalled)
			require.NotContains(t, err.Error(), "cf-token")
			require.Empty(t, stored(t, to), "nothing was written")
		})
	}
}

func TestARestoreThatTheStoreRefusesLeavesNoInstall(t *testing.T) {
	good := carried(t, collect(t, installed(t), true))
	c := good
	c.ManualRoutes = append(slices.Clone(good.ManualRoutes), json.RawMessage(`{"hostname":"not a host","manualId":"y"}`))
	to := openStore(t)

	err := Restore(to, c)

	require.ErrorContains(t, err, "manual routes")
	_, found, ierr := to.Install()
	require.NoError(t, ierr)
	require.False(t, found, "an install that was not restored whole is not there: a run that stops half way is run again")
}

// stored lists the objects of a store, with the root of each.
func stored(t *testing.T, st *store.Store) []string {
	t.Helper()
	p := st.Paths()
	var out []string
	for _, root := range []string{p.Cluster, p.Private} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				out = append(out, path)
			}
			return err
		})
		require.NoError(t, err)
	}
	return out
}

func TestARestoreThatStoppedHalfWayIsRunAgain(t *testing.T) {
	good := carried(t, collect(t, installed(t), false))
	broken := good
	broken.ManualRoutes = append(slices.Clone(good.ManualRoutes), json.RawMessage(`{"hostname":"not a host","manualId":"y"}`))
	to := openStore(t)
	require.Error(t, Restore(to, broken))

	require.NoError(t, Restore(to, good))

	_, found, err := to.Install()
	require.NoError(t, err)
	require.True(t, found)
}

func TestAnInstallWithoutTheTimeItWasMadeIsDatedByTheBundle(t *testing.T) {
	c := carried(t, collect(t, installed(t), false))
	c.InstalledAt = time.Time{}
	to := openStore(t)

	require.NoError(t, Restore(to, c))

	inst, _, err := to.Install()
	require.NoError(t, err)
	require.Equal(t, t0, inst.CreatedAt)
}

func TestARestoredInstallIsAHostInstall(t *testing.T) {
	// The appliance records itself in its install, and what it records is
	// its own: a bundle never carries it.
	c := carried(t, collect(t, installed(t), false))
	to := openStore(t)

	require.NoError(t, Restore(to, c))

	inst, _, err := to.Install()
	require.NoError(t, err)
	require.Equal(t, store.ProfileHost, inst.ProfileName())
	require.Nil(t, inst.Appliance)
	require.Nil(t, inst.Cluster)
}
