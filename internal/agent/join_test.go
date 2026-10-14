package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const joinVMID = 9301

// joinCtrl is an appliance controller that takes joins: it issues what the
// endpoint lets through, unless a test makes it refuse.
type joinCtrl struct {
	*ctrlFake
	ca     pki.CA
	mu     sync.Mutex
	facts  []clusterapi.JoinFacts
	refuse error
}

func (c *joinCtrl) Join(_ context.Context, _ pki.JoinToken, csr []byte, _ netip.Addr, facts clusterapi.JoinFacts) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.facts = append(c.facts, facts)
	if c.refuse != nil {
		return nil, c.refuse
	}
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: pkiInstall, Node: facts.Node, VMID: facts.VMID}
	cert, err := c.ca.IssueClient(csr, id, time.Now(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), nil
}

func (c *joinCtrl) joins() []clusterapi.JoinFacts {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clusterapi.JoinFacts{}, c.facts...)
}

// joinEnv is an appliance controller on a loopback address with one join
// token of node pve1, and the volume of the agent that joins with it.
type joinEnv struct {
	t     *testing.T
	ca    pki.CA
	ctrl  *joinCtrl
	addr  netip.AddrPort
	st    *store.Store
	tok   pki.JoinToken
	dir   string
	logs  *bytes.Buffer
	files TLSFiles
}

func newJoinEnv(t *testing.T) *joinEnv {
	t.Helper()
	e := &joinEnv{t: t, ca: newCA(t, time.Now().Add(-day)), dir: t.TempDir(), logs: &bytes.Buffer{}}
	base := t.TempDir()
	st, err := store.Open(store.Paths{Cluster: filepath.Join(base, "c"), Private: filepath.Join(base, "p"), Local: filepath.Join(base, "l")})
	require.NoError(t, err)
	require.NoError(t, st.Init())
	e.st = st
	e.ctrl = &joinCtrl{ctrlFake: newCtrlFake(), ca: e.ca}
	e.addr = e.serve(e.ca)
	e.tok, err = pki.NewJoinToken(pkiInstall, testNode, []string{e.addr.String()}, e.ca, time.Now(), rand.Reader)
	require.NoError(t, err)
	require.NoError(t, st.SaveJoinToken(store.JoinRecord{
		ID: "tok1", Node: testNode, SecretSHA256: e.tok.SecretHash(), CreatedAt: time.Now(), Expires: e.tok.Expires,
	}))
	e.files = TLSFiles{Dir: filepath.Join(e.dir, "agent", "tls")}
	e.writeToken(e.tok.Encode(), 0o600)
	return e
}

// serve starts an endpoint that takes joins with a server certificate
// under ca, and returns where it listens.
func (e *joinEnv) serve(ca pki.CA) netip.AddrPort {
	t := e.t
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueServer(&key.PublicKey, []string{pki.ControllerServerName(pkiInstall)}, time.Now(), rand.Reader)
	require.NoError(t, err)
	pool := func() *x509.CertPool {
		p := x509.NewCertPool()
		p.AddCert(ca.Cert)
		return p
	}
	srv := clusterapi.New(e.ctrl, &authFake{}, pki.TLSCertificate(key, cert, ca.Cert), pool, zerolog.Nop())
	srv.AcceptJoins(clusterapi.Joins{InstallID: pkiInstall, CASHA256: func() string { return ca.SHA256() }, Tokens: e.st})
	listening := make(chan netip.AddrPort, 1)
	srv.OnListening(func(ap netip.AddrPort) { listening <- ap })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, netip.MustParseAddrPort("127.0.0.1:0")) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	return <-listening
}

func (e *joinEnv) tokenFile() string { return filepath.Join(e.dir, "join.token") }

func (e *joinEnv) writeToken(text string, perm os.FileMode) {
	require.NoError(e.t, os.WriteFile(e.tokenFile(), []byte(text+"\n"), perm))
	require.NoError(e.t, os.Chmod(e.tokenFile(), perm))
}

func (e *joinEnv) joiner() Joiner {
	return Joiner{
		TokenFile: e.tokenFile(), Owner: os.Getuid(), Files: e.files, Dir: filepath.Join(e.dir, "agent"),
		InstallID: pkiInstall, Node: testNode, VMID: joinVMID, MACs: []string{"bc:24:11:00:00:10"}, Build: "dev",
		Log: zerolog.New(e.logs),
	}
}

// noSecret fails when the token shows in s.
func (e *joinEnv) noSecret(s string) {
	e.t.Helper()
	require.NotContains(e.t, s, e.tok.Encode())
	require.NotContains(e.t, e.logs.String(), e.tok.Encode())
}

func TestAnApplianceAgentJoinsWithItsTokenOnce(t *testing.T) {
	e := newJoinEnv(t)

	creds, err := e.joiner().Join(context.Background())

	require.NoError(t, err)
	id, err := pki.IdentityOf(creds.Cert)
	require.NoError(t, err)
	require.Equal(t, pki.Identity{Kind: pki.KindAppliance, InstallID: pkiInstall, Node: testNode, VMID: joinVMID}, id)
	require.Equal(t, e.ca.SHA256(), creds.CASHA256())
	kept, err := e.files.Client()
	require.NoError(t, err)
	require.Equal(t, creds.Cert.Raw, kept.Cert.Raw)
	require.NoFileExists(t, e.tokenFile(), "the token goes once it was used")
	rec, found, err := ReadController(filepath.Join(e.dir, "agent"))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ControllerRecord{
		InstallID: pkiInstall, Endpoints: []string{e.addr.String()}, ServerName: pki.ControllerServerName(pkiInstall), CASHA256: e.ca.SHA256(),
	}, rec)
	require.Equal(t, []clusterapi.JoinFacts{{
		Node: testNode, VMID: joinVMID, MACs: []string{"bc:24:11:00:00:10"}, Build: "dev", Protocol: e.ctrl.joins()[0].Protocol,
	}}, e.ctrl.joins())
	e.noSecret(e.logs.String())

	_, err = e.joiner().Join(context.Background())
	require.ErrorIs(t, err, ErrNoJoinToken, "a token is used once")
}

func TestAJoinTheControllerRefusesRemovesTheToken(t *testing.T) {
	e := newJoinEnv(t)
	e.ctrl.refuse = errors.New("container 9301 is not in pool pco")

	_, err := e.joiner().Join(context.Background())

	var refused *JoinRefusedError
	require.ErrorAs(t, err, &refused)
	require.NoFileExists(t, e.tokenFile())
	_, err = e.files.Client()
	require.ErrorIs(t, err, ErrNoCredentials)
	e.noSecret(err.Error())
}

func TestAJoinThatReachesNoControllerKeepsTheToken(t *testing.T) {
	e := newJoinEnv(t)
	j := e.joiner()
	j.Dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("connection refused") }

	_, err := j.Join(context.Background())

	require.ErrorContains(t, err, "connection refused")
	var refused *JoinRefusedError
	require.False(t, errors.As(err, &refused), "the next try may reach it")
	require.FileExists(t, e.tokenFile())
	e.noSecret(err.Error())
}

func TestAControllerOfAnotherCAIsNotSentTheToken(t *testing.T) {
	e := newJoinEnv(t)
	other := e.serve(newCA(t, time.Now().Add(-day)))
	tok, err := pki.NewJoinToken(pkiInstall, testNode, []string{other.String()}, e.ca, time.Now(), rand.Reader)
	require.NoError(t, err)
	e.tok = tok
	e.writeToken(tok.Encode(), 0o600)

	_, err = e.joiner().Join(context.Background())

	require.ErrorContains(t, err, "is not under the CA "+e.ca.SHA256())
	require.Empty(t, e.ctrl.joins(), "the request is never sent")
	require.FileExists(t, e.tokenFile())
	e.noSecret(err.Error())
}

func TestATokenThatIsNotThisContainersIsRemovedUnsent(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(j *Joiner, e *joinEnv)
		want string
	}{
		{"another node", func(j *Joiner, _ *joinEnv) { j.Node = "pve2" }, "the token is for node pve1, this container runs on node pve2"},
		{"another install", func(j *Joiner, _ *joinEnv) { j.InstallID = "ba9876543210" }, "the token is of install"},
		{"expired", func(j *Joiner, e *joinEnv) { j.Now = func() time.Time { return e.tok.Expires } }, "the token expired"},
		{"a file others may read", func(_ *Joiner, e *joinEnv) { e.writeToken(e.tok.Encode(), 0o644) }, "has mode 0644"},
		{"a file of another user", func(j *Joiner, _ *joinEnv) { j.Owner = os.Getuid() + 1 }, "is not root's"},
		{"no token at all", func(_ *Joiner, e *joinEnv) { e.writeToken("pco1.bm90IGEgdG9rZW4", 0o600) }, "the file holds no join token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newJoinEnv(t)
			j := e.joiner()
			tt.edit(&j, e)

			_, err := j.Join(context.Background())

			var refused *JoinRefusedError
			require.ErrorAs(t, err, &refused)
			require.ErrorContains(t, err, tt.want)
			require.NoFileExists(t, e.tokenFile())
			require.Empty(t, e.ctrl.joins())
			e.noSecret(err.Error())
		})
	}
}

func TestAJoinedCertificateIsCheckedBeforeItIsKept(t *testing.T) {
	ca := newCA(t, time.Now().Add(-day))
	other := newCA(t, time.Now().Add(-day))
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: pkiInstall, Node: testNode, VMID: joinVMID}
	issue := func(ca pki.CA, key any, id pki.Identity) string {
		k, err := pki.NewKey(rand.Reader)
		require.NoError(t, err)
		if key != nil {
			k = key.(*ecdsa.PrivateKey)
		}
		csr, err := pki.NewCSR(k, id, rand.Reader)
		require.NoError(t, err)
		cert, err := ca.IssueClient(csr, id, time.Now(), rand.Reader)
		require.NoError(t, err)
		return pemCert(cert)
	}
	for _, tt := range []struct {
		name   string
		answer clusterapi.CertificateResponse
		want   string
	}{
		{"another key", clusterapi.CertificateResponse{CertPEM: issue(ca, nil, id), CAPEM: pemCert(ca.Cert)}, "for another key"},
		{"another container", clusterapi.CertificateResponse{
			CertPEM: issue(ca, key, pki.Identity{Kind: pki.KindAppliance, InstallID: pkiInstall, Node: testNode, VMID: 9302}), CAPEM: pemCert(ca.Cert),
		}, "not pco:appliance"},
		{"another CA", clusterapi.CertificateResponse{CertPEM: issue(other, key, id), CAPEM: pemCert(other.Cert)}, "another CA than the token names"},
		{"a CA it is not under", clusterapi.CertificateResponse{CertPEM: issue(other, key, id), CAPEM: pemCert(ca.Cert)}, "not under its CA"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := joined(key, tt.answer, id, ca.SHA256())
			require.ErrorContains(t, err, tt.want)
		})
	}
	creds, err := joined(key, clusterapi.CertificateResponse{CertPEM: issue(ca, key, id), CAPEM: pemCert(ca.Cert)}, id, ca.SHA256())
	require.NoError(t, err)
	require.Equal(t, ca.Cert.Raw, creds.CA.Raw)
}
