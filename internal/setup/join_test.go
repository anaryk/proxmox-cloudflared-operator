package setup

import (
	"crypto/x509"
	"encoding/pem"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

const (
	firstNode = "pve2"
	firstAddr = "192.0.2.20"
)

// joinable makes the store the one of an install set up on firstNode, which
// is active there with index 0: the install with the identity of the cluster
// lab this node is in, the writer, the CA, a credential, the Proxmox token and
// the entry of firstNode. It returns the CA.
func (e *testEnv) joinable() pki.CA {
	t := e.t
	t.Helper()
	e.clusterOf(firstNode)
	require.NoError(t, e.st.Init())
	require.NoError(t, e.st.SaveInstall(store.Install{
		ID: testInstall, CreatedAt: t0.Add(-time.Hour), Profile: store.ProfileHost,
		Cluster: &store.ClusterIdentity{CASHA256: pki.Fingerprint(e.pveCA), Name: "lab"},
	}))
	require.NoError(t, e.st.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 3, Nonce: "abcd1234"}))
	ca := e.installCA()
	require.NoError(t, e.st.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret(pveSecret)}))
	require.NoError(t, e.st.SaveCredential(store.Credential{
		ID: "c0ffee00", Label: "main", Kind: "scoped", Token: store.NewSecret(cfToken), AddedAt: t0,
	}))
	zero := 0
	require.NoError(t, e.st.SaveNode(store.NodeEntry{
		Name: firstNode, Version: version.Version, Since: t0.Add(-time.Hour), Index: &zero, Kind: "host",
		AgentAddress: netip.MustParseAddr(firstAddr), State: string(agentproto.Active), StateSince: t0.Add(-time.Hour),
	}))
	return ca
}

// installCA gives the install a CA and returns it.
func (e *testEnv) installCA() pki.CA {
	e.t.Helper()
	ca, err := pki.NewCA(testInstall, t0.Add(-time.Hour), e.s.rand)
	require.NoError(e.t, err)
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(e.t, err)
	require.NoError(e.t, e.st.SaveCA(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}, nil, time.Time{}))
	return ca
}

// joined is the script of a setup that joins an install set up elsewhere:
// nothing is created in Proxmox, the credential is there, and the daemon is
// started.
func joined() [][]call {
	return [][]call{preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), cloudflaredKept(), serviceStarted()}
}

func (e *testEnv) nodes() map[string]store.NodeEntry {
	e.t.Helper()
	nodes, err := e.st.Nodes()
	require.NoError(e.t, err)
	out := make(map[string]store.NodeEntry)
	for _, n := range nodes {
		out[n.Name] = n
	}
	return out
}

func (e *testEnv) node(name string) store.NodeEntry {
	e.t.Helper()
	n, ok := e.nodes()[name]
	require.True(e.t, ok, "node %s is registered", name)
	return n
}

// enrolment is the enrolment of this node.
func (e *testEnv) enrolment() store.Enrolment {
	e.t.Helper()
	enrol, found, err := e.st.Enrolment(testNode)
	require.NoError(e.t, err)
	require.True(e.t, found, "node %s is enrolled", testNode)
	return enrol
}

func (e *testEnv) agentFile(name string) string { return filepath.Join(e.paths.Local, agentDir, name) }

// agentCert reads the client certificate of the node's agent.
func (e *testEnv) agentCert() *x509.Certificate {
	e.t.Helper()
	cert, err := readCert(e.agentFile(agentCertFile))
	require.NoError(e.t, err)
	return cert
}

func TestSetupOnAFurtherNodeJoinsTheInstall(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	ca := e.joinable()
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	inst := e.install()
	require.Equal(t, testInstall, inst.ID, "the install is kept")
	require.Equal(t, &store.ClusterIdentity{CASHA256: pki.Fingerprint(e.pveCA), Name: "lab"}, inst.Cluster)
	require.Equal(t, 3, e.writer().Generation, "the writer is left alone")
	require.Empty(t, e.cf.Calls(), "nothing is written or read at Cloudflare")

	enrol := e.enrolment()
	cert := e.agentCert()
	id, err := pki.IdentityOf(cert)
	require.NoError(t, err)
	require.Equal(t, pki.Identity{Kind: pki.KindHost, InstallID: testInstall, Node: testNode}, id)
	require.NoError(t, cert.CheckSignatureFrom(ca.Cert), "the certificate is of the install's CA")
	require.Equal(t, pki.Fingerprint(cert), enrol.SHA256)
	require.Equal(t, cert.SerialNumber.Text(16), enrol.Serial)
	requireMode(t, e.agentFile(agentKeyFile), 0o600)
	requireMode(t, e.agentFile(agentCertFile), 0o600)
	caPEM, _, err := ca.PEM()
	require.NoError(t, err)
	require.Equal(t, string(caPEM), e.read(e.agentFile(agentCAFile)))
	server, err := readCert(e.agentFile(serverCertFile))
	require.NoError(t, err)
	require.Equal(t, []string{pki.NodeServerName(testInstall, testNode)}, server.DNSNames)
	require.NoError(t, server.CheckSignatureFrom(ca.Cert))
	requireMode(t, e.agentFile(serverKeyFile), 0o600)

	nodes := e.nodes()
	require.Len(t, nodes, 2)
	require.Equal(t, string(agentproto.Active), nodes[firstNode].State, "the first node is left alone")
	require.Equal(t, store.NodeEntry{
		Name: testNode, Version: version.Version, Since: t0, Kind: "host", AgentAddress: netip.MustParseAddr(testAddr),
		Protocol: store.ProtocolRange{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax}, Schema: store.SchemaVersion(),
		CertSerial: enrol.Serial, CertSHA256: enrol.SHA256, State: string(agentproto.Joining), StateSince: t0,
	}, nodes[testNode], "the node joins without an index, which the controller gives it")

	require.Equal(t, Manifest{Node: testNode, InstalledAt: t0}, e.manifest(), "nothing of the install is this node's to remove")
	e.requireShown("store: install " + testInstall + " kept; joining the nodes " + firstNode)
	e.requireShown("node registry: registered " + testNode + " as joining")
	require.NotContains(t, e.ask.text(), "later release")
	e.requireNoSecret()
}

func TestARemovedNodeDoesNotCountAsAnotherNodeOfTheInstall(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.installed(Manifest{})
	require.NoError(t, e.st.SaveNode(store.NodeEntry{Name: "pve9", Since: t0, State: string(agentproto.Removed), StateSince: t0}))
	e.script(preflight("9.0.10"), addressRead(), roleKept(), userKept(), tokenKept(), tagsKept(), cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()
}

func TestTheAgentNetworkPrefersTheAddressPmxcfsLists(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.addrs = append(e.addrs, netip.MustParseAddr("192.0.2.77"))
	settings, err := e.st.Settings()
	require.NoError(t, err)
	settings.Cluster.AgentNetwork = netip.MustParsePrefix("192.0.2.0/24")
	require.NoError(t, e.st.SaveSettings(settings))
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.Equal(t, netip.MustParseAddr(testAddr), e.node(testNode).AgentAddress)
}

func TestTwoAddressesInTheAgentNetworkAreRefused(t *testing.T) {
	e := newTestEnv(t)
	e.joinable()
	e.addrs = []netip.Addr{netip.MustParseAddr(testAddr), netip.MustParseAddr("10.99.0.10"), netip.MustParseAddr("10.99.0.11")}
	settings, err := e.st.Settings()
	require.NoError(t, err)
	settings.Cluster.AgentNetwork = netip.MustParsePrefix("10.99.0.0/24")
	require.NoError(t, e.st.SaveSettings(settings))
	e.script(preflight("9.0.10"))

	err = e.setup(Options{Yes: true, Node: testNode})

	require.EqualError(t, err, "setup step cluster: this node has 2 addresses in cluster.agentNetwork 10.99.0.0/24 "+
		"(10.99.0.10, 10.99.0.11), and the agent listens on one: leave one of them in it, or narrow the setting")
	e.done()
}

func TestTwoJoinsAtOnceLeaveTwoEntriesWithoutAnIndex(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	// A third node on the same cluster filesystem, with a local root of its own.
	other := newTestEnv(t)
	other.installUnit(serviceUnit)
	other.clusterAs("pve3", "192.0.2.30", firstNode, testNode)
	other.paths.Cluster, other.paths.Private = e.paths.Cluster, e.paths.Private
	other.reopen()
	// The nodes of one cluster share its CA.
	for from, to := range map[string]string{e.s.host.clusterCA: other.s.host.clusterCA, e.s.host.clusterCAKey: other.s.host.clusterCAKey} {
		b, err := os.ReadFile(from)
		require.NoError(t, err)
		other.writeFile(to, b, 0o640)
	}
	e.script(joined()...)
	other.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	require.NoError(t, other.setup(Options{Yes: true, Node: "pve3"}))
	e.done()
	other.done()

	nodes := e.nodes()
	require.Len(t, nodes, 3)
	for _, name := range []string{testNode, "pve3"} {
		require.Nil(t, nodes[name].Index, "%s has no index", name)
		require.Equal(t, string(agentproto.Joining), nodes[name].State)
	}
	require.NotEqual(t, nodes[testNode].CertSerial, nodes["pve3"].CertSerial)
	require.Equal(t, 0, *nodes[firstNode].Index)
}

func TestSetupAgainKeepsTheCertificateAndTheState(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()
	first := e.node(testNode)
	// The controller admitted the node meanwhile.
	seven := 7
	first.Index, first.State, first.StateSince = &seven, string(agentproto.Active), t0.Add(time.Minute)
	require.NoError(t, e.st.SaveNode(first))
	key := e.read(e.agentFile(agentKeyFile))

	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.Equal(t, first, e.node(testNode), "the entry is as the controller left it")
	require.Equal(t, key, e.read(e.agentFile(agentKeyFile)))
	e.requireShown("certificates: kept")
	e.requireShown("node registry: " + testNode + " is registered, nothing needed")
}

func TestSetupRefreshesTheEntryOfARegisteredNodeAndLeavesItsStateAlone(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	three := 3
	require.NoError(t, e.st.SaveNode(store.NodeEntry{
		Name: testNode, Version: "0.1.0", Since: t0.Add(-time.Hour), Index: &three, Kind: "host",
		AgentAddress: netip.MustParseAddr("192.0.2.99"), State: string(agentproto.Draining), StateSince: t0.Add(-time.Minute),
		Drained: &store.NodeDrain{Reason: store.DrainAdmin, Since: t0.Add(-time.Minute)},
	}))
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	n := e.node(testNode)
	require.Equal(t, version.Version, n.Version)
	require.Equal(t, netip.MustParseAddr(testAddr), n.AgentAddress)
	require.Equal(t, e.enrolment().Serial, n.CertSerial)
	require.Equal(t, 3, *n.Index)
	require.Equal(t, string(agentproto.Draining), n.State)
	require.Equal(t, t0.Add(-time.Minute), n.StateSince)
	require.Equal(t, t0.Add(-time.Hour), n.Since)
	require.NotNil(t, n.Drained)
	e.requireShown("node registry: refreshed " + testNode)
}

func TestARemovedNodeJoinsAgainUnderItsOldIndex(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	five := 5
	require.NoError(t, e.st.SaveNode(store.NodeEntry{
		Name: testNode, Version: "0.1.0", Since: t0.Add(-time.Hour), Index: &five, Kind: "host",
		CertSerial: "01", CertSHA256: strings.Repeat("0", 64), State: string(agentproto.Removed), StateSince: t0.Add(-time.Minute),
	}))
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	n := e.node(testNode)
	require.Equal(t, 5, *n.Index, "the name keeps its index, so the leg MACs stay")
	require.Equal(t, string(agentproto.Joining), n.State)
	require.Equal(t, t0, n.StateSince)
	require.NotEqual(t, "01", n.CertSerial, "a new certificate")
	require.Equal(t, e.enrolment().Serial, n.CertSerial)
}

func TestAJoinNeedsTheCAOfTheInstall(t *testing.T) {
	e := newTestEnv(t)
	e.joinable()
	require.NoError(t, os.RemoveAll(filepath.Join(e.paths.Private, "ca")))
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.EqualError(t, err, "setup step cluster: the install has no CA yet, which the node "+firstNode+
		" makes when it runs this release of pco: upgrade and start pco there first")
	_, found, err := e.st.Enrolment(testNode)
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, e.nodes()[testNode].Name, "nothing is registered")
	e.done()
}

func TestANewInstallGetsItsCAAndTheIdentityOfTheCluster(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(freshInstall()...)

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	inst := e.install()
	require.Equal(t, &store.ClusterIdentity{CASHA256: pki.Fingerprint(e.pveCA)}, inst.Cluster, "a node that stands alone has no cluster name")
	current, previous, _, err := e.st.CA()
	require.NoError(t, err)
	require.Nil(t, previous)
	ca, err := pki.ParseCA([]byte(current.CertPEM), []byte(current.KeyPEM.Reveal()))
	require.NoError(t, err)
	require.Equal(t, "pco CA "+inst.ID, ca.Cert.Subject.CommonName)
	leader, _, found, err := e.st.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ca.SHA256(), leader.CASHA256)
	require.NoError(t, e.agentCert().CheckSignatureFrom(ca.Cert))

	n := e.node(testNode)
	require.Empty(t, n.State, "the first node gets its state at the first start of the daemon")
	require.Nil(t, n.Index)
	require.Equal(t, e.enrolment().Serial, n.CertSerial)
	require.Equal(t, netip.MustParseAddr(testAddr), n.AgentAddress)
	e.requireShown("cluster: recorded the identity of this node, which stands alone")
	e.requireShown("cluster CA: created")
	e.requireNoSecret()
}

func TestACAStoredMeanwhileByAnotherNodeIsTheOneUsed(t *testing.T) {
	e := newTestEnv(t)
	require.NoError(t, e.st.Init())
	require.NoError(t, e.st.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "abcd1234"}))
	// The other node stored its CA between this node's look and its create.
	theirs := e.installCA()
	r := &run{Setup: e.s, o: Options{Node: testNode}, install: store.Install{ID: testInstall}}

	ca, err := r.createCA()

	require.NoError(t, err)
	require.Equal(t, theirs.SHA256(), ca.SHA256(), "this node issues under the CA stored first")
	leader, _, found, err := e.st.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, theirs.SHA256(), leader.CASHA256, "leader.json names the stored CA")
	e.requireShown("cluster CA: another node made it meanwhile")
}

func TestSetupFillsTheClusterNameOfAnInstallMadeAlone(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	inst := e.install()
	inst.Cluster = &store.ClusterIdentity{CASHA256: pki.Fingerprint(e.pveCA)}
	require.NoError(t, e.st.SaveInstall(inst))
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.Equal(t, "lab", e.install().Cluster.Name)
}

// A store made in another cluster is refused before anything of the node
// is registered, issued or opened: setup and repair alike.
func TestSetupRefusesAStoreMadeInAnotherCluster(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options Options
	}{
		{"setup", Options{Yes: true, Node: testNode, firewallAlone: true}},
		{"repair", Options{Yes: true, Repair: true, Node: testNode}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			e.joinable()
			inst := e.install()
			inst.Cluster = &store.ClusterIdentity{CASHA256: strings.Repeat("ab", 32), Name: "elsewhere"}
			require.NoError(t, e.st.SaveInstall(inst))
			e.script(preflight("9.0.10"))

			err := e.setup(tt.options)

			require.ErrorContains(t, err, "the store was made in cluster elsewhere")
			require.ErrorContains(t, err, "pco promote")
			require.ErrorContains(t, err, "pco setup --recover")
			e.done()
			require.Equal(t, "elsewhere", e.install().Cluster.Name, "the record is not rewritten")
			_, registered := e.nodes()[testNode]
			require.False(t, registered)
			_, found, err := e.st.Enrolment(testNode)
			require.NoError(t, err)
			require.False(t, found)
			require.NotContains(t, strings.Join(e.run.ran, "\n"), "firewall")
		})
	}
}

func TestACertificateNearItsEndIsIssuedAgainWithANewKey(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()
	key, serial := e.read(e.agentFile(agentKeyFile)), e.node(testNode).CertSerial
	later := t0.Add(pki.LeafValidity - pki.SelfRenewBefore + time.Hour)
	e.s.now = func() time.Time { return later }
	e.cf.SetNow(e.s.now)

	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.NotEqual(t, key, e.read(e.agentFile(agentKeyFile)))
	n := e.node(testNode)
	require.NotEqual(t, serial, n.CertSerial)
	require.Equal(t, e.enrolment().Serial, n.CertSerial)
	e.requireShown("certificates: issued")
}

func TestACertificateOfAnotherCAIsIssuedAgain(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()
	// The CA was rotated under the node.
	e.installCA()

	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	current, _, _, err := e.st.CA()
	require.NoError(t, err)
	block, _ := pem.Decode([]byte(current.CertPEM))
	caCert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.NoError(t, e.agentCert().CheckSignatureFrom(caCert))
	require.Equal(t, current.CertPEM, e.read(e.agentFile(agentCAFile)))
}

func TestTheAgentAddressComesFromTheAgentNetworkWhenSet(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	settings, err := e.st.Settings()
	require.NoError(t, err)
	settings.Cluster.AgentNetwork = netip.MustParsePrefix("10.99.0.0/24")
	require.NoError(t, e.st.SaveSettings(settings))
	e.script(joined()...)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.Equal(t, netip.MustParseAddr("10.99.0.10"), e.node(testNode).AgentAddress)
}

func TestANodeWithoutAnAddressInTheAgentNetworkIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.joinable()
	settings, err := e.st.Settings()
	require.NoError(t, err)
	settings.Cluster.AgentNetwork = netip.MustParsePrefix("10.200.0.0/24")
	require.NoError(t, e.st.SaveSettings(settings))
	e.script(preflight("9.0.10"))

	err = e.setup(Options{Yes: true, Node: testNode})

	require.EqualError(t, err, "setup step cluster: this node has no address in cluster.agentNetwork 10.200.0.0/24 "+
		"(it has 192.0.2.10, 10.99.0.10): give it one, or change the setting")
	e.done()
}

func TestTheAgentAddressFallsBackToTheClusterStatus(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	// A node that stands alone, before corosync ever listed it.
	e.writeFile(e.s.host.members, []byte(`{"nodename":"`+testNode+`","version":0}`+"\n"), 0o644)
	e.script(preflight("9.0.10"), noConnectorsSeen(),
		[]call{{line: "pvesh get /cluster/status --output-format json", out: clusterStatus("192.0.2.11")}}, roleCreated(privs9), userCreated(), tokenCreated(), tagsAdded("", "cf-tunnel;cf-tunnel-managed"),
		cloudflaredInstalled(), daemonIs("inactive"), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	require.Equal(t, netip.MustParseAddr("192.0.2.11"), e.node(testNode).AgentAddress)
}

func TestTheAgentAddressIsOneOtherNodesCanReach(t *testing.T) {
	tests := []struct {
		addr string
		why  string
	}{
		{"192.0.2.10", ""},
		{"fd00:4::10", ""},
		{"127.0.0.1", "a loopback address"},
		{"::1", "a loopback address"},
		{"0.0.0.0", "the unspecified address"},
		{"::", "the unspecified address"},
		{"169.254.7.7", "a link-local address"},
		{"fe80::1", "a link-local address"},
		{"ff02::1", "a link-local address"},
		{"224.0.0.251", "a link-local address"},
		{"239.1.1.1", "a multicast address"},
		{"ff05::1", "a multicast address"},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			require.Equal(t, tt.why, unusableAgentAddress(netip.MustParseAddr(tt.addr)))
		})
	}

	t.Run("IPv4 first", func(t *testing.T) {
		a, err := pickAgentAddress([]netip.Addr{netip.MustParseAddr("fd00:4::10"), netip.MustParseAddr("192.0.2.10")})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("192.0.2.10"), a)
	})
	t.Run("IPv6 when it is the only one", func(t *testing.T) {
		a, err := pickAgentAddress([]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("fd00:4::10")})
		require.NoError(t, err)
		require.Equal(t, netip.MustParseAddr("fd00:4::10"), a)
	})
	t.Run("none usable", func(t *testing.T) {
		_, err := pickAgentAddress([]netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("fe80::1")})
		require.EqualError(t, err, "127.0.0.1 is a loopback address, fe80::1 is a link-local address")
		_, err = pickAgentAddress(nil)
		require.EqualError(t, err, "none listed")
	})
}

func TestAClusterStatusWithOnlyALoopbackAddressRefusesTheSetup(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.script(preflight("9.0.10"), noConnectorsSeen(),
		[]call{{line: "pvesh get /cluster/status --output-format json", out: clusterStatus("127.0.0.1")}})

	err := e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode})

	require.EqualError(t, err, "setup step cluster: neither "+e.s.host.members+" nor the cluster status lists an address of node "+
		testNode+" the agent can listen on: 127.0.0.1 is a loopback address")
	e.done()
}

func TestAListedAddressTheAgentCannotListenOnRefusesTheJoin(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.clusterAs(testNode, "fe80::10", firstNode)
	e.script(preflight("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.EqualError(t, err, "setup step cluster: "+e.s.host.members+" lists fe80::10 for node "+testNode+
		", which is a link-local address: the agent cannot listen there")
	e.done()
}

// A member whose corosync has not reported yet prints the file of a node
// alone; its own copy of corosync.conf is what gives it away.
func TestANodeWithACorosyncConfigOfItsOwnIsNotAlone(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.writeFile(e.s.host.localCorosync, []byte("totem {\n}\n"), 0o644)
	e.script(preflight("9.0.10"), noConnectorsSeen())

	err := e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode})

	require.ErrorContains(t, err, "setup step cluster: reading who is in the cluster")
	require.ErrorContains(t, err, e.s.host.localCorosync)
	e.done()
}

func TestRepairKeepsTheCertificatesAndTheEntry(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.joinable()
	e.script(joined()...)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()
	before := e.node(testNode)

	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), serviceStarted())
	require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}))
	e.done()

	require.Equal(t, before, e.node(testNode))
	e.requireShown("certificates: kept")
}
