package daemon

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	joinInstall = "0123456789ab"
	agentVMID   = 9301
	agentMAC    = "bc:24:11:00:93:01"
)

var agentAddr = netip.MustParseAddr("10.92.0.31")

// fakeJoinPVE is the Proxmox an appliance controller attests a joining
// container through: lxc/9301 on pve2, in pool pco, with one card.
type fakeJoinPVE struct {
	resources []pve.Resource
	ha        []pve.HAResource
	config    map[string]string
	addrs     []netip.Addr
}

func newFakeJoinPVE() *fakeJoinPVE {
	return &fakeJoinPVE{
		resources: []pve.Resource{{Kind: model.KindLXC, VMID: agentVMID, Node: "pve2", Pool: "pco", Status: "running"}},
		config:    map[string]string{"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:93:01,ip=dhcp,type=veth"},
		addrs:     []netip.Addr{agentAddr},
	}
}

func (f *fakeJoinPVE) Resources(context.Context) ([]pve.Resource, error)     { return f.resources, nil }
func (f *fakeJoinPVE) HAResources(context.Context) ([]pve.HAResource, error) { return f.ha, nil }
func (f *fakeJoinPVE) GuestConfig(context.Context, string, model.GuestRef) (pve.GuestConfig, error) {
	return pve.GuestConfig{Values: f.config}, nil
}

func (f *fakeJoinPVE) LXCInterfaces(context.Context, string, int) ([]pve.GuestIface, error) {
	return []pve.GuestIface{{Name: "eth0", MAC: agentMAC, Addrs: f.addrs}}, nil
}

type joinTest struct {
	t     *testing.T
	st    *store.Store
	ca    pki.CA
	px    *fakeJoinPVE
	j     *applianceJoins
	tok   pki.JoinToken
	notes []string
	now   time.Time
}

func newJoinTest(t *testing.T) *joinTest {
	t.Helper()
	base := t.TempDir()
	st, err := store.Open(store.Paths{Cluster: filepath.Join(base, "c"), Private: filepath.Join(base, "p"), Local: filepath.Join(base, "l")})
	require.NoError(t, err)
	require.NoError(t, st.Init())
	jt := &joinTest{t: t, st: st, px: newFakeJoinPVE(), now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	jt.ca, err = pki.NewCA(joinInstall, jt.now.Add(-time.Hour), rand.Reader)
	require.NoError(t, err)
	certPEM, keyPEM, err := jt.ca.PEM()
	require.NoError(t, err)
	require.NoError(t, st.SaveCAIfAbsent(store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}))
	require.NoError(t, st.SaveNode(store.NodeEntry{Name: "pve1", Since: jt.now, State: store.NodeActive, StateSince: jt.now}))
	jt.tok, err = pki.NewJoinToken(joinInstall, "pve2", []string{"10.92.0.150:8644"}, jt.ca, jt.now, rand.Reader)
	require.NoError(t, err)
	require.NoError(t, st.SaveJoinToken(store.JoinRecord{
		ID: "tok1", Node: "pve2", SecretSHA256: jt.tok.SecretHash(), CreatedAt: jt.now, Expires: jt.tok.Expires,
	}))
	jt.j = &applianceJoins{
		st: st, px: jt.px, install: joinInstall, now: func() time.Time { return jt.now }, rand: rand.Reader, log: zerolog.Nop(),
		note: func(node, msg string) { jt.notes = append(jt.notes, node+": "+msg) },
	}
	return jt
}

// use records a use of the token, as the endpoint does before the join.
func (jt *joinTest) use(src netip.Addr) store.JoinRecord {
	jt.t.Helper()
	rec, err := clusterapi.UseToken(jt.st, jt.tok, store.JoinUse{At: jt.now, Source: src, VMID: agentVMID, MACs: []string{agentMAC}})
	require.NoError(jt.t, err)
	return rec
}

func (jt *joinTest) csr(vmid int) []byte {
	jt.t.Helper()
	key, err := pki.NewKey(rand.Reader)
	require.NoError(jt.t, err)
	csr, err := pki.NewCSR(key, pki.Identity{Kind: pki.KindAppliance, InstallID: joinInstall, Node: "pve2", VMID: vmid}, rand.Reader)
	require.NoError(jt.t, err)
	return csr
}

func (jt *joinTest) join(src netip.Addr) ([]byte, error) {
	jt.use(src)
	return jt.j.Join(context.Background(), jt.tok, jt.csr(agentVMID), src, clusterapi.JoinFacts{
		Node: "pve2", VMID: agentVMID, MACs: []string{agentMAC}, Build: "1.2.3", Protocol: agentproto.Range{Min: 1, Max: 1},
	})
}

func (jt *joinTest) entry(node string) store.NodeEntry {
	jt.t.Helper()
	nodes, err := jt.st.Nodes()
	require.NoError(jt.t, err)
	for _, n := range nodes {
		if n.Name == node {
			return n
		}
	}
	jt.t.Fatalf("node %s is not in the registry", node)
	return store.NodeEntry{}
}

func parseCertPEM(t *testing.T, b []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(b)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func TestAContainerThatHoldsTogetherJoinsAsJoining(t *testing.T) {
	jt := newJoinTest(t)

	certPEM, err := jt.join(agentAddr)

	require.NoError(t, err)
	cert := parseCertPEM(t, certPEM)
	require.NoError(t, cert.CheckSignatureFrom(jt.ca.Cert))
	id, err := pki.IdentityOf(cert)
	require.NoError(t, err)
	require.Equal(t, pki.Identity{Kind: pki.KindAppliance, InstallID: joinInstall, Node: "pve2", VMID: agentVMID}, id)
	require.Equal(t, store.NodeEntry{
		Name: "pve2", Version: "1.2.3", Since: jt.now, Kind: "appliance", VMID: agentVMID,
		Protocol:   store.ProtocolRange{Min: 1, Max: 1},
		CertSerial: cert.SerialNumber.Text(16), CertSHA256: pki.Fingerprint(cert),
		State: store.NodeJoining, StateSince: jt.now,
	}, jt.entry("pve2"))
	require.Len(t, jt.notes, 1)
	require.Contains(t, jt.notes[0], "pco node admit pve2 --fingerprint")
	require.Contains(t, jt.notes[0], pki.Fingerprint(cert))
}

func TestAContainerThatDoesNotMatchItsJoinIsContested(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(jt *joinTest)
		src    netip.Addr
		want   string
	}{
		{"on another node", func(jt *joinTest) { jt.px.resources[0].Node = "pve3" }, agentAddr, "container 9301 runs on pve3, not on pve2"},
		{"no such container", func(jt *joinTest) { jt.px.resources = nil }, agentAddr, "container 9301 is not in the cluster"},
		{"another MAC", func(jt *joinTest) { jt.px.config["net0"] = "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:99:99" }, agentAddr,
			"is not configured with the MAC"},
		{"another source", func(*joinTest) {}, netip.MustParseAddr("10.92.0.66"), "the join came from 10.92.0.66, which is no address of container 9301"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			jt := newJoinTest(t)
			tt.change(jt)

			_, err := jt.join(tt.src)

			require.ErrorContains(t, err, tt.want)
			n := jt.entry("pve2")
			require.Equal(t, store.NodeContested, n.State)
			require.Len(t, n.ContestedUses, 1, "every use of the token is shown")
			require.Equal(t, tt.src, n.ContestedUses[0].Source)
			require.Empty(t, n.CertSHA256, "no certificate was issued")
		})
	}
}

func TestAContainerOutsideThePoolOrOnHAIsRefusedWithoutAnEntry(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(jt *joinTest)
		want   string
	}{
		{"not in pool pco", func(jt *joinTest) { jt.px.resources[0].Pool = "" }, "container 9301 is not in pool pco"},
		{"an HA resource", func(jt *joinTest) {
			jt.px.ha = []pve.HAResource{{SID: "ct:9301", Ref: model.GuestRef{Kind: model.KindLXC, VMID: agentVMID}}}
		}, "agent 9301 is a Proxmox HA resource; agents are bound to their node"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			jt := newJoinTest(t)
			tt.change(jt)

			_, err := jt.join(agentAddr)

			require.ErrorContains(t, err, tt.want)
			nodes, err := jt.st.Nodes()
			require.NoError(t, err)
			require.Len(t, nodes, 1, "only the controller's node")
		})
	}
}

func TestANodeServedAlreadyTakesNoSecondAgent(t *testing.T) {
	jt := newJoinTest(t)
	require.NoError(t, jt.st.SaveNode(store.NodeEntry{Name: "pve2", Since: jt.now, State: store.NodeActive, StateSince: jt.now, Kind: "appliance", VMID: 9300}))

	_, err := jt.join(agentAddr)

	require.ErrorContains(t, err, "node pve2 is active in the install already")
	require.Equal(t, 9300, jt.entry("pve2").VMID)
	require.Equal(t, store.NodeActive, jt.entry("pve2").State)
}

func TestATokenUsedTwiceHoldsItsNodeContested(t *testing.T) {
	jt := newJoinTest(t)
	_, err := jt.join(agentAddr)
	require.NoError(t, err)

	rec := jt.use(netip.MustParseAddr("10.92.0.66"))
	jt.j.misused(context.Background(), rec)

	n := jt.entry("pve2")
	require.Equal(t, store.NodeContested, n.State)
	require.Len(t, n.ContestedUses, 2)
	require.Equal(t, agentVMID, n.VMID)
}

func TestAnEnrolledAgentRenewsAndTheOldCertificateIsNoLongerNamed(t *testing.T) {
	jt := newJoinTest(t)
	first, err := jt.join(agentAddr)
	require.NoError(t, err)

	next, err := jt.j.Renew(context.Background(), "pve2", jt.csr(agentVMID))

	require.NoError(t, err)
	cert := parseCertPEM(t, next)
	require.Equal(t, pki.Fingerprint(cert), jt.entry("pve2").CertSHA256)
	require.NotEqual(t, pki.Fingerprint(parseCertPEM(t, first)), jt.entry("pve2").CertSHA256)

	_, err = jt.j.Renew(context.Background(), "pve1", jt.csr(agentVMID))
	require.ErrorContains(t, err, "node pve1 is no appliance agent")
	n := jt.entry("pve2")
	n.State = store.NodeRemoved
	require.NoError(t, jt.st.SaveNode(n))
	_, err = jt.j.Renew(context.Background(), "pve2", jt.csr(agentVMID))
	require.ErrorContains(t, err, "node pve2 is removed")
}
