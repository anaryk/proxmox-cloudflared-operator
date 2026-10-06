package applianceinstall

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	mathrand "math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testVersion = "1.2.3"
	testNode    = "pve1"

	// Neither secret may show in anything the installer prints, returns or
	// writes on the node.
	pveSecret = "pve-secret-must-not-leak-"
	cfToken   = "cf-token-must-not-leak-4f1d2c"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// answer is the reply of the admin to a question that contains about.
type answer struct {
	about string
	yes   bool
}

// fakePrompter is an admin who answers from a script and sees every line.
type fakePrompter struct {
	t       *testing.T
	answers []answer
	secrets []string
	lines   []string
}

func (p *fakePrompter) Confirm(question string, def bool) (bool, error) {
	p.t.Helper()
	p.lines = append(p.lines, "ask: "+question)
	if len(p.answers) == 0 {
		p.t.Errorf("unexpected question %q", question)
		return def, nil
	}
	a := p.answers[0]
	p.answers = p.answers[1:]
	if !strings.Contains(question, a.about) {
		p.t.Errorf("asked %q, want a question about %q", question, a.about)
	}
	return a.yes, nil
}

func (p *fakePrompter) Secret(question string) (string, error) {
	p.t.Helper()
	p.lines = append(p.lines, "secret: "+question)
	if len(p.secrets) == 0 {
		p.t.Errorf("unexpected question for a secret %q", question)
		return "", nil
	}
	s := p.secrets[0]
	p.secrets = p.secrets[1:]
	return s, nil
}

func (p *fakePrompter) Info(format string, args ...any) {
	p.lines = append(p.lines, "info: "+fmt.Sprintf(format, args...))
}

func (p *fakePrompter) Warn(format string, args ...any) {
	p.lines = append(p.lines, "warn: "+fmt.Sprintf(format, args...))
}

func (p *fakePrompter) text() string { return strings.Join(p.lines, "\n") }

// fakeSignals delivers the signals a test sends.
type fakeSignals struct{ ch chan<- os.Signal }

func (s *fakeSignals) Notify(c chan<- os.Signal, _ ...os.Signal) { s.ch = c }
func (s *fakeSignals) Stop(chan<- os.Signal)                     { s.ch = nil }

// testCerts are the certificates the API of the fake node may present.
type testCerts struct {
	clusterCA, customCA []byte   // PEM
	node, custom        [][]byte // chains, DER: pveproxy's own, and one of a custom CA
}

func newCerts(t *testing.T) testCerts {
	t.Helper()
	ca := func(name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
			NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(24 * time.Hour * 365),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		require.NoError(t, err)
		cert, err := x509.ParseCertificate(der)
		require.NoError(t, err)
		return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	leaf := func(parent *x509.Certificate, key *ecdsa.PrivateKey, dns []string, ips []net.IP) []byte {
		lk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dns[0]},
			NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(24 * time.Hour * 365),
			DNSNames: dns, IPAddresses: ips, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &lk.PublicKey, key)
		require.NoError(t, err)
		return der
	}
	cluster, clusterKey, clusterPEM := ca("Proxmox Virtual Environment")
	custom, customKey, customPEM := ca("Example Private CA")
	// pveproxy's certificate carries the node's names and its primary address,
	// not an address on another bridge.
	return testCerts{
		clusterCA: clusterPEM, customCA: customPEM,
		node:   [][]byte{leaf(cluster, clusterKey, []string{"localhost", testNode, testNode + ".example.test"}, []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("192.0.2.10")})},
		custom: [][]byte{leaf(custom, customKey, []string{"pve.example.test"}, nil)},
	}
}

func chain(t *testing.T, ders [][]byte) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	for _, der := range ders {
		c, err := x509.ParseCertificate(der)
		require.NoError(t, err)
		out = append(out, c)
	}
	return out
}

// testEnv is a node with the fake, an admin, and an installer whose journal,
// /run and /etc/pve are temporary directories.
type testEnv struct {
	t         *testing.T
	node      *fakeNode
	ask       *fakePrompter
	in        *Installer
	journals  string
	runDir    string
	pveDir    string
	checksums string
	certs     testCerts
	presented [][]byte // the chain the API presents
	roots     *x509.CertPool
	sigs      *fakeSignals
	probed    []string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	base := t.TempDir()
	e := &testEnv{
		t: t, node: newFakeNode(t), ask: &fakePrompter{t: t},
		journals: filepath.Join(base, "root", ".pco-appliance-install"),
		runDir:   filepath.Join(base, "run"),
		pveDir:   filepath.Join(base, "pve"),
		certs:    newCerts(t),
		roots:    x509.NewCertPool(),
		sigs:     &fakeSignals{},
	}
	e.presented = e.certs.node
	require.NoError(t, os.MkdirAll(e.runDir, 0o755))
	require.NoError(t, os.MkdirAll(e.pveDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(e.pveDir, "pve-root-ca.pem"), e.certs.clusterCA, 0o644))

	template := []byte("the template of pco " + testVersion)
	name := templateName(testVersion, "amd64")
	e.node.urls[ReleaseBase(testVersion)+"/"+name] = template
	sum := sha256.Sum256(template)
	e.checksums = filepath.Join(base, "checksums.txt")
	require.NoError(t, os.WriteFile(e.checksums, []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"+
		strings.Repeat("0", 64)+"  pco_"+testVersion+"_amd64.deb\n"), 0o644))

	e.in = New(e.node, e.ask, func() time.Time { return t0 }, mathrand.NewChaCha8([32]byte{7}), e.journals)
	e.in.h = host{
		euid:        func() int { return 0 },
		hostname:    func() (string, error) { return testNode + ".example.test", nil },
		findCommand: func(string) error { return nil },
		readFile:    os.ReadFile,
		pveDir:      e.pveDir,
		clusterCA:   filepath.Join(e.pveDir, "pve-root-ca.pem"),
		runDir:      e.runDir,
		probeCert: func(_ context.Context, address string) ([]*x509.Certificate, error) {
			e.probed = append(e.probed, address)
			return chain(t, e.presented), nil
		},
		systemRoots: func() (*x509.CertPool, error) { return e.roots, nil },
		signals:     e.sigs,
		sleep:       func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		version:     testVersion,
	}
	t.Cleanup(e.checkNoSecret)
	return e
}

// options are those of an install that asks nothing.
func (e *testEnv) options() Options {
	return Options{Yes: true, ChecksumsFile: e.checksums, KeepTemplate: true}
}

// install installs an appliance with the options, which must succeed.
func (e *testEnv) install(o Options) {
	e.t.Helper()
	require.NoError(e.t, e.in.Install(e.t.Context(), o), e.ask.text())
}

// checkNoSecret fails the test when a secret is in what the installer
// printed or left in its journal directory or under /run.
func (e *testEnv) checkNoSecret() {
	text := e.ask.text()
	for _, secret := range []string{pveSecret, cfToken} {
		require.NotContains(e.t, text, secret, "printed")
	}
	for _, dir := range []string{e.journals, e.runDir} {
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, rerr := os.ReadFile(path)
			require.NoError(e.t, rerr)
			for _, secret := range []string{pveSecret, cfToken} {
				require.NotContains(e.t, string(b), secret, "written to "+path)
			}
			return nil
		})
	}
}

// entries lists what a directory holds, nothing when it is not there.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

// journal returns the one journal the journal directory holds.
func (e *testEnv) journal() string {
	e.t.Helper()
	var found []string
	for _, n := range entries(e.t, e.journals) {
		if strings.HasSuffix(n, ".json") {
			found = append(found, filepath.Join(e.journals, n))
		}
	}
	require.Len(e.t, found, 1, "journals")
	return found[0]
}

// bootstrap returns the bootstrap of the nth init the fake node saw.
func (e *testEnv) bootstrap(n int) map[string]any {
	e.t.Helper()
	require.Greater(e.t, len(e.node.inits), n, "inits")
	return e.node.inits[n].Bootstrap
}

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o644) }
