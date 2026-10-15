package setup

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// oldCluster is the fingerprint of the cluster CA of the cluster a bundle
// was made in, which was lost.
var oldCluster = strings.Repeat("ab", 32)

// restoredElsewhere makes the store the one pco import leaves of a bundle
// made in the cluster old-lab, which was lost: the install, its writer, its
// CA, a credential, and the nodes pve2 and this node's name registered and
// enrolled under that CA. The cluster this node is in now is lab, with pve2.
// It returns the CA of the bundle.
func (e *testEnv) restoredElsewhere() pki.CA {
	t := e.t
	t.Helper()
	ca := e.joinable()
	inst := e.install()
	inst.Cluster = &store.ClusterIdentity{CASHA256: oldCluster, Name: "old-lab"}
	inst.Restored = &store.Restored{BundleMadeAt: t0.Add(-24 * time.Hour)}
	require.NoError(t, e.st.SaveInstall(inst))
	three := 3
	require.NoError(t, e.st.SaveNode(store.NodeEntry{
		Name: testNode, Since: t0.Add(-time.Hour), Index: &three, Kind: "host",
		State: string(agentproto.Active), StateSince: t0.Add(-time.Hour), CertSerial: "01",
	}))
	for _, n := range []string{firstNode, testNode} {
		require.NoError(t, e.st.SaveEnrolment(n, store.Enrolment{CertPEM: e.oldLeaf(ca, n)}))
	}
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 3))
	return ca
}

// oldLeaf is a client certificate of node issued under ca, in PEM.
func (e *testEnv) oldLeaf(ca pki.CA, node string) string {
	e.t.Helper()
	key, err := pki.NewKey(e.s.rand)
	require.NoError(e.t, err)
	id := pki.Identity{Kind: pki.KindHost, InstallID: testInstall, Node: node}
	csr, err := pki.NewCSR(key, id, e.s.rand)
	require.NoError(e.t, err)
	cert, err := ca.IssueClient(csr, id, t0.Add(-time.Hour), e.s.rand)
	require.NoError(e.t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

// currentCA reads the CA of the install, and whether a previous one is kept.
func (e *testEnv) currentCA() (pki.CA, bool) {
	e.t.Helper()
	current, previous, _, err := e.st.CA()
	require.NoError(e.t, err)
	ca, err := pki.ParseCA([]byte(current.CertPEM), []byte(current.KeyPEM.Reveal()))
	require.NoError(e.t, err)
	return ca, previous != nil
}

// The first node set up in a new cluster takes a store restored from a
// bundle of the cluster that was lost: a new CA, so that nothing the nodes of
// that cluster hold is trusted, every node of it removed, to enrol again
// with pco setup, and the store left of the cluster it was made in, so that
// the daemon writes nothing until pco promote.
func TestTheFirstNodeTakesARestoredStoreIntoANewCluster(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	inst := e.install()
	require.Equal(t, &store.ClusterIdentity{CASHA256: oldCluster, Name: "old-lab"}, inst.Cluster,
		"the cluster is made this one's by pco promote, which the daemon waits for")
	require.Equal(t, 3, e.writer().Generation, "leader.json stays the bundle's: the promotion draws above it")

	ca, previous := e.currentCA()
	require.Equal(t, &store.Restored{BundleMadeAt: t0.Add(-24 * time.Hour), TakenOverBy: testNode, CASHA256: ca.SHA256()}, inst.Restored)
	require.NotEqual(t, old.SHA256(), ca.SHA256(), "a new CA")
	require.False(t, previous, "the CA of the bundle is not kept as the previous one")
	leader, _, found, err := e.st.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ca.SHA256(), leader.CASHA256)
	require.NoError(t, e.agentCert().CheckSignatureFrom(ca.Cert))

	nodes := e.nodes()
	require.Equal(t, string(agentproto.Removed), nodes[firstNode].State, "a node of the lost cluster enrols again")
	require.Equal(t, t0, nodes[firstNode].StateSince)
	require.Equal(t, 0, *nodes[firstNode].Index, "its name keeps its index")
	_, found, err = e.st.Enrolment(firstNode)
	require.NoError(t, err)
	require.False(t, found, "its enrolment is gone")
	self := nodes[testNode]
	require.Empty(t, self.State, "this node is the first one: it gets its state at the first start of the daemon")
	require.Equal(t, 3, *self.Index)
	require.Equal(t, e.enrolment().Serial, self.CertSerial)

	settings, err := e.st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly)
	e.requireShown("store: install " + testInstall + " kept, restored from a bundle made on " + dateOf(t0.Add(-24*time.Hour)))
	e.requireShown("cluster: took the store of cluster old-lab into this one")
	e.requireShown("pco promote")
	require.NotContains(t, e.ask.text(), "joining the nodes")
	e.requireNoSecret()

	// Run again, it keeps what it did.
	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()
	again, _ := e.currentCA()
	require.Equal(t, ca.SHA256(), again.SHA256())
}

// While a connector of the install is connected at Cloudflare, the cluster
// the bundle was made in still runs it: nothing is taken over.
func TestARestoredStoreIsNotTakenOverWhileTheOldClusterRunsIt(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	e.cf.SetConnectors(testAccount, tunnelID, []cfapi.Connector{{ID: "conn-1", Connections: 4, OriginIP: "203.0.113.7"}})
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "the cluster the bundle was made in still runs install "+testInstall)
	require.ErrorContains(t, err, "203.0.113.7")
	require.ErrorContains(t, err, "nothing was changed")
	e.done()
	requireUntaken(t, e, old)
}

// Without a Cloudflare token there is no telling whether the old cluster
// still runs the install.
func TestARestoredStoreWithoutACredentialNeedsAToken(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	require.NoError(t, e.st.DeleteCredential("c0ffee00"))
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "--cf-token-file")
	e.done()
	requireUntaken(t, e, old)

	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), cloudflaredKept(), daemonIs("inactive"), serviceStarted())
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode, CloudflareToken: cfToken}))
	e.done()
	require.Equal(t, testNode, e.install().Restored.TakenOverBy)
}

// A Cloudflare that cannot be read cannot say that the old cluster stopped.
func TestARestoredStoreIsNotTakenOverWhenCloudflareCannotBeRead(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	e.api = func(api cfapi.API) cfapi.API { return unreadableConnectors{api} }
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "cannot tell whether the cluster the bundle was made in still runs install "+testInstall)
	e.done()
	requireUntaken(t, e, old)
}

// A second node of the new cluster waits until the first one is promoted.
func TestAnotherNodeWaitsForThePromotionOfTheNodeThatTookTheStore(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options Options
	}{
		{"setup", Options{Yes: true, Node: testNode}},
		{"repair", Options{Yes: true, Repair: true, Node: testNode}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			old := e.restoredElsewhere()
			inst := e.install()
			inst.Restored.TakenOverBy = firstNode
			require.NoError(t, e.st.SaveInstall(inst))
			e.script(preflight("9.0.10"))

			err := e.setup(tt.options)

			require.ErrorContains(t, err, "node "+firstNode+" took it into this cluster")
			require.ErrorContains(t, err, "pco promote on "+firstNode)
			e.done()
			requireUntaken(t, e, old)
		})
	}
}

// requireUntaken checks that a restored store was left as pco import left it.
func requireUntaken(t *testing.T, e *testEnv, old pki.CA) {
	t.Helper()
	inst := e.install()
	require.Equal(t, oldCluster, inst.Cluster.CASHA256)
	ca, _ := e.currentCA()
	require.Equal(t, old.SHA256(), ca.SHA256(), "the CA is the bundle's")
	require.Equal(t, string(agentproto.Active), e.node(firstNode).State)
	_, found, err := e.st.Enrolment(firstNode)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "01", e.node(testNode).CertSerial, "nothing is issued")
	require.NotContains(t, strings.Join(e.run.ran, "\n"), "firewall")
}

type unreadableConnectors struct{ cfapi.API }

func (unreadableConnectors) Connectors(context.Context, string, string) ([]cfapi.Connector, error) {
	return nil, errors.New("internal server error")
}

// A take-over cut short after this node claimed the store is done again by
// the next setup on the node.
func TestATakeOverCutShortIsDoneAgain(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	inst := e.install()
	inst.Restored.TakenOverBy = testNode
	require.NoError(t, e.st.SaveInstall(inst))
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	ca, _ := e.currentCA()
	require.NotEqual(t, old.SHA256(), ca.SHA256())
	require.Equal(t, ca.SHA256(), e.install().Restored.CASHA256)
	require.Equal(t, string(agentproto.Removed), e.node(firstNode).State)
}

// Of two nodes set up at once, the one that claims the store first takes it;
// the other changes nothing.
func TestOfTwoNodesSetUpAtOnceOneTakesTheStore(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	e.api = func(api cfapi.API) cfapi.API {
		return claimedMeanwhile{API: api, claim: func() {
			inst := e.install()
			inst.Restored.TakenOverBy = firstNode
			require.NoError(t, e.st.SaveInstall(inst))
		}}
	}
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "node "+firstNode+" took it into this cluster")
	e.done()
	requireUntaken(t, e, old)
}

// claimedMeanwhile lets another node claim the store while this one looks
// at Cloudflare.
type claimedMeanwhile struct {
	cfapi.API
	claim func()
}

func (c claimedMeanwhile) Connectors(ctx context.Context, account, tunnel string) ([]cfapi.Connector, error) {
	c.claim()
	return c.API.Connectors(ctx, account, tunnel)
}

// A bundle that records no cluster cannot be told from one of this cluster.
func TestARestoredStoreWithoutAClusterIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	inst := e.install()
	inst.Cluster = nil
	require.NoError(t, e.st.SaveInstall(inst))
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "records no cluster")
	require.ErrorContains(t, err, "pco uninstall --keep-cloudflare, then adopt the install from Cloudflare with pco setup --recover")
	e.done()
	require.Nil(t, e.install().Cluster, "nothing is recorded")
	ca, _ := e.currentCA()
	require.Equal(t, old.SHA256(), ca.SHA256())
	require.Equal(t, string(agentproto.Active), e.node(firstNode).State)
}

// A stored credential Cloudflare does not take sees nothing, and is passed
// over while another one looks.
func TestACredentialThatNoLongerWorksIsPassedOver(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.restoredElsewhere()
	require.NoError(t, e.st.SaveCredential(store.Credential{
		ID: "dead0000", Label: "old", Kind: "scoped", Token: store.NewSecret("revoked-token-0123456789abcdef0123456"), AddedAt: t0,
	}))
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	e.requireShown("cloudflare: credential dead0000")
	require.Equal(t, testNode, e.install().Restored.TakenOverBy)
	e.requireNoSecret()
}

// A take-over cut short at any step is finished by the next setup on the
// node that claimed the store, which looks at Cloudflare again first; what
// the install says of it is not taken on its word.
func TestATakeOverCutShortAtAnyStepIsFinishedAfterTheSameChecks(t *testing.T) {
	for _, tt := range []struct {
		name  string
		crash func(e *testEnv)
	}{
		{"after the claim", func(*testEnv) {}},
		{"after the new CA", func(e *testEnv) { e.installCA() }},
		{"recorded done with a node of the lost cluster left", func(e *testEnv) {
			ca := e.installCA()
			inst := e.install()
			inst.Restored.CASHA256 = ca.SHA256()
			require.NoError(e.t, e.st.SaveInstall(inst))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			e.restoredElsewhere()
			inst := e.install()
			inst.Restored.TakenOverBy = testNode
			require.NoError(t, e.st.SaveInstall(inst))
			tt.crash(e)
			half, _ := e.currentCA()

			e.cf.SetConnectors(testAccount, tunnelID, []cfapi.Connector{{ID: "conn-1", Connections: 2, OriginIP: "203.0.113.7"}})
			e.script(preflight("9.0.10"))
			err := e.setup(Options{Yes: true, Node: testNode})
			require.ErrorContains(t, err, "still runs install "+testInstall, "the redo looks at Cloudflare again")
			e.done()
			require.Equal(t, string(agentproto.Active), e.node(firstNode).State)

			e.cf.SetConnectors(testAccount, tunnelID, nil)
			e.script(joined()...)
			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			e.done()

			ca, previous := e.currentCA()
			require.NotEqual(t, half.SHA256(), ca.SHA256(), "a CA made by a run cut short is not kept on its word")
			require.False(t, previous)
			require.Equal(t, ca.SHA256(), e.install().Restored.CASHA256)
			require.Equal(t, string(agentproto.Removed), e.node(firstNode).State)
			done, why, err := e.st.TakenOver(e.install())
			require.NoError(t, err)
			require.True(t, done, why)
		})
	}
}

// What the lost cluster left unfinished does not go on in the new one: its
// rotation of every secret, and the rotations of the forgets of its nodes.
// The take-over makes a new CA, and the promotion gives every tunnel a new
// secret anyway.
func TestATakeOverDropsWhatTheLostClusterLeftUnfinished(t *testing.T) {
	for _, finished := range []bool{false, true} {
		t.Run(map[bool]string{false: "unfinished", true: "finished"}[finished], func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			e.restoredElsewhere()
			rot := store.CARotation{Node: "pve9", StartedAt: t0.Add(-2 * time.Hour), Done: []string{"pve token"}}
			if finished {
				rot.FinishedAt = t0.Add(-time.Hour)
			}
			require.NoError(t, e.st.SaveCARotation(rot))
			n := e.node(firstNode)
			n.Rotation = &store.ForgetRotation{Since: t0.Add(-time.Hour)}
			require.NoError(t, e.st.SaveNode(n))
			e.script(joined()...)

			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			e.done()

			_, found, err := e.st.CARotation()
			require.NoError(t, err)
			require.Equal(t, finished, found, "a finished rotation is only a record of what was done")
			require.Nil(t, e.node(firstNode).Rotation)
		})
	}
}

// pco setup --recover on a store restored from a bundle of another cluster
// takes it over as setup does: the same checks, a new CA, the nodes of the
// lost cluster removed. The record of the restore stays, so that pco promote
// still waits for it and the tunnels still get new secrets.
func TestRecoverOnARestoredStoreTakesItOverAsSetupDoes(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	e.cf.SetConnectors(testAccount, tunnelID, []cfapi.Connector{{ID: "conn-1", Connections: 2, OriginIP: "203.0.113.7"}})
	e.script(preflight("9.0.10"), daemonIs("inactive"))

	err := e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode})

	require.ErrorContains(t, err, "still runs install "+testInstall)
	e.done()
	requireUntaken(t, e, old)
	require.Equal(t, oldCluster, e.install().Cluster.CASHA256)
	require.NotNil(t, e.install().Restored)
	require.Equal(t, 3, e.writer().Generation, "no writer is drawn while the lost cluster runs the install")

	e.cf.SetConnectors(testAccount, tunnelID, nil)
	e.script(preflight("9.0.10"), daemonIs("inactive"), roleKept(), userKept(), tokenKept(), tagsKept(), cloudflaredKept(), serviceStarted())
	require.NoError(t, e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	inst := e.install()
	require.Equal(t, oldCluster, inst.Cluster.CASHA256, "pco promote makes this cluster the store's")
	require.Equal(t, testNode, inst.Restored.TakenOverBy)
	ca, _ := e.currentCA()
	require.NotEqual(t, old.SHA256(), ca.SHA256())
	require.Equal(t, ca.SHA256(), inst.Restored.CASHA256)
	require.Equal(t, string(agentproto.Removed), e.node(firstNode).State)
	require.Equal(t, 4, e.writer().Generation, "above the sentinel at Cloudflare")
}

// The claim of a node that is no longer an online member of the cluster is
// taken over by the next node set up, after the same checks: a node that
// claimed the store and died does not hold the restore up for good. The
// node it was taken from is recorded, for the security event of the
// promotion. A claim of an online node, a take-over under way, is not.
func TestTheClaimOfANodeThatIsGoneIsTakenOver(t *testing.T) {
	for _, tt := range []struct {
		name    string
		members func(e *testEnv)
	}{
		{"not a member any more", func(e *testEnv) { e.clusterOf(firstNode) }},
		{"offline", func(e *testEnv) {
			e.writeFile(e.s.host.members, []byte(`{"nodename":"`+testNode+`","version":7,`+
				`"cluster":{"name":"lab","version":3,"nodes":3,"quorate":1},"nodelist":{`+
				`"`+firstNode+`":{"id":2,"online":1,"ip":"192.0.2.20"},"pve9":{"id":3,"online":0,"ip":"192.0.2.29"},`+
				`"`+testNode+`":{"id":1,"online":1,"ip":"`+testAddr+`"}}}`+"\n"), 0o644)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			e.restoredElsewhere()
			tt.members(e)
			inst := e.install()
			inst.Restored.TakenOverBy = "pve9"
			require.NoError(t, e.st.SaveInstall(inst))
			e.cf.SetConnectors(testAccount, tunnelID, []cfapi.Connector{{ID: "conn-1", Connections: 2}})
			e.script(preflight("9.0.10"))

			err := e.setup(Options{Yes: true, Node: testNode})
			require.ErrorContains(t, err, "still runs install "+testInstall, "the checks come first")
			e.done()
			require.Equal(t, "pve9", e.install().Restored.TakenOverBy)

			e.cf.SetConnectors(testAccount, tunnelID, nil)
			e.script(joined()...)
			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			e.done()

			r := e.install().Restored
			require.Equal(t, testNode, r.TakenOverBy)
			require.Equal(t, "pve9", r.ClaimedFrom)
			e.requireShown("took the claim of node pve9")
		})
	}
}

// --new-install would start a second install beside the restored one and
// leave the restore half done: it is refused.
func TestANewInstallIsRefusedOnARestoredStore(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, NewInstall: true, CloudflareToken: cfToken, Node: testNode})

	require.ErrorContains(t, err, "--new-install")
	require.ErrorContains(t, err, "restored from a bundle")
	e.done()
	requireUntaken(t, e, old)
	require.Equal(t, testInstall, e.install().ID)
}

// A node that claims the store after this one did, and before this one made
// the new CA, keeps it: the CA it may already have made is not replaced.
func TestATakeOverStopsBeforeTheCAWhenTheStoreIsClaimedMeanwhile(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	e.s.rand = claimingRand{Reader: e.s.rand, claim: func() {
		inst := e.install()
		if inst.Restored != nil && inst.Restored.TakenOverBy == testNode {
			inst.Restored.TakenOverBy = firstNode
			require.NoError(t, e.st.SaveInstall(inst))
		}
	}}
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "claimed again")
	e.done()
	requireUntaken(t, e, old)
	require.Equal(t, firstNode, e.install().Restored.TakenOverBy)
}

// claimingRand lets another node claim the store whenever setup draws
// random bytes, as it does to make the new CA.
type claimingRand struct {
	io.Reader
	claim func()
}

func (c claimingRand) Read(p []byte) (int, error) {
	c.claim()
	return c.Reader.Read(p)
}

// A recovery keeps the rotation of the tunnel secrets a promotion of a
// restored store asked for and that did not end: without it, the install
// would not name the record, and the rotation would never go on.
func TestRecoverKeepsTheRotationARestoreAskedFor(t *testing.T) {
	e := newTestEnv(t)
	e.joinable()
	inst := e.install()
	inst.RestoreRotation = "2026-09-30T00:00:00Z/2026-10-01T12:00:00Z"
	require.NoError(t, e.st.SaveInstall(inst))

	got, _, err := adopt(e.st, testInstall, 3, func() time.Time { return t0 }, e.s.rand, store.Install{Profile: store.ProfileHost})

	require.NoError(t, err)
	require.Equal(t, inst.RestoreRotation, got.RestoreRotation)
	require.Equal(t, inst.RestoreRotation, e.install().RestoreRotation)
}

// --recover refuses a restored store that records no cluster before it
// draws a writer, as setup refuses it: nothing is changed.
func TestRecoverRefusesARestoredStoreWithoutAClusterFirst(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	old := e.restoredElsewhere()
	inst := e.install()
	inst.Cluster = nil
	require.NoError(t, e.st.SaveInstall(inst))
	e.script(preflight("9.0.10"), daemonIs("inactive"))

	err := e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode})

	require.ErrorContains(t, err, "records no cluster")
	e.done()
	require.Equal(t, 3, e.writer().Generation, "no writer is drawn")
	require.Nil(t, e.install().Cluster)
	ca, _ := e.currentCA()
	require.Equal(t, old.SHA256(), ca.SHA256())
}
