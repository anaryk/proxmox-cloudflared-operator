package clusterapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var (
	addr2   = netip.MustParseAddr("10.0.0.2")
	addr3   = netip.MustParseAddr("10.0.0.3")
	errNope = errors.New("nope")
)

type recorded struct {
	mu   sync.Mutex
	list []store.SecurityEvent
}

func (r *recorded) add(e store.SecurityEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, e)
}

func (r *recorded) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.list))
	for i, e := range r.list {
		out[i] = e.Message
	}
	return out
}

type checkEnv struct {
	t       *testing.T
	st      *store.Store
	ca      pki.CA
	clock   *clock
	events  *recorded
	observe bool
	sources Sources
	c       *Checker
}

func newCheckEnv(t *testing.T) *checkEnv {
	t.Helper()
	e := &checkEnv{t: t, st: openStore(t), clock: &clock{t: t0}, events: &recorded{}}
	var err error
	e.ca, err = pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	e.saveCA(e.ca, nil, time.Time{})
	e.sources = Sources{Nodes: map[string][]netip.Addr{testNode: {addr2}, "pve3": {addr3}}}
	list := NewAllowlist(func(context.Context) (Sources, error) { return e.sources, nil }, e.clock.now, zerolog.Nop())
	require.NoError(t, list.Refresh(t.Context()))
	e.c = &Checker{
		InstallID: testInstall, Store: e.st, Allow: list, Now: e.clock.now, Record: e.events.add, Log: zerolog.Nop(),
		ObserveOnly: func() (time.Time, bool) { return t0, e.observe },
	}
	return e
}

func caFiles(t *testing.T, ca pki.CA) store.CAFiles {
	t.Helper()
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	return store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}
}

func (e *checkEnv) saveCA(current pki.CA, previous *pki.CA, rotatedAt time.Time) {
	e.t.Helper()
	var prev *store.CAFiles
	if previous != nil {
		p := caFiles(e.t, *previous)
		prev = &p
	}
	require.NoError(e.t, e.st.SaveCA(caFiles(e.t, current), prev, rotatedAt))
}

func (e *checkEnv) issue(ca pki.CA, id pki.Identity) (*ecdsa.PrivateKey, *x509.Certificate) {
	e.t.Helper()
	key, err := pki.NewKey(rand.Reader)
	require.NoError(e.t, err)
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(e.t, err)
	cert, err := ca.IssueClient(csr, id, e.clock.now(), rand.Reader)
	require.NoError(e.t, err)
	return key, cert
}

func certPEM(cert *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
}

// enrol writes the entry and the enrolment of testNode as setup does.
func (e *checkEnv) enrol(state string, cert *x509.Certificate) {
	node := testNode
	e.t.Helper()
	entry := store.NodeEntry{Name: node, Version: "dev", Since: t0, Kind: string(pki.KindHost), State: state, AgentAddress: addr2}
	if cert != nil {
		require.NoError(e.t, e.st.SaveEnrolment(node, store.Enrolment{CertPEM: certPEM(cert)}))
		en, _, err := e.st.Enrolment(node)
		require.NoError(e.t, err)
		entry.CertSerial, entry.CertSHA256 = en.Serial, en.SHA256
	}
	require.NoError(e.t, e.st.SaveNode(entry))
}

func (e *checkEnv) entry(node string) store.NodeEntry {
	e.t.Helper()
	nodes, err := e.st.Nodes()
	require.NoError(e.t, err)
	for _, n := range nodes {
		if n.Name == node {
			return n
		}
	}
	e.t.Fatalf("no entry of %s", node)
	return store.NodeEntry{}
}

func TestACertificateTheRegistryDoesNotListIsNotEnrolled(t *testing.T) {
	e := newCheckEnv(t)
	_, cert := e.issue(e.ca, hostID(testNode))
	node, r := e.c.Check(cert, addr2)
	require.Empty(t, node)
	require.NotNil(t, r)
	require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
	require.Contains(t, r.Message, "not in the registry")

	t.Run("an entry without an enrolment", func(t *testing.T) {
		e.enrol(store.NodeActive, nil)
		_, r := e.c.Check(cert, addr2)
		require.NotNil(t, r)
		require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
		require.Contains(t, r.Message, "no enrolment")
	})
	t.Run("no identity", func(t *testing.T) {
		key, err := pki.NewKey(rand.Reader)
		require.NoError(t, err)
		server, err := e.ca.IssueServer(&key.PublicKey, []string{pki.NodeServerName(testInstall, testNode)}, t0, rand.Reader)
		require.NoError(t, err)
		_, r := e.c.Check(server, addr2)
		require.NotNil(t, r)
		require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
	})
}

func TestARemovedOrContestedNodeIsRefusedAsSuch(t *testing.T) {
	e := newCheckEnv(t)
	_, cert := e.issue(e.ca, hostID(testNode))
	for state, code := range map[string]string{store.NodeRemoved: agentproto.RefusalRemoved, store.NodeContested: agentproto.RefusalContested} {
		t.Run(state, func(t *testing.T) {
			e.enrol(state, cert)
			_, r := e.c.Check(cert, addr2)
			require.NotNil(t, r)
			require.Equal(t, code, r.Code)
		})
	}
}

func TestAHostCertificateEqualToItsEnrolmentIsAccepted(t *testing.T) {
	e := newCheckEnv(t)
	_, cert := e.issue(e.ca, hostID(testNode))
	e.enrol(store.NodeActive, cert)
	node, r := e.c.Check(cert, addr2)
	require.Nil(t, r)
	require.Equal(t, testNode, node)
	require.Empty(t, e.events.messages())

	for _, state := range []string{store.NodeJoining, store.NodeDraining, store.NodeUnknown, store.NodeDown, ""} {
		t.Run("state "+state, func(t *testing.T) {
			e.enrol(state, cert)
			_, r := e.c.Check(cert, addr2)
			require.Nil(t, r)
		})
	}
	t.Run("a certificate the node issued itself is the enrolment's", func(t *testing.T) {
		_, renewed := e.issue(e.ca, hostID(testNode))
		require.NoError(t, e.st.SaveEnrolment(testNode, store.Enrolment{CertPEM: certPEM(renewed)}))
		_, r := e.c.Check(cert, addr2)
		require.NotNil(t, r, "the old certificate is not the enrolment's any more")
		require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
		node, r := e.c.Check(renewed, addr2)
		require.Nil(t, r)
		require.Equal(t, testNode, node)
	})
}

func TestTheRegistryFollowsTheEnrolmentWithAnEvent(t *testing.T) {
	e := newCheckEnv(t)
	_, old := e.issue(e.ca, hostID(testNode))
	e.enrol(store.NodeActive, old)
	_, cert := e.issue(e.ca, hostID(testNode))
	require.NoError(t, e.st.SaveEnrolment(testNode, store.Enrolment{CertPEM: certPEM(cert)}))

	node, r := e.c.Check(cert, addr2)
	require.Nil(t, r)
	require.Equal(t, testNode, node)
	entry := e.entry(testNode)
	require.Equal(t, cert.SerialNumber.Text(16), entry.CertSerial)
	require.Equal(t, pki.Fingerprint(cert), entry.CertSHA256)
	require.Equal(t, store.NodeActive, entry.State, "nothing else of the entry changed")
	require.Len(t, e.events.messages(), 1)
	require.Contains(t, e.events.messages()[0], "registry")
	require.Equal(t, testNode, e.events.list[0].Subject)

	_, r = e.c.Check(cert, addr2)
	require.Nil(t, r)
	require.Len(t, e.events.messages(), 1, "once brought in line, no event")
}

func TestACertificateMintedElsewhereWithTheCAKeyIsRefused(t *testing.T) {
	e := newCheckEnv(t)
	_, cert := e.issue(e.ca, hostID(testNode))
	e.enrol(store.NodeActive, cert)
	_, minted := e.issue(e.ca, hostID(testNode))
	_, r := e.c.Check(minted, addr2)
	require.NotNil(t, r)
	require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
	require.Contains(t, r.Message, "enrolment")
}

func TestAValidCertificateFromAnotherAddressIsRefused(t *testing.T) {
	e := newCheckEnv(t)
	_, cert := e.issue(e.ca, hostID(testNode))
	e.enrol(store.NodeActive, cert)
	_, r := e.c.Check(cert, addr3)
	require.NotNil(t, r)
	require.Equal(t, agentproto.RefusalOtherSource, r.Code)
	require.False(t, r.Definitive())
	require.Contains(t, r.Message, addr3.String())

	t.Run("an address the allowlist learns of", func(t *testing.T) {
		e.sources.Nodes[testNode] = []netip.Addr{addr2, addr3}
		e.clock.advance(missGap)
		_, r := e.c.Check(cert, addr3)
		require.Nil(t, r)
	})
}

func TestACertificateOfAnotherInstallOrKindIsRefused(t *testing.T) {
	e := newCheckEnv(t)
	other, err := pki.NewCA("fedcba987654", t0, rand.Reader)
	require.NoError(t, err)
	_, cert := e.issue(other, pki.Identity{Kind: pki.KindHost, InstallID: "fedcba987654", Node: testNode})
	_, r := e.c.Check(cert, addr2)
	require.NotNil(t, r)
	require.Equal(t, agentproto.RefusalOtherInstall, r.Code)

	t.Run("an appliance certificate for a host entry", func(t *testing.T) {
		_, cert := e.issue(e.ca, applianceID(testNode, 200))
		e.enrol(store.NodeActive, cert)
		_, r := e.c.Check(cert, addr2)
		require.NotNil(t, r)
		require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
		require.Contains(t, r.Message, "kind")
	})
}

func TestAnApplianceCertificateIsCheckedAgainstTheRegistryAlone(t *testing.T) {
	e := newCheckEnv(t)
	_, cert := e.issue(e.ca, applianceID(testNode, 200))
	entry := store.NodeEntry{
		Name: testNode, Version: "dev", Since: t0, Kind: string(pki.KindAppliance), VMID: 200, State: store.NodeJoining,
		CertSerial: cert.SerialNumber.Text(16), CertSHA256: pki.Fingerprint(cert),
	}
	require.NoError(t, e.st.SaveNode(entry))
	node, r := e.c.Check(cert, addr2)
	require.Nil(t, r)
	require.Equal(t, testNode, node)

	t.Run("another vmid", func(t *testing.T) {
		_, other := e.issue(e.ca, applianceID(testNode, 201))
		entry := entry
		entry.CertSerial, entry.CertSHA256 = other.SerialNumber.Text(16), pki.Fingerprint(other)
		require.NoError(t, e.st.SaveNode(entry))
		_, r := e.c.Check(other, addr2)
		require.NotNil(t, r)
		require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
		require.Contains(t, r.Message, "container")
	})
	t.Run("another serial", func(t *testing.T) {
		require.NoError(t, e.st.SaveNode(entry))
		_, other := e.issue(e.ca, applianceID(testNode, 200))
		_, r := e.c.Check(other, addr2)
		require.NotNil(t, r)
		require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
	})
}

func TestTheObserveOnlyWindowTakesACertificateOfANodeWithoutOne(t *testing.T) {
	e := newCheckEnv(t)
	e.enrol(store.NodeActive, nil)
	_, cert := e.issue(e.ca, hostID(testNode))

	_, r := e.c.Check(cert, addr2)
	require.NotNil(t, r, "outside the window a node without an enrolment is refused")

	e.observe = true
	node, r := e.c.Check(cert, addr2)
	require.Nil(t, r)
	require.Equal(t, testNode, node)
	en, _, err := e.st.Enrolment(testNode)
	require.NoError(t, err)
	require.Equal(t, pki.Fingerprint(cert), en.SHA256, "the enrolment is written")
	require.Equal(t, pki.Fingerprint(cert), e.entry(testNode).CertSHA256)
	require.Len(t, e.events.messages(), 1)
	require.Contains(t, e.events.messages()[0], "observe-only")

	_, r = e.c.Check(cert, addr2)
	require.Nil(t, r)
	require.Len(t, e.events.messages(), 1, "one event per node")

	e.observe = false
	_, r = e.c.Check(cert, addr2)
	require.Nil(t, r, "recorded, so accepted after the window too")

	t.Run("a certificate other than the enrolment's is refused in the window", func(t *testing.T) {
		e.observe = true
		_, other := e.issue(e.ca, hostID(testNode))
		_, r := e.c.Check(other, addr2)
		require.NotNil(t, r)
		require.Equal(t, agentproto.RefusalNotEnrolled, r.Code)
		en, _, err := e.st.Enrolment(testNode)
		require.NoError(t, err)
		require.Equal(t, pki.Fingerprint(cert), en.SHA256, "the enrolment stays")
	})
	t.Run("the window ends after a day whatever the controller says", func(t *testing.T) {
		e := newCheckEnv(t)
		e.enrol(store.NodeActive, nil)
		e.observe = true
		e.clock.advance(ObserveWindow - time.Second)
		_, late := e.issue(e.ca, hostID(testNode))
		e.clock.advance(time.Second)
		_, r := e.c.Check(late, addr2)
		require.NotNil(t, r)
	})
	t.Run("not for a removed or contested node", func(t *testing.T) {
		e.observe = true
		for _, state := range []string{store.NodeRemoved, store.NodeContested} {
			e.enrol(state, nil)
			_, newer := e.issue(e.ca, hostID(testNode))
			_, r := e.c.Check(newer, addr2)
			require.NotNil(t, r)
			require.NotEqual(t, agentproto.RefusalNotEnrolled, r.Code)
		}
	})
	t.Run("an appliance node", func(t *testing.T) {
		_, cert := e.issue(e.ca, applianceID("pve3", 300))
		entry := store.NodeEntry{Name: "pve3", Version: "dev", Since: t0, Kind: string(pki.KindAppliance), VMID: 300, State: store.NodeActive}
		require.NoError(t, e.st.SaveNode(entry))
		e.observe = false
		_, r := e.c.Check(cert, addr3)
		require.NotNil(t, r)
		e.observe = true
		_, r = e.c.Check(cert, addr3)
		require.Nil(t, r)
		require.Equal(t, pki.Fingerprint(cert), e.entry("pve3").CertSHA256)

		_, other := e.issue(e.ca, applianceID("pve3", 300))
		_, r = e.c.Check(other, addr3)
		require.NotNil(t, r, "a registry that holds a certificate refuses another in the window")
	})
}

func TestThePoolHoldsBothCAsForTenMinutesAfterARotation(t *testing.T) {
	e := newCheckEnv(t)
	previous := e.ca
	current, err := pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	e.saveCA(current, &previous, t0)

	_, oldCert := e.issue(previous, hostID(testNode))
	_, newCert := e.issue(current, hostID(testNode))
	verifies := func(cert *x509.Certificate) bool {
		_, err := cert.Verify(x509.VerifyOptions{Roots: e.c.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		return err == nil
	}
	require.True(t, verifies(oldCert))
	require.True(t, verifies(newCert))

	e.clock.advance(RotationWindow - time.Second)
	require.True(t, verifies(oldCert))
	e.clock.advance(time.Second)
	require.False(t, verifies(oldCert), "after ten minutes only the new CA")
	require.True(t, verifies(newCert))

	t.Run("no CA", func(t *testing.T) {
		e2 := &Checker{InstallID: testInstall, Store: openStore(t), Now: e.clock.now, Log: zerolog.Nop()}
		require.False(t, func() bool {
			_, err := newCert.Verify(x509.VerifyOptions{Roots: e2.Pool()})
			return err == nil
		}())
	})
}

func TestTheCheckerNeedsWhatItReads(t *testing.T) {
	e := newCheckEnv(t)
	_, cert := e.issue(e.ca, hostID(testNode))
	e.enrol(store.NodeActive, cert)
	c := *e.c
	c.Allow = nil
	_, r := c.Check(cert, addr2)
	require.NotNil(t, r)
	require.Equal(t, agentproto.RefusalOtherSource, r.Code)

	t.Run("a store that cannot be read", func(t *testing.T) {
		c := *e.c
		c.Store = failingRegistry{}
		_, r := c.Check(cert, addr2)
		require.NotNil(t, r)
		require.Empty(t, r.Code, "a check that could not be made stands no agent down")
		require.Contains(t, r.Message, "could not be read")
	})
}

type failingRegistry struct{}

func (failingRegistry) NodesRev() ([]store.NodeRev, error) { return nil, errNope }
func (failingRegistry) SaveNodeIf(store.NodeEntry, int64) (int64, error) {
	return 0, errNope
}
func (failingRegistry) Enrolment(string) (store.Enrolment, bool, error) {
	return store.Enrolment{}, false, errNope
}
func (failingRegistry) SaveEnrolment(string, store.Enrolment) error { return errNope }
func (failingRegistry) CA() (store.CAFiles, *store.CAFiles, time.Time, error) {
	return store.CAFiles{}, nil, time.Time{}, errNope
}
