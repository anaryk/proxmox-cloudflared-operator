package members

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fingerprints are what openssl x509 -noout -fingerprint -sha256 prints for
// the certificates of the fixtures, in lower case and without the colons.
const (
	fingerprintOfFirst  = "e73c3ec33a81817fccf1815a77edcdba4a648b22c1b5a1b62005438a829bf8ae"
	fingerprintOfSecond = "19f941b4a67d814b3a7f7ce226306f068d4315df7f2f6c8a012d1d9f70b4519e"
)

func TestCAFingerprintIsTheSHA256OfTheDER(t *testing.T) {
	got, err := CAFingerprint(fixture(t, "pve-root-ca.pem"))
	require.NoError(t, err)
	require.Equal(t, fingerprintOfFirst, got)
}

func TestCAFingerprintOfFileWithTwoCertificatesIsTheFirstOnes(t *testing.T) {
	got, err := CAFingerprint(fixture(t, "pve-root-ca-two.pem"))
	require.NoError(t, err)
	require.Equal(t, fingerprintOfFirst, got)
	require.NotEqual(t, fingerprintOfSecond, got)
}

func TestCAFingerprintSkipsBlocksThatAreNoCertificate(t *testing.T) {
	cert, err := os.ReadFile(fixture(t, "pve-root-ca.pem"))
	require.NoError(t, err)
	in := "-----BEGIN EC PARAMETERS-----\nBggqhkjOPQMBBw==\n-----END EC PARAMETERS-----\n" + string(cert)
	p := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(p, []byte(in), 0o600))

	got, err := CAFingerprint(p)
	require.NoError(t, err)
	require.Equal(t, fingerprintOfFirst, got)
}

func TestCAFingerprintDoesNotDependOnHowThePEMIsWrapped(t *testing.T) {
	cert, err := os.ReadFile(fixture(t, "pve-root-ca.pem"))
	require.NoError(t, err)
	crlf := []byte{}
	for _, c := range cert {
		if c == '\n' {
			crlf = append(crlf, '\r')
		}
		crlf = append(crlf, c)
	}
	p := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(p, crlf, 0o600))

	got, err := CAFingerprint(p)
	require.NoError(t, err)
	require.Equal(t, fingerprintOfFirst, got)
}

func TestCAFingerprintRefuses(t *testing.T) {
	cert, err := os.ReadFile(fixture(t, "pve-root-ca.pem"))
	require.NoError(t, err)
	tests := []struct {
		name, in string
	}{
		{"an empty file", ""},
		{"text", "this is no certificate\n"},
		{"a block that is no certificate", "-----BEGIN EC PARAMETERS-----\nBggqhkjOPQMBBw==\n-----END EC PARAMETERS-----\n"},
		{"a certificate that does not parse", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"},
		{"a certificate cut short", string(cert[:len(cert)/2])},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "ca.pem")
			require.NoError(t, os.WriteFile(p, []byte(tt.in), 0o600))
			got, err := CAFingerprint(p)
			require.Error(t, err)
			require.Empty(t, got)
			require.ErrorContains(t, err, "ca.pem")
		})
	}
}

func TestCAFingerprintOfAMissingFileIsAnErrorTheCallerCanTell(t *testing.T) {
	_, err := CAFingerprint(filepath.Join(t.TempDir(), "pve-root-ca.pem"))
	require.ErrorIs(t, err, fs.ErrNotExist)
}
