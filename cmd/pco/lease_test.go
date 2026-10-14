package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
)

// dirLock is the lock of the lease in a plain directory, as a node that
// stands alone keeps it for a daemon of the tests.
type dirLock struct{}

func (dirLock) Mkdir(path string) error { return os.Mkdir(path, 0o755) }

func (dirLock) Refresh(path string, mtime time.Time) error {
	return os.Chtimes(path, time.Unix(0, 0), mtime)
}

func (dirLock) UnlockRequest(path string) error {
	return os.Chtimes(path, time.Unix(0, 0), time.Unix(0, 0))
}

func (dirLock) Rmdir(path string) error { return os.Remove(path) }

// standaloneLease is what the daemon of a test keeps its lease with: the
// cluster filesystem of a node that stands alone, below base.
func standaloneLease(t *testing.T, base, node string) daemon.LeaseDeps {
	t.Helper()
	pve := filepath.Join(base, "pve")
	require.NoError(t, os.MkdirAll(filepath.Join(pve, "priv", "lock"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(pve, ".members"), []byte(`{"nodename": "`+node+`", "version": 0}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(pve, "pve-root-ca.pem"), selfSigned(t), 0o600))
	return daemon.LeaseDeps{
		FS:          dirLock{},
		BootID:      func() (string, error) { return "boot-1", nil },
		PVEDir:      pve,
		ProcessLock: filepath.Join(base, "lease.lock"),
	}
}

// selfSigned is a CA certificate in PEM, as Proxmox makes for a node.
func selfSigned(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Proxmox Virtual Environment"},
		NotBefore: t0, NotAfter: t0.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
