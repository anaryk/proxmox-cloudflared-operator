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
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
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
	first, index := 0, 1
	require.NoError(t, st.SaveNode(store.NodeEntry{Name: "pve1", Version: "0.2.0", Since: t0.Add(-72 * time.Hour), Index: &first}))
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
	// Saved as the controller saves it, with the indexes of the registry.
	require.NoError(t, st.SaveNetworkIndexed(store.Network{
		ID: "vmbr0", Kind: store.NetworkAttach, Bridge: "vmbr0", Prefix: netip.MustParsePrefix("192.0.2.0/24"),
		Legs:   map[string]netip.Addr{"pve1": netip.MustParseAddr("192.0.2.254"), "pve2": netip.MustParseAddr("192.0.2.253")},
		LegMAC: store.LegMACFor(store.NetworkAttach, 1), Mark: 1, Uplink: true, CreatedAt: t0.Add(-40 * time.Hour),
	}, 0, map[string]int{"pve1": first, "pve2": index}))
	_, err = st.AllocateServiceAddr("vmbr0", netip.MustParseAddr("192.0.2.20"), store.ServiceOwner{Owner: "qemu/101", Guest: "qemu/101", Identity: "uuid:101"}, t0.Add(-39*time.Hour))
	require.NoError(t, err)
	_, err = st.AllocateNodeServiceAddr("vmbr1.20", "pve2", netip.MustParseAddr("10.20.0.5"), store.ServiceOwner{Owner: "qemu/101", Guest: "qemu/101", Identity: "uuid:101"}, t0.Add(-38*time.Hour))
	require.NoError(t, err)
	managed := netip.MustParseAddr("10.203.118.10")
	require.NoError(t, st.SaveLease(store.Lease{
		MAC: store.ManagedMAC(managed), Addr: managed, Owner: "qemu/101", Identity: "qemu/101 on pve1", NIC: 1, Since: t0.Add(-30 * time.Hour),
	}, 0))
	require.NoError(t, st.SaveConsent(store.Consent{Owner: "qemu/101", Identity: "qemu/101 on pve1", At: t0.Add(-31 * time.Hour), Actor: "cli"}))
	require.NoError(t, st.SaveAttachment(store.Attachment{
		Owner: "qemu/101", Identity: "qemu/101 on pve1", NIC: 1, MAC: store.ManagedMAC(managed), Addr: managed, Consent: "approval", Since: t0.Add(-30 * time.Hour),
	}))
	require.NoError(t, st.SaveClaims(map[string]planner.Claim{
		"grafana.example.com": {Hostname: "grafana.example.com", Owner: "manual/grafana", Since: t0.Add(-20 * time.Hour)},
	}))
	require.NoError(t, st.Tombstones().Save(t.Context(), map[string]reconcile.Tombstone{
		"zone1/old.example.com": {Since: t0.Add(-10 * time.Hour), Seen: t0.Add(-time.Hour), Generation: 6, Nonce: "n0nce"},
	}))
	require.NoError(t, st.SavePlacements(map[string]store.Placement{
		"grafana.example.com": {Hostname: "grafana.example.com", Tunnel: "tun-1", Kind: store.PlacementShared, Since: t0.Add(-20 * time.Hour)},
	}))
	require.NoError(t, st.SaveMove(store.Move{Owner: "qemu/101", From: "tun-1", To: "tun-2", Step: store.MoveRulesAdded, At: t0.Add(-time.Hour)}))
	require.NoError(t, st.SaveCARotation(store.CARotation{Node: "pve3", StartedAt: t0.Add(-2 * time.Hour), Actor: "cli", Done: []string{"credentials"}}))
	return st
}

// rotated replaces the CA of the store with a new one, as a rotation does,
// keeping the old one as the previous CA.
func rotated(t *testing.T, st *store.Store) (current, previous store.CAFiles) {
	t.Helper()
	previous, _, _, err := st.CA()
	require.NoError(t, err)
	ca, err := pki.NewCA(installID, t0, rand.Reader)
	require.NoError(t, err)
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	current = store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}
	require.NoError(t, st.SaveCA(current, &previous, t0.Add(-time.Hour)))
	return current, previous
}

func collect(t *testing.T, st *store.Store, credentials bool) Contents {
	t.Helper()
	c, err := Collect(t.Context(), st, credentials, t0, "0.2.0")
	require.NoError(t, err)
	return c
}

// same compares what two stores hold of one kind.
func same[T any](t *testing.T, what string, from, to *store.Store, read func(*store.Store) (T, error)) {
	t.Helper()
	want, err := read(from)
	require.NoError(t, err)
	got, err := read(to)
	require.NoError(t, err)
	require.Equal(t, want, got, what)
}

func tombstonesOf(t *testing.T) func(*store.Store) (map[string]reconcile.Tombstone, error) {
	return func(s *store.Store) (map[string]reconcile.Tombstone, error) { return s.Tombstones().Load(t.Context()) }
}

func poolOf(s *store.Store) (netip.Addr, error) {
	next, _, _, err := s.ServicePool()
	return next, err
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

	require.NoError(t, Restore(t.Context(), to, carried(t, collect(t, from, true))))

	inst, found, err := to.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, store.Install{
		ID: installID, CreatedAt: t0.Add(-72 * time.Hour), Profile: store.ProfileHost,
		Cluster: &store.ClusterIdentity{CASHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "lab"},
	}, inst, "with the cluster it was made in: a store in another cluster writes nothing before an admin says so")
	same(t, "networks", from, to, (*store.Store).Networks)
	same(t, "service addresses", from, to, (*store.Store).ServiceAddrs)
	same(t, "service addresses of nodes", from, to, (*store.Store).NodeServiceAddrs)
	same(t, "the pool of service addresses", from, to, poolOf)
	same(t, "leases", from, to, (*store.Store).Leases)
	same(t, "consents", from, to, (*store.Store).Consents)
	same(t, "attachments", from, to, (*store.Store).Attachments)
	same(t, "claims", from, to, (*store.Store).Claims)
	same(t, "tombstones", from, to, tombstonesOf(t))
	same(t, "placements", from, to, (*store.Store).Placements)
	same(t, "moves", from, to, (*store.Store).Moves)
	same(t, "the progress of the rotation", from, to, func(s *store.Store) (store.CARotation, error) {
		r, found, err := s.CARotation()
		require.True(t, found)
		return r, err
	})

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

	require.NoError(t, Restore(t.Context(), to, carried(t, collect(t, from, false))))

	creds, err := to.Credentials()
	require.NoError(t, err)
	require.Empty(t, creds)
}

func TestAnInstallWithoutACAOrALeaderIsRestoredWithoutThem(t *testing.T) {
	from := openStore(t)
	require.NoError(t, from.SaveInstall(store.Install{ID: installID, CreatedAt: t0, Profile: store.ProfileHost}))
	to := openStore(t)

	c := carried(t, collect(t, from, false))
	require.NoError(t, Restore(t.Context(), to, c))

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

	require.NoError(t, Restore(t.Context(), to, carried(t, collect(t, from, false))))

	got, err := to.Settings()
	require.NoError(t, err)
	require.True(t, got.ObserveOnly, "it publishes nothing until pco apply")
}

// The start of observe-only mode the controller recorded goes into the
// bundle, but a restore starts the window anew: the restored install is in
// observe-only mode from the restore on, and the agents' certificates of
// serials it does not know are taken for a day from then, not from when the
// exported install began observing.
func TestTheStartOfObserveOnlyModeIsCarriedButNotRestored(t *testing.T) {
	from := installed(t)
	require.NoError(t, from.SaveObserveSince(store.ObserveSince{Since: t0.Add(-3 * 24 * time.Hour)}))

	c := carried(t, collect(t, from, false))
	require.JSONEq(t, `{"since":"2026-10-03T12:00:00Z"}`, string(c.Observe))

	to := openStore(t)
	require.NoError(t, Restore(t.Context(), to, c))
	_, found, err := to.ObserveSince()
	require.NoError(t, err)
	require.False(t, found)
}

// A bundle whose record of observe-only mode holds what the store has no
// field for is refused, as any other object of it.
func TestABundleWithABadStartOfObserveOnlyModeIsRefused(t *testing.T) {
	c := carried(t, collect(t, installed(t), false))
	c.Observe = json.RawMessage(`{"since":"2026-09-28T12:00:00Z","by":"x"}`)

	err := Restore(t.Context(), openStore(t), c)

	require.ErrorContains(t, err, "the start of observe-only mode of the bundle")
}

func TestAStoreThatHoldsAnInstallIsNotRestoredInto(t *testing.T) {
	c := carried(t, collect(t, installed(t), true))
	to := openStore(t)
	require.NoError(t, to.SaveInstall(store.Install{ID: "ba9876543210", CreatedAt: t0, Profile: store.ProfileHost}))

	err := Restore(t.Context(), to, c)

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
	require.NoError(t, Restore(t.Context(), to, c))

	require.ErrorIs(t, Restore(t.Context(), to, c), ErrInstalled)
}

func TestCollectOfAStoreWithoutAnInstallSaysSo(t *testing.T) {
	_, err := Collect(t.Context(), openStore(t), false, t0, "0.2.0")

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
		"a route with a field inside its target no build knows": func(c *Contents) {
			c.ManualRoutes = append(c.ManualRoutes, json.RawMessage(`{"hostname":"x.example.com","manualId":"x","target":{"surprise":1}}`))
		},
		"a previous CA without the time of the rotation": func(c *Contents) { c.PreviousCA = c.CA },
		"a previous CA without a key": func(c *Contents) {
			c.PreviousCA, c.CARotatedAt = &CAFiles{CertPEM: c.CA.CertPEM}, t0
		},
		"a cluster without a fingerprint": func(c *Contents) { c.Cluster = json.RawMessage(`{"name":"lab"}`) },
		"a claim without a hostname":      func(c *Contents) { c.Claims = []json.RawMessage{json.RawMessage(`{"owner":"qemu/101"}`)} },
		"tombstones of another shape": func(c *Contents) {
			c.Tombstones = json.RawMessage(`{"zone/a":{"generation":1,"surprise":true}}`)
		},
		"a network that is no object": func(c *Contents) { c.Networks = []json.RawMessage{json.RawMessage(`"vmbr0"`)} },
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			c.ManualRoutes = slices.Clone(good.ManualRoutes)
			damage(&c)
			to := openStore(t)

			err := Restore(t.Context(), to, c)

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

	err := Restore(t.Context(), to, c)

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
	require.Error(t, Restore(t.Context(), to, broken))

	require.NoError(t, Restore(t.Context(), to, good))

	_, found, err := to.Install()
	require.NoError(t, err)
	require.True(t, found)
}

func TestARestoreThatStoppedAtItsLastStepIsRunAgainOverWhatItWrote(t *testing.T) {
	// The moves are the last kind written before leader.json and the
	// install: by then every network and lease is in the store, at a
	// revision, and the run again writes over them.
	from := installed(t)
	good := carried(t, collect(t, from, true))
	broken := good
	broken.Moves = []json.RawMessage{json.RawMessage(`{"owner":"qemu/101","from":"tun-1","to":"tun-1","step":"rules-added","at":"2026-10-06T11:00:00Z"}`)}
	to := openStore(t)
	err := Restore(t.Context(), to, broken)
	require.ErrorContains(t, err, "restoring the moves")
	networks, err := to.Networks()
	require.NoError(t, err)
	require.Len(t, networks, 1, "the first run got as far as the moves")
	_, found, err := to.Install()
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, Restore(t.Context(), to, good))

	_, found, err = to.Install()
	require.NoError(t, err)
	require.True(t, found)
	same(t, "networks", from, to, (*store.Store).Networks)
	same(t, "leases", from, to, (*store.Store).Leases)
	same(t, "service addresses", from, to, (*store.Store).ServiceAddrs)
	same(t, "the pool of service addresses", from, to, poolOf)
	same(t, "moves", from, to, (*store.Store).Moves)
}

func TestANetworkWithALegOfANodeWithoutAnIndexIsRestoredAsItWas(t *testing.T) {
	// A store from before the nodes had indexes: the node has none, and the
	// network's leg has no MAC of its own, so it is saved as it was stored.
	from := installed(t)
	require.NoError(t, from.SaveNode(store.NodeEntry{Name: "pve3", Version: "0.1.0", Since: t0.Add(-100 * time.Hour)}))
	require.NoError(t, from.SaveNetwork(store.Network{
		ID: "vmbr5", Kind: store.NetworkAttach, Bridge: "vmbr5", Prefix: netip.MustParsePrefix("10.5.0.0/24"),
		Legs: map[string]netip.Addr{"pve3": netip.MustParseAddr("10.5.0.254")}, LegMAC: store.LegMACFor(store.NetworkAttach, 2),
		Mark: 2, CreatedAt: t0.Add(-90 * time.Hour),
	}, 0))
	networks, err := from.Networks()
	require.NoError(t, err)
	require.Empty(t, networks["vmbr5"].LegMACs, "the shape of such a store")
	to := openStore(t)

	require.NoError(t, Restore(t.Context(), to, carried(t, collect(t, from, false))))

	same(t, "networks", from, to, (*store.Store).Networks)
}

func TestARestoreNeverMergesWithWhatAnotherBundleLeft(t *testing.T) {
	good := carried(t, collect(t, installed(t), true))
	for name, leave := range map[string]struct {
		do   func(*store.Store)
		want string
	}{
		"a node": {func(s *store.Store) {
			require.NoError(t, s.SaveNode(store.NodeEntry{Name: "pve9", Version: "0.2.0", Since: t0}))
		}, "node pve9"},
		"a credential": {func(s *store.Store) {
			require.NoError(t, s.SaveCredential(store.Credential{ID: "old", Label: "old", Kind: "scoped", Token: store.NewSecret("x"), AddedAt: t0}))
		}, "credential old"},
		"a network": {func(s *store.Store) {
			require.NoError(t, s.SaveNetwork(store.Network{
				ID: "vmbr7", Kind: store.NetworkAttach, Bridge: "vmbr7", Prefix: netip.MustParsePrefix("10.7.0.0/24"),
				Legs: map[string]netip.Addr{"pve1": netip.MustParseAddr("10.7.0.254")}, LegMAC: store.LegMACFor(store.NetworkAttach, 2), Mark: 2, CreatedAt: t0,
			}, 0))
		}, "network vmbr7"},
		"a service address": {func(s *store.Store) {
			_, err := s.AllocateServiceAddr("vmbr7", netip.MustParseAddr("10.7.0.5"), store.ServiceOwner{Owner: "qemu/101", Guest: "qemu/101", Identity: "uuid:101"}, t0)
			require.NoError(t, err)
		}, "service address vmbr7 10.7.0.5"},
		"a claim": {func(s *store.Store) {
			require.NoError(t, s.SaveClaims(map[string]planner.Claim{"old.example.com": {Hostname: "old.example.com", Owner: "qemu/9", Since: t0}}))
		}, "claim old.example.com"},
		"a move": {func(s *store.Store) {
			require.NoError(t, s.SaveMove(store.Move{Owner: "qemu/9", From: "a", To: "b", Step: store.MoveRulesAdded, At: t0}))
		}, "move qemu/9"},
	} {
		t.Run(name, func(t *testing.T) {
			to := openStore(t)
			leave.do(to)
			before := stored(t, to)

			err := Restore(t.Context(), to, good)

			require.ErrorIs(t, err, ErrOtherBundle)
			require.ErrorContains(t, err, leave.want)
			require.ErrorContains(t, err, "clear the store, or import the bundle it was restored from")
			require.Equal(t, before, stored(t, to), "nothing was written")
		})
	}
}

func TestAnInstallWithoutTheTimeItWasMadeIsDatedByTheBundle(t *testing.T) {
	c := carried(t, collect(t, installed(t), false))
	c.InstalledAt = time.Time{}
	to := openStore(t)

	require.NoError(t, Restore(t.Context(), to, c))

	inst, _, err := to.Install()
	require.NoError(t, err)
	require.Equal(t, t0, inst.CreatedAt)
}

func TestARestoredInstallIsAHostInstall(t *testing.T) {
	// The appliance records itself in its install, and what it records is
	// its own: a bundle never carries it.
	c := carried(t, collect(t, installed(t), false))
	to := openStore(t)

	require.NoError(t, Restore(t.Context(), to, c))

	inst, _, err := to.Install()
	require.NoError(t, err)
	require.Equal(t, store.ProfileHost, inst.ProfileName())
	require.Nil(t, inst.Appliance)
}

func TestABundleMadeDuringARotationRestoresBothCAs(t *testing.T) {
	// The agents still on the old CA are accepted by the restored controller
	// for the rest of the window, as they would have been by the lost one.
	from := installed(t)
	current, previous := rotated(t, from)
	to := openStore(t)

	c := carried(t, collect(t, from, false))
	require.NoError(t, Restore(t.Context(), to, c))

	require.Equal(t, current.CertPEM, c.CA.CertPEM)
	require.Equal(t, previous.CertPEM, c.PreviousCA.CertPEM)
	gotCurrent, gotPrevious, rotatedAt, err := to.CA()
	require.NoError(t, err)
	require.Equal(t, current.CertPEM, gotCurrent.CertPEM)
	require.Equal(t, current.KeyPEM.Reveal(), gotCurrent.KeyPEM.Reveal())
	require.NotNil(t, gotPrevious)
	require.Equal(t, previous.CertPEM, gotPrevious.CertPEM)
	require.Equal(t, previous.KeyPEM.Reveal(), gotPrevious.KeyPEM.Reveal())
	require.Equal(t, t0.Add(-time.Hour), rotatedAt)
}

func TestSameCATellsARotationThatRanDuringTheExport(t *testing.T) {
	st := installed(t)
	c := collect(t, st, false)
	ok, err := SameCA(st, c)
	require.NoError(t, err)
	require.True(t, ok)

	rotated(t, st)

	ok, err = SameCA(st, c)
	require.NoError(t, err)
	require.False(t, ok, "the current CA changed")
	during := collect(t, st, false)
	ok, err = SameCA(st, during)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, st.SaveCA(*during.CA, nil, during.CARotatedAt))
	ok, err = SameCA(st, during)
	require.NoError(t, err)
	require.False(t, ok, "the previous CA was dropped")

	none := openStore(t)
	require.NoError(t, none.SaveInstall(store.Install{ID: installID, CreatedAt: t0, Profile: store.ProfileHost}))
	ok, err = SameCA(none, collect(t, none, false))
	require.NoError(t, err)
	require.True(t, ok, "no CA, then and now")
}

func TestABundleOfAnEarlierShapeIsRestored(t *testing.T) {
	// A bundle written before the controller's objects were carried holds
	// only the install's decisions, and still restores.
	from := installed(t)
	ca, _, _, err := from.CA()
	require.NoError(t, err)
	caJSON, err := json.Marshal(map[string]string{"certPEM": ca.CertPEM, "keyPEM": ca.KeyPEM.Reveal()})
	require.NoError(t, err)
	text := `{"installId":"` + installID + `","installedAt":"2026-10-03T12:00:00Z","settings":{"gateTag":"published","observeOnly":false},
		"manualRoutes":[{"hostname":"grafana.example.com","manualId":"grafana","source":"manual","target":{"scheme":"http","addr":"192.0.2.10","port":3000}}],
		"nodes":[{"name":"pve1","version":"0.2.0","since":"2026-10-03T12:00:00Z"}],"ca":` + string(caJSON) + `,
		"leader":{"installId":"` + installID + `","generation":7,"nonce":"n0nce","node":"pve1"},"madeAt":"2026-10-06T12:00:00Z","build":"0.2.0"}`
	c, err := Open(sealText(t, []byte(text)), []byte(passphrase))
	require.NoError(t, err)
	to := openStore(t)

	require.NoError(t, Restore(t.Context(), to, c))

	inst, found, err := to.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Nil(t, inst.Cluster)
	routes, err := to.ManualRoutes()
	require.NoError(t, err)
	require.Len(t, routes, 1)
	networks, err := to.Networks()
	require.NoError(t, err)
	require.Empty(t, networks)
	_, previous, _, err := to.CA()
	require.NoError(t, err)
	require.Nil(t, previous)
}
