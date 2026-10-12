package store

import (
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
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sha is the SHA-256 of s in lower-case hex, a fingerprint for tests.
func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// testCert makes a self-signed certificate and its key, in PEM.
func testCert(t *testing.T, serial int64) (certPEM, keyPEM string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: fmt.Sprintf("test %d", serial)},
		NotBefore:    t0.Add(-time.Hour),
		NotAfter:     t0.Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})), cert
}

func testCA(t *testing.T, serial int64) CAFiles {
	t.Helper()
	certPEM, keyPEM, _ := testCert(t, serial)
	return CAFiles{CertPEM: certPEM, KeyPEM: NewSecret(keyPEM)}
}

func TestTheCARoundTripsOnThePrivateRoot(t *testing.T) {
	s, p := openStore(t)
	_, _, _, err := s.CA()
	require.ErrorIs(t, err, ErrNoCA)

	current := testCA(t, 1)
	require.NoError(t, s.SaveCA(current, nil, time.Time{}))
	got, previous, rotatedAt, err := s.CA()
	require.NoError(t, err)
	require.Equal(t, current.CertPEM, got.CertPEM)
	require.True(t, current.KeyPEM.Equal(got.KeyPEM))
	require.Nil(t, previous)
	require.True(t, rotatedAt.IsZero())
	require.Equal(t, []string{"ca/current.json"}, stored(t, p.Private))
	require.Empty(t, stored(t, p.Cluster), "the key never leaves the private root")
	require.NotContains(t, fmt.Sprintf("%v %+v %#v", got, got, got), "PRIVATE KEY")

	next := testCA(t, 2)
	require.NoError(t, s.SaveCA(next, &current, t0))
	got, previous, rotatedAt, err = s.CA()
	require.NoError(t, err)
	require.Equal(t, next.CertPEM, got.CertPEM)
	require.NotNil(t, previous)
	require.Equal(t, current.CertPEM, previous.CertPEM)
	require.True(t, current.KeyPEM.Equal(previous.KeyPEM))
	require.Equal(t, t0, rotatedAt)

	require.NoError(t, s.SaveCA(next, nil, t0), "after the window the previous one goes")
	_, previous, _, err = s.CA()
	require.NoError(t, err)
	require.Nil(t, previous)
	require.Equal(t, []string{"ca/current.json"}, stored(t, p.Private))
}

func TestTheFirstCAIsStoredOnlyWhileThereIsNone(t *testing.T) {
	s, _ := openStore(t)
	first, second := testCA(t, 1), testCA(t, 2)

	require.NoError(t, s.SaveCAIfAbsent(first))
	err := s.SaveCAIfAbsent(second)

	require.ErrorIs(t, err, ErrRevision)
	got, previous, _, err := s.CA()
	require.NoError(t, err)
	require.Equal(t, first.CertPEM, got.CertPEM, "the CA stored first stays the install's")
	require.True(t, first.KeyPEM.Equal(got.KeyPEM))
	require.Nil(t, previous)
	require.Error(t, s.SaveCAIfAbsent(CAFiles{CertPEM: "no certificate"}))
}

func TestSaveCARefusesWhatIsNoCA(t *testing.T) {
	s, p := openStore(t)
	good := testCA(t, 1)
	require.ErrorContains(t, s.SaveCA(CAFiles{CertPEM: "not pem", KeyPEM: good.KeyPEM}, nil, time.Time{}), "certificate")
	require.ErrorContains(t, s.SaveCA(CAFiles{CertPEM: good.CertPEM}, nil, time.Time{}), "key")
	require.ErrorContains(t, s.SaveCA(CAFiles{CertPEM: good.CertPEM, KeyPEM: NewSecret(good.CertPEM)}, nil, time.Time{}), "key")
	require.ErrorContains(t, s.SaveCA(good, &CAFiles{}, t0), "previous")
	require.ErrorContains(t, s.SaveCA(good, &good, time.Time{}), "rotated")
	require.Empty(t, stored(t, p.Private))
}

func TestAnEnrolmentKeepsTheCertificateWithItsSerialAndFingerprint(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.Enrolment("pve-b")
	require.NoError(t, err)
	require.False(t, found)

	certPEM, _, cert := testCert(t, 0x1f2e)
	require.NoError(t, s.SaveEnrolment("pve-b", Enrolment{CertPEM: certPEM}))

	got, found, err := s.Enrolment("pve-b")
	require.NoError(t, err)
	require.True(t, found)
	sum := sha256.Sum256(cert.Raw)
	require.Equal(t, Enrolment{Node: "pve-b", CertPEM: certPEM, Serial: "1f2e", SHA256: hex.EncodeToString(sum[:])}, got)
	require.Equal(t, []string{"enrol/pve-b.json"}, stored(t, p.Private))

	require.ErrorContains(t, s.SaveEnrolment("pve-b", Enrolment{CertPEM: certPEM, SHA256: sha("other")}), "SHA-256")
	require.ErrorContains(t, s.SaveEnrolment("pve-b", Enrolment{CertPEM: certPEM, Serial: "99"}), "serial")
	require.ErrorContains(t, s.SaveEnrolment("pve-b", Enrolment{Node: "pve-c", CertPEM: certPEM}), "pve-c")
	require.ErrorContains(t, s.SaveEnrolment("pve-b", Enrolment{CertPEM: "garbage"}), "certificate")

	require.NoError(t, s.DeleteEnrolment("pve-b"))
	_, found, err = s.Enrolment("pve-b")
	require.NoError(t, err)
	require.False(t, found)
}

func joinRecord(id, node string) JoinRecord {
	return JoinRecord{ID: id, Node: node, SecretSHA256: sha("secret of " + id), CreatedAt: t0, Expires: t0.Add(15 * time.Minute)}
}

func TestJoinTokensKeepOnlyTheHashOfTheSecret(t *testing.T) {
	s, p := openStore(t)
	got, err := s.JoinTokens()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	r, other := joinRecord("t1", "pve-b"), joinRecord("t2", "pve-c")
	require.NoError(t, s.SaveJoinToken(r))
	require.NoError(t, s.SaveJoinToken(other))
	got, err = s.JoinTokens()
	require.NoError(t, err)
	require.Equal(t, map[string]JoinRecord{"t1": r, "t2": other}, got)
	require.Equal(t, []string{"jointokens/t1.json", "jointokens/t2.json"}, stored(t, p.Private))

	for _, bad := range []JoinRecord{
		{Node: "pve-b", SecretSHA256: sha("x"), Expires: t0},
		{ID: "t2", SecretSHA256: sha("x"), Expires: t0},
		{ID: "t2", Node: "pve-b", SecretSHA256: "secret", Expires: t0},
		{ID: "t2", Node: "pve-b", SecretSHA256: sha("x")},
	} {
		require.Error(t, s.SaveJoinToken(bad), "%+v", bad)
	}

	require.NoError(t, s.DeleteJoinToken("t1"))
	got, err = s.JoinTokens()
	require.NoError(t, err)
	require.Equal(t, map[string]JoinRecord{"t2": other}, got)
}

func TestAJoinTokenIsMarkedUsedOnEveryUse(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveJoinToken(joinRecord("t1", "pve-b")))
	first := JoinUse{At: t0, Source: netip.MustParseAddr("192.168.4.31"), VMID: 9101, MACs: []string{"bc:24:11:00:00:01"}}
	second := JoinUse{At: t0.Add(time.Minute), Source: netip.MustParseAddr("192.168.4.99"), VMID: 9102}

	r, err := s.UseJoinToken("t1", first)
	require.NoError(t, err)
	require.Equal(t, []JoinUse{first}, r.Uses)

	r, err = s.UseJoinToken("t1", second)
	require.NoError(t, err)
	require.Equal(t, []JoinUse{first, second}, r.Uses, "a second use is told with the first: the node is contested")

	stored, err := s.JoinTokens()
	require.NoError(t, err)
	require.Equal(t, r, stored["t1"])

	_, err = s.UseJoinToken("t9", first)
	require.ErrorIs(t, err, ErrNoJoinToken)
}

// A token is made once: saving it again, as a retry after a crash would,
// must not give back a use.
func TestAJoinTokenIsMadeOnceAndNeverLosesAUse(t *testing.T) {
	s, _ := openStore(t)
	made := joinRecord("t1", "pve-b")
	require.NoError(t, s.SaveJoinToken(made))
	_, err := s.UseJoinToken("t1", JoinUse{At: t0})
	require.NoError(t, err)

	require.ErrorIs(t, s.SaveJoinToken(made), ErrRevision)
	r, rev, found, err := s.JoinToken("t1")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 2, rev)
	require.Len(t, r.Uses, 1, "the use is still there")

	longer := r
	longer.Expires = r.Expires.Add(time.Minute)
	_, err = s.SaveJoinTokenIf(longer, 1)
	require.ErrorIs(t, err, ErrRevision, "a change goes at the revision it was read at")

	unused := longer
	unused.Uses = nil
	_, err = s.SaveJoinTokenIf(unused, 2)
	require.ErrorContains(t, err, "use")
	require.NotErrorIs(t, err, ErrRevision)

	other := longer
	other.Uses = []JoinUse{{At: t0.Add(time.Hour)}}
	_, err = s.SaveJoinTokenIf(other, 2)
	require.ErrorContains(t, err, "use", "a use is never written over")

	rev, err = s.SaveJoinTokenIf(longer, 2)
	require.NoError(t, err)
	require.EqualValues(t, 3, rev)
	r, _, _, err = s.JoinToken("t1")
	require.NoError(t, err)
	require.Equal(t, longer, r)

	_, _, found, err = s.JoinToken("t9")
	require.NoError(t, err)
	require.False(t, found)
}

// Uses that race each other each find the record as the others left it:
// a revision that moved in between is read again, and no use is lost.
func TestUsesOfAJoinTokenAtOnceAreAllKept(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveJoinToken(joinRecord("t1", "pve-b")))
	const uses = 8
	counts := make([]int, uses)
	var wg sync.WaitGroup
	for i := range uses {
		wg.Go(func() {
			r, err := s.UseJoinToken("t1", JoinUse{At: t0, VMID: 9100 + i})
			if err == nil {
				counts[i] = len(r.Uses)
			}
		})
	}
	wg.Wait()

	got, err := s.JoinTokens()
	require.NoError(t, err)
	require.Len(t, got["t1"].Uses, uses)
	require.ElementsMatch(t, []int{1, 2, 3, 4, 5, 6, 7, 8}, counts, "each use saw the ones before it")
}

func TestTheCARotationIsKeptOnTheClusterRoot(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.CARotation()
	require.NoError(t, err)
	require.False(t, found)

	r := CARotation{Node: "pve-c", StartedAt: t0, Actor: "root@pam", Done: []string{"credentials", "tunnel secrets"}}
	require.NoError(t, s.SaveCARotation(r))
	got, found, err := s.CARotation()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, r, got)
	require.Equal(t, []string{"meta/ca-rotation.json"}, stored(t, p.Cluster))

	require.ErrorContains(t, s.SaveCARotation(CARotation{StartedAt: t0}), "node")
	require.ErrorContains(t, s.SaveCARotation(CARotation{Node: "pve-c"}), "start")
}

func TestThePrivateKindsAreSweptOfLeftovers(t *testing.T) {
	p := testPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, s.SaveJoinToken(joinRecord("t1", "pve-b")))
	leftover := filepath.Join(p.Private, "jointokens", ".t1.json.123"+tempExt)
	writeFile(t, leftover, "{")
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(leftover, old, old))

	_, err = Open(p)

	require.NoError(t, err)
	require.NoFileExists(t, leftover)
}
