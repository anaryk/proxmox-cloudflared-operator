package engine

import (
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// pkiInstall is an install id the PKI takes, which the test install's is not.
const pkiInstall = "abc123abc123"

// testCA is a CA under pkiInstall.
func testCA(t *testing.T) pki.CA {
	t.Helper()
	ca, err := pki.NewCA(pkiInstall, t0, rand.Reader)
	require.NoError(t, err)
	return ca
}

// testCertPEM is a client certificate of a host node under a CA of its own.
func testCertPEM(t *testing.T, node string) string {
	t.Helper()
	ca := testCA(t)
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	id := pki.Identity{Kind: pki.KindHost, InstallID: pkiInstall, Node: node}
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueClient(csr, id, t0, rand.Reader)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

// sharedTokenOf is the run token of the shared tunnel in the snapshot last
// served to node.
func sharedTokenOf(t *testing.T, w *world, node string) string {
	t.Helper()
	s, ok := w.r.lastOf(node)
	require.True(t, ok, "node %s was served no snapshot", node)
	for _, tun := range s.Tunnels {
		if tun.Kind == TunnelShared {
			return tun.Token.Reveal()
		}
	}
	t.Fatalf("the snapshot of %s names no shared tunnel", node)
	return ""
}

// Forgetting a node removes its entry and closes its connections before the
// secret of the shared tunnel is rotated: the node's run token stops
// working, and the remaining replicas get the new one. The node is served
// no snapshot again.
func TestForgetRemovesTheNodeAndClosesItBeforeAnythingIsRotated(t *testing.T) {
	w := threeNodes(t)
	log := &callLog{}
	w.useAPI(testToken, hookedAPI{API: w.cf, before: func(method string) { log.add("cf " + method) }})
	w.closeNode = func(node string) { log.add("close " + node) }
	w.settle()
	require.NoError(t, w.store.SaveEnrolment("pve3", store.Enrolment{CertPEM: testCertPEM(t, "pve3")}))
	before := sharedTokenOf(t, w, "pve2")
	n := len(log.all())

	res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	calls := log.all()[n:]
	closed, rotated := slices.Index(calls, "close pve3"), slices.Index(calls, "cf RotateTunnelSecret")
	require.NotEqual(t, -1, closed, calls)
	require.NotEqual(t, -1, rotated, calls)
	require.Less(t, closed, rotated, "closed before the secret is rotated: %v", calls)
	require.Equal(t, store.NodeRemoved, entryOf(t, w.env, "pve3").State)
	_, found, err := w.store.Enrolment("pve3")
	require.NoError(t, err)
	require.False(t, found, "the enrolment is gone")
	require.Equal(t, "pve3", res.Node)
	require.False(t, res.Compromised)
	require.Contains(t, strings.Join(res.Done, "\n"), "the secret of tunnel pco-abc123 in account acc1 is rotated")
	require.Empty(t, res.Pending)

	st := w.tick(time.Second)
	_, served := w.r.lastOf("pve3")
	require.False(t, served, "a removed node is served no snapshot")
	require.NotEqual(t, before, sharedTokenOf(t, w, "pve2"), "the other replicas get the new token")
	require.Contains(t, nodeEventsOf(w.env), "node pve3 is removed, was active")
	require.False(t, hasProblem(st, "pve3"), st.Problems)
}

// With cluster.rotateOnForget off the secret is kept, and the answer says
// that the node's token still works.
func TestForgetKeepsTheSecretWhenTheSettingSaysSo(t *testing.T) {
	w := threeNodes(t)
	log := &callLog{}
	w.useAPI(testToken, hookedAPI{API: w.cf, before: func(method string) { log.add("cf " + method) }})
	w.settle()
	no := false
	w.settings(func(s *store.Settings) { s.Cluster.RotateOnForget = &no })

	res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	require.NotContains(t, log.all(), "cf RotateTunnelSecret")
	require.Contains(t, strings.Join(res.Done, "\n"), "cluster.rotateOnForget is off")
}

// A node is forgotten as compromised only once it left the Proxmox cluster:
// one still in the corosync node list, or online by the members file, would
// receive every new secret through the cluster filesystem.
func TestForgetAsCompromisedIsRefusedWhileTheNodeIsStillInTheCluster(t *testing.T) {
	w := threeNodes(t)
	closed := 0
	w.closeNode = func(string) { closed++ }
	w.settle()
	w.corosync = func() (members.Corosync, error) {
		return members.Corosync{}, errors.New("corosync.conf cannot be read")
	}
	_, err := w.eng.ForgetNode(t.Context(), "pve3", true)
	require.ErrorIs(t, err, ErrRefused, "what cannot be told is refused")
	require.ErrorContains(t, err, "corosync.conf cannot be read")

	w.corosync = func() (members.Corosync, error) {
		return members.Corosync{Nodes: []string{"pve1", "pve2", "pve3"}}, nil
	}
	_, err = w.eng.ForgetNode(t.Context(), "pve3", true)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "run pvecm delnode pve3 first: a node still in the cluster would receive every new secret")
	require.Equal(t, store.NodeActive, entryOf(t, w.env, "pve3").State, "nothing changed")
	require.Zero(t, closed)

	w.corosync = func() (members.Corosync, error) { return members.Corosync{Nodes: []string{"pve1", "pve2"}}, nil }
	_, err = w.eng.ForgetNode(t.Context(), "pve3", true)
	require.ErrorContains(t, err, "pvecm delnode pve3", "still online by the members file")

	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}}
	res, err := w.eng.ForgetNode(t.Context(), "pve3", true)
	require.NoError(t, err)
	require.True(t, res.Compromised)
	require.Equal(t, rotationSteps, res.Pending)
	require.NotEmpty(t, res.NotRotated, "what the disk holds that pco cannot rotate")
	require.Equal(t, 1, closed)
	rec, found, err := w.store.CARotation()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "pve3", rec.Node)
	require.Empty(t, rec.Done)
}

// This node is not forgotten: it is uninstalled.
func TestThisNodeIsNotForgotten(t *testing.T) {
	w := threeNodes(t)
	_, err := w.eng.ForgetNode(t.Context(), testNode, false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "pco uninstall")
}

// A drain takes a node out of the replicas at once; the last replica is
// drained only with force; a node not active yet keeps the intent.
func TestDrainTakesANodeOutOfTheReplicasAndSparesTheLastOne(t *testing.T) {
	w := threeNodes(t)
	w.settle()

	v, err := w.eng.DrainNode(t.Context(), "pve2", false)
	require.NoError(t, err)
	require.Equal(t, store.NodeDraining, v.State)
	require.Equal(t, store.DrainAdmin, v.Drained)
	n := entryOf(t, w.env, "pve2")
	require.Equal(t, store.NodeDraining, n.State)
	require.Equal(t, store.DrainAdmin, n.Drained.Reason)
	again, err := w.eng.DrainNode(t.Context(), "pve2", false)
	require.NoError(t, err, "a drain repeated is no error")
	require.Equal(t, v.State, again.State)

	_, err = w.eng.DrainNode(t.Context(), "pve3", false)
	require.NoError(t, err)
	_, err = w.eng.DrainNode(t.Context(), testNode, false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "the only active replica")
	require.Equal(t, store.NodeActive, entryOf(t, w.env, testNode).State)
	_, err = w.eng.DrainNode(t.Context(), testNode, true)
	require.NoError(t, err)
	require.Equal(t, store.NodeDraining, entryOf(t, w.env, testNode).State)

	st := w.tick(time.Second)
	require.Equal(t, store.NodeDraining, entryOf(t, w.env, "pve2").State, "the registry step keeps it")
	require.Len(t, st.Nodes, 3)
	for _, n := range st.Nodes {
		require.Equal(t, store.DrainAdmin, n.Drained, n.Name)
	}
}

// A node that joins is drained as it is admitted.
func TestDrainOfAJoiningNodeKeepsTheIntent(t *testing.T) {
	e, _, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeJoining, StateSince: t0})
	e.lease = holding(testWriter)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()

	v, err := e.eng.DrainNode(t.Context(), "pve2", false)

	require.NoError(t, err)
	require.Equal(t, store.NodeJoining, v.State)
	require.Equal(t, store.DrainAdmin, v.Drained)
	require.Contains(t, adminEvents(e, time.Time{}), "pve2: node pve2 is joining: it is drained as it is admitted")
}

// An admin's drain ends by undrain; one maintenance mode made ends with the
// mode.
func TestUndrainEndsAnAdminsDrainNotMaintenance(t *testing.T) {
	one, two := 1, 2
	e, _, _ := clusterOf(t,
		store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeDraining, StateSince: t0, Index: &one, Drained: &store.NodeDrain{Reason: store.DrainAdmin, Since: t0}},
		store.NodeEntry{Name: "pve3", Since: t0, State: store.NodeDraining, StateSince: t0, Index: &two, Drained: &store.NodeDrain{Reason: store.DrainMaintenance, Since: t0}},
	)
	e.lease = holding(testWriter)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()

	v, err := e.eng.UndrainNode(t.Context(), "pve2")
	require.NoError(t, err)
	require.Equal(t, store.NodeActive, v.State)
	require.Empty(t, v.Drained)
	require.Nil(t, entryOf(t, e, "pve2").Drained)

	_, err = e.eng.UndrainNode(t.Context(), "pve3")
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "maintenance mode")
	require.Equal(t, store.NodeDraining, entryOf(t, e, "pve3").State)

	_, err = e.eng.UndrainNode(t.Context(), "pve2")
	require.NoError(t, err, "an undrain repeated is no error")
}

// An appliance agent is admitted by the fingerprint of its certificate: at
// least eight digits of it, which must match; a contested node is not.
func TestAdmitMatchesTheFingerprintOfTheEnrolledCertificate(t *testing.T) {
	const sum = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	e, _, _ := clusterOf(t,
		store.NodeEntry{Name: "app1", Since: t0, State: store.NodeJoining, StateSince: t0, Kind: "appliance", VMID: 9001, CertSHA256: sum},
		store.NodeEntry{Name: "app2", Since: t0, State: store.NodeContested, StateSince: t0, Kind: "appliance", VMID: 9002,
			ContestedUses: []store.JoinUse{{At: t0, Source: netip.MustParseAddr("10.0.0.9"), VMID: 9002}, {At: t0.Add(time.Minute), Source: netip.MustParseAddr("10.0.0.10"), VMID: 9003}}},
	)
	e.lease = holding(testWriter)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()

	for _, tt := range []struct {
		name, fp string
		err      error
		contains string
	}{
		{"none for an appliance", "", ErrInvalid, "--fingerprint"},
		{"seven digits", "9f86d08", ErrInvalid, "at least 8 hex digits"},
		{"not hex", "9f86d08g", ErrInvalid, "in hex"},
		{"a wrong one", "9f86d082", ErrRefused, "is not that of the certificate enrolled for node app1"},
		{"longer than a fingerprint", sum + "0", ErrRefused, "is not that of the certificate enrolled for node app1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.eng.AdmitNode(t.Context(), "app1", tt.fp, false)
			require.ErrorIs(t, err, tt.err)
			require.ErrorContains(t, err, tt.contains)
			require.False(t, e.eng.adminAdmitted("app1"))
		})
	}

	// The renewal of an appliance agent's certificate comes with the
	// appliance install; until then it is refused, not taken and dropped.
	_, err := e.eng.AdmitNode(t.Context(), "app1", "9F:86:D0:81", true)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "not available until the appliance install")
	require.False(t, e.eng.adminAdmitted("app1"))

	v, err := e.eng.AdmitNode(t.Context(), "app1", "9F:86:D0:81", false)
	require.NoError(t, err)
	require.Equal(t, "app1", v.Name)
	require.True(t, e.eng.adminAdmitted("app1"))
	evs := adminEvents(e, time.Time{})
	require.Contains(t, evs[len(evs)-1], "trusted by fingerprint 9f86d081")

	_, err = e.eng.AdmitNode(t.Context(), "app2", sum[:8], false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "contested: its join token was used 2 times (2026-10-01T12:00:00Z from 10.0.0.9 as container 9002; 2026-10-01T12:01:00Z from 10.0.0.10 as container 9003)")
	require.ErrorContains(t, err, "pco node forget app2")

	_, err = e.eng.AdmitNode(t.Context(), testNode, "", false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "is active already")

	_, err = e.eng.AdmitNode(t.Context(), "nope", "", false)
	require.ErrorIs(t, err, ErrNotFound)

	// An enrolled value that is no SHA-256 vouches for nothing.
	n := entryOf(t, e, "app1")
	n.CertSHA256 = "9f86d081"
	require.NoError(t, e.store.SaveNode(n))
	_, err = e.eng.AdmitNode(t.Context(), "app1", "9f86d081", false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "has no fingerprint pco can check")
}

// fingerprintMatches takes a prefix of the enrolled SHA-256 only, of at
// least eight digits and never more than the whole.
func TestFingerprintMatches(t *testing.T) {
	const sum = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	require.True(t, fingerprintMatches(sum, "9f86d081"))
	require.True(t, fingerprintMatches(sum, sum))
	require.False(t, fingerprintMatches(sum, "9f86d08"))
	require.False(t, fingerprintMatches(sum, sum+"0"))
	require.False(t, fingerprintMatches(sum, "9f86d082"))
	require.False(t, fingerprintMatches(sum[:60], "9f86d081"), "an enrolled value that is no SHA-256")
	require.False(t, fingerprintMatches("", ""))
}

// A node that stays joining because more routes would move than the setting
// allows is admitted by the admin: every route it fails moves first, and it
// becomes active.
func TestAdminAdmissionLetsEveryFailedRouteMove(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.settings(func(s *store.Settings) { s.Cluster.AdmissionMaxMoves = 1 })
	n := entryOf(t, w.env, "pve3")
	n.State, n.StateSince = store.NodeJoining, w.clock.now()
	require.NoError(t, w.store.SaveNode(n))
	w.acts["pve3"].started = w.clock.now()
	w.acts["pve3"].fail = map[string]string{"www.example.com": "no", "db.example.com": "no"}
	for range 6 {
		w.tick(5 * time.Second)
	}
	require.Equal(t, store.NodeJoining, entryOf(t, w.env, "pve3").State)

	_, err := w.eng.AdmitNode(t.Context(), "pve3", "", false)
	require.NoError(t, err)

	var st State
	for range 40 {
		st = w.tick(5 * time.Second)
		if entryOf(t, w.env, "pve3").State == store.NodeActive {
			break
		}
	}
	require.Equal(t, store.NodeActive, entryOf(t, w.env, "pve3").State, st.Problems)
	require.False(t, w.eng.adminAdmitted("pve3"), "used up")
	w.requireNothingWithdrawn(t, hosts3...)
}

// A hand-off goes to a node that can take the lease: a registered one that
// is neither this node, removed, contested, down nor ineligible.
func TestHandoffReleasesTheLeaseForTheNodeNamed(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	no := false
	n := entryOf(t, w.env, "pve3")
	n.ControllerEligible = &no
	require.NoError(t, w.store.SaveNode(n))

	for _, tt := range []struct{ to, contains string }{
		{testNode, "cannot be handed to itself"},
		{"pve3", "may not hold the lease"},
		{"pve9", "not in the registry"},
	} {
		_, err := w.eng.Handoff(t.Context(), tt.to)
		require.Error(t, err, tt.to)
		require.ErrorContains(t, err, tt.contains)
	}
	require.Empty(t, w.lease.handedTo)

	res, err := w.eng.Handoff(t.Context(), "pve2")
	require.NoError(t, err)
	require.Equal(t, HandoffResult{From: testNode, To: "pve2", Until: w.clock.now().Add(30 * time.Second)}, res)
	require.Equal(t, []string{"pve2"}, w.lease.handedTo)

	_, err = w.eng.Handoff(t.Context(), "")
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "does not hold the lease")
	st := w.cycle()
	require.True(t, hasProblem(st, "does not hold the lease"), st.Problems)
}

// A drained controller resolves no manual route itself: the first replica
// whose agent reports takes them.
func TestADrainedControllerHandsTheManualRoutesToAReplica(t *testing.T) {
	w := threeNodes(t)
	require.NoError(t, w.store.SaveManualRoute(model.Route{
		Hostname: "status.example.com", ManualID: "status", Source: model.SourceManual,
		Target: model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.0.5.20"), Port: 9000},
	}))
	w.settle()
	tg := w.targetOf(t, "pve2", "status.example.com")
	require.False(t, tg.Resolve, "this node resolves it while it is not drained")

	_, err := w.eng.DrainNode(t.Context(), testNode, false)
	require.NoError(t, err)
	w.settle()

	tg = w.targetOf(t, "pve2", "status.example.com")
	require.True(t, tg.Resolve, "the first replica resolves it")
	tg = w.targetOf(t, "pve3", "status.example.com")
	require.False(t, tg.Resolve)

	_, err = w.eng.UndrainNode(t.Context(), testNode)
	require.NoError(t, err)
	w.settle()
	require.False(t, w.targetOf(t, "pve2", "status.example.com").Resolve, "this node takes them back")
}

// The admin ends a block before the ten minutes without a report passed.
func TestUnblockEndsABlockByTheAdmin(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	addr := netip.MustParseAddr("10.0.0.77")
	w.eng.place.blocks[addr] = &conflictBlock{addr: addr, guest: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, mac: dbCard, node: "pve3", port: "tap103i0", why: "conflict", since: w.clock.now(), lastReport: w.clock.now()}

	require.ErrorIs(t, w.eng.Unblock(t.Context(), netip.MustParseAddr("10.0.0.78")), ErrNotFound)
	require.NoError(t, w.eng.Unblock(t.Context(), addr))

	require.Empty(t, w.eng.place.blocks)
	var found bool
	for _, ev := range w.eng.Events(time.Time{}) {
		if ev.Kind == kindBlock && strings.Contains(ev.Message, "ended by the admin") {
			found = ev.Security && ev.Node == "pve3"
		}
	}
	require.True(t, found, "the end is a security event")
	st := w.tick(time.Second)
	require.Empty(t, st.Cluster.Blocks)
}

// SetNode writes what the admin may: whether the node may hold the lease.
func TestSetNodeChangesTheEligibility(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	no := false

	_, err := w.eng.SetNode(t.Context(), "pve2", NodePatch{})
	require.ErrorIs(t, err, ErrInvalid)

	v, err := w.eng.SetNode(t.Context(), "pve2", NodePatch{ControllerEligible: &no})
	require.NoError(t, err)
	require.Equal(t, "pve2", v.Name)
	n := entryOf(t, w.env, "pve2")
	require.NotNil(t, n.ControllerEligible)
	require.False(t, *n.ControllerEligible)
	require.Contains(t, adminEvents(w.env, time.Time{}), "pve2: node pve2 may not hold the lease from now on")
}

// Admin actions on the nodes are the controller's alone.
func TestNodeActionsAreRefusedOffTheController(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	w.lease.set(func(st *lease.State) {
		st.Holder, st.Valid = false, false
		st.Record = store.LeaderRecord{Writer: testWriter, Node: "pve2", AcquiredAt: t0}
	})
	require.NoError(t, w.store.SaveLeader(store.LeaderRecord{Writer: testWriter, Node: "pve2", AcquiredAt: t0}))

	_, err := w.eng.DrainNode(t.Context(), "pve3", false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "node pve2 holds the lease")
	_, err = w.eng.ForgetNode(t.Context(), "pve3", false)
	require.ErrorIs(t, err, ErrRefused)
	_, err = w.eng.AdmitNode(t.Context(), "pve3", "", false)
	require.ErrorIs(t, err, ErrRefused)
	require.Equal(t, store.NodeActive, entryOf(t, w.env, "pve3").State)
}

// A forgotten node's reports do not count: the registry leaves a removed
// entry as it is whatever its agent still sends.
func TestAForgottenNodeThatStillReportsStaysRemoved(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	_, err := w.eng.ForgetNode(t.Context(), "pve3", false)
	require.NoError(t, err)

	for range 3 {
		w.tick(5 * time.Second)
	}

	require.Equal(t, store.NodeRemoved, entryOf(t, w.env, "pve3").State)
	_, served := w.r.lastOf("pve3")
	require.False(t, served)
	st := w.eng.State()
	for _, n := range st.Nodes {
		if n.Name == "pve3" {
			require.Equal(t, agentproto.Removed, agentproto.NodeState(n.State))
		}
	}
}

// Forgetting a node as compromised rotates at Cloudflare, which observe-only
// mode must not: it is refused, with nothing changed, until pco apply.
func TestForgetAsCompromisedIsRefusedInObserveOnlyMode(t *testing.T) {
	w := threeNodes(t)
	closed := 0
	w.closeNode = func(string) { closed++ }
	w.settle()
	*w.listed = []members.Member{{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}}
	w.settings(func(s *store.Settings) { s.ObserveOnly = true })

	_, err := w.eng.ForgetNode(t.Context(), "pve3", true)

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "pco apply")
	require.Equal(t, store.NodeActive, entryOf(t, w.env, "pve3").State, "nothing changed")
	require.Zero(t, closed)
	_, found, err := w.store.CARotation()
	require.NoError(t, err)
	require.False(t, found)
}
