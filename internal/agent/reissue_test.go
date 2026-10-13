package agent

import (
	"crypto/rand"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const day = 24 * time.Hour

func newCA(t *testing.T, at time.Time) pki.CA {
	t.Helper()
	ca, err := pki.NewCA(pkiInstall, at, rand.Reader)
	require.NoError(t, err)
	return ca
}

func caFiles(t *testing.T, ca pki.CA) store.CAFiles {
	t.Helper()
	certPEM, keyPEM, err := ca.PEM()
	require.NoError(t, err)
	return store.CAFiles{CertPEM: string(certPEM), KeyPEM: store.NewSecret(string(keyPEM))}
}

// reissueEnv is a host node's store with the install's CA, and the
// reissuer of node pve1.
type reissueEnv struct {
	t     *testing.T
	st    *store.Store
	ca    pki.CA
	clock *clock
	r     Reissuer
}

func newReissueEnv(t *testing.T) *reissueEnv {
	t.Helper()
	e := newEnv(t)
	re := &reissueEnv{t: t, st: e.store, clock: &clock{t: t0}}
	re.ca = newCA(t, t0.Add(-365*day))
	require.NoError(t, e.store.SaveCA(caFiles(t, re.ca), nil, time.Time{}))
	re.r = Reissuer{
		Node: testNode, InstallID: pkiInstall, Store: e.store, Files: TLSFiles{Dir: filepath.Join(t.TempDir(), "tls")},
		Now: re.clock.now, Log: zerolog.Nop(),
	}
	return re
}

// issued writes a client certificate issued at, under ca, as the node's own.
func (re *reissueEnv) issued(ca pki.CA, at time.Time) *x509.Certificate {
	re.t.Helper()
	id := pki.Identity{Kind: pki.KindHost, InstallID: pkiInstall, Node: testNode}
	key, err := pki.NewKey(rand.Reader)
	require.NoError(re.t, err)
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(re.t, err)
	cert, err := ca.IssueClient(csr, id, at, rand.Reader)
	require.NoError(re.t, err)
	require.NoError(re.t, re.r.Files.SaveClient(Creds{Key: key, Cert: cert, CA: ca.Cert}))
	require.NoError(re.t, re.st.SaveEnrolment(testNode, store.Enrolment{CertPEM: pemCert(cert)}))
	return cert
}

func (re *reissueEnv) enrolled() string {
	re.t.Helper()
	en, found, err := re.st.Enrolment(testNode)
	require.NoError(re.t, err)
	require.True(re.t, found)
	return en.SHA256
}

func TestANodeWithoutACertificateIssuesItsOwn(t *testing.T) {
	re := newReissueEnv(t)
	creds, issued, err := re.r.Ensure("")
	require.NoError(t, err)
	require.True(t, issued)
	id, err := pki.IdentityOf(creds.Cert)
	require.NoError(t, err)
	require.Equal(t, pki.Identity{Kind: pki.KindHost, InstallID: pkiInstall, Node: testNode}, id)
	require.Equal(t, re.ca.SHA256(), creds.CASHA256())
	require.Equal(t, pki.Fingerprint(creds.Cert), re.enrolled(), "the enrolment is the new certificate")

	onDisk, err := re.r.Files.Client()
	require.NoError(t, err)
	require.Equal(t, creds.Cert.Raw, onDisk.Cert.Raw)
	info, err := os.Stat(filepath.Join(re.r.Files.Dir, clientFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	server, err := re.r.Files.Server()
	require.NoError(t, err)
	require.Equal(t, []string{pki.NodeServerName(pkiInstall, testNode)}, server.Cert.DNSNames)
	require.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, server.Cert.ExtKeyUsage)

	t.Run("and keeps it while it is fresh", func(t *testing.T) {
		again, issued, err := re.r.Ensure(re.ca.SHA256())
		require.NoError(t, err)
		require.False(t, issued)
		require.Equal(t, creds.Cert.Raw, again.Cert.Raw)
	})
}

func TestACertificateTwentyNineDaysFromItsEndIsIssuedAgainAtStart(t *testing.T) {
	re := newReissueEnv(t)
	old := re.issued(re.ca, t0.Add(-61*day))
	require.WithinDuration(t, t0.Add(29*day), old.NotAfter, time.Hour)

	creds, issued, err := re.r.Ensure("")
	require.NoError(t, err)
	require.True(t, issued)
	require.NotEqual(t, old.SerialNumber, creds.Cert.SerialNumber)
	require.Equal(t, pki.Fingerprint(creds.Cert), re.enrolled(), "enrol/<node> is rewritten")
	require.WithinDuration(t, t0.Add(pki.LeafValidity), creds.Cert.NotAfter, 2*time.Hour)
}

func TestACertificateIsRenewedSixtyDaysAfterItWasIssued(t *testing.T) {
	re := newReissueEnv(t)
	old := re.issued(re.ca, t0.Add(-59*day))
	_, issued, err := re.r.Ensure("")
	require.NoError(t, err)
	require.False(t, issued)

	re.clock.advance(day)
	creds, issued, err := re.r.Ensure("")
	require.NoError(t, err)
	require.True(t, issued)
	require.NotEqual(t, old.SerialNumber, creds.Cert.SerialNumber)
}

func TestANewCAIsFollowed(t *testing.T) {
	re := newReissueEnv(t)
	old := re.issued(re.ca, t0)
	next := newCA(t, t0)
	require.NoError(t, re.st.SaveCA(caFiles(t, next), new(caFiles(t, re.ca)), t0))

	creds, issued, err := re.r.Ensure(next.SHA256())
	require.NoError(t, err)
	require.True(t, issued)
	require.Equal(t, next.SHA256(), creds.CASHA256())
	require.NoError(t, creds.Cert.CheckSignatureFrom(next.Cert))
	require.NotEqual(t, pki.Fingerprint(old), re.enrolled())
	require.Equal(t, pki.Fingerprint(creds.Cert), re.enrolled())
	server, err := re.r.Files.Server()
	require.NoError(t, err)
	require.Equal(t, next.SHA256(), server.CASHA256(), "the server certificate follows the CA as well")

	t.Run("leader.json names a CA the store does not hold yet", func(t *testing.T) {
		third := newCA(t, t0)
		_, _, err := re.r.Ensure(third.SHA256())
		require.ErrorContains(t, err, "waiting for them to agree")
		onDisk, err := re.r.Files.Client()
		require.NoError(t, err)
		require.Equal(t, creds.Cert.Raw, onDisk.Cert.Raw, "the certificate on disk is left as it was")
	})
}

func TestAnEnrolmentThatIsNotTheCertificateIsWrittenAgain(t *testing.T) {
	re := newReissueEnv(t)
	cert := re.issued(re.ca, t0)
	other := newCA(t, t0)
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	id := pki.Identity{Kind: pki.KindHost, InstallID: pkiInstall, Node: testNode}
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(t, err)
	foreign, err := other.IssueClient(csr, id, t0, rand.Reader)
	require.NoError(t, err)
	require.NoError(t, re.st.SaveEnrolment(testNode, store.Enrolment{CertPEM: pemCert(foreign)}))

	creds, issued, err := re.r.Ensure("")
	require.NoError(t, err)
	require.False(t, issued)
	require.Equal(t, cert.Raw, creds.Cert.Raw)
	require.Equal(t, pki.Fingerprint(cert), re.enrolled())
}

func TestADamagedCertificateFileIsIssuedAgain(t *testing.T) {
	re := newReissueEnv(t)
	re.issued(re.ca, t0)
	require.NoError(t, os.WriteFile(filepath.Join(re.r.Files.Dir, clientFile), []byte("junk"), 0o600))
	_, err := re.r.Files.Client()
	require.Error(t, err)
	_, issued, err := re.r.Ensure("")
	require.NoError(t, err)
	require.True(t, issued)
}

func TestTheCertificateFileHoldsAKeyOfItsCertificate(t *testing.T) {
	re := newReissueEnv(t)
	cert := re.issued(re.ca, t0)
	other, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	err = re.r.Files.SaveClient(Creds{Key: other, Cert: cert, CA: re.ca.Cert})
	require.NoError(t, err)
	_, err = re.r.Files.Client()
	require.ErrorContains(t, err, "not of the key")

	_, err = TLSFiles{Dir: t.TempDir()}.Client()
	require.ErrorIs(t, err, ErrNoCredentials)
}

func TestTheCertificateDirectoryIsTheAgentsAlone(t *testing.T) {
	re := newReissueEnv(t)
	cert := re.issued(re.ca, t0)
	dir := filepath.Join(t.TempDir(), "tls")
	require.NoError(t, os.Mkdir(dir, 0o755))
	creds, err := re.r.Files.Client()
	require.NoError(t, err)
	require.NoError(t, TLSFiles{Dir: dir}.SaveClient(Creds{Key: creds.Key, Cert: cert, CA: re.ca.Cert}))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "a directory made before is brought to 0700")
}
