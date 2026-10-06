package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

func testLeaf(t *testing.T, notAfter time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "pve1"},
		NotBefore: t0.Add(-time.Hour), NotAfter: notAfter,
		DNSNames:    []string{"pve1", "pve1.example.lan"},
		IPAddresses: []net.IP{net.ParseIP("192.0.2.10")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return leaf
}

func TestWebCertOutput(t *testing.T) {
	leaf := testLeaf(t, t0.Add(90*24*time.Hour))
	a := &app{env: testEnv()}
	var out bytes.Buffer

	require.NoError(t, a.printWebCert(&out, setup.WebCertStatus{Mode: webcert.ModeCA, Leaf: leaf}))

	// A new key makes another fingerprint every run: the golden has a place
	// for it.
	got := out.String()
	require.Contains(t, got, webcert.Fingerprint(leaf))
	requireGolden(t, "web_cert.golden", strings.ReplaceAll(got, webcert.Fingerprint(leaf), "<fingerprint>"))
}

func TestWebCertOutputOfACertificateThatExpired(t *testing.T) {
	a := &app{env: testEnv()}
	for _, tt := range []struct {
		mode     string
		notAfter time.Time
		want     []string
	}{
		{webcert.ModeOwn, t0.Add(-time.Hour), []string{"own: the admin's own certificate and key", "(expired)"}},
		{webcert.ModePVEProxy, t0.Add(36 * time.Hour), []string{"pveproxy: the certificate and the key pveproxy serves", "(in 1 day)"}},
	} {
		var out bytes.Buffer

		require.NoError(t, a.printWebCert(&out, setup.WebCertStatus{Mode: tt.mode, Leaf: testLeaf(t, tt.notAfter)}))

		for _, want := range tt.want {
			require.Contains(t, out.String(), want)
		}
	}
}

func TestTheCertificateOfTheAppliance(t *testing.T) {
	a := &app{env: applianceEnv(t, "192.0.2.30", nil)}
	a.webDir = filepath.Join(t.TempDir(), "web")
	_, err := a.applianceWebCert()
	require.EqualError(t, err, "the web interface has no certificate yet: the daemon makes one as it starts, and systemctl status pco says whether it runs")

	names, err := webcert.ApplianceNames("pco", "", "192.0.2.30:8643", nil)
	require.NoError(t, err)
	leaf, err := webcert.MakeSelfSigned(a.webDir, names, t0, rand.Reader)
	require.NoError(t, err)
	st, err := a.applianceWebCert()
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, a.printWebCert(&out, st))

	require.Contains(t, out.String(), "mode         self-signed: a key of its own and a self-signed certificate, renewed by pco\n")
	require.Contains(t, out.String(), "names        pco, 192.0.2.30\n")
	require.Contains(t, out.String(), "valid until  ")
	require.Contains(t, out.String(), "(in 396 days)", "397 days from a time the certificate keeps to the second")
	require.Contains(t, out.String(), "SHA-256      "+webcert.Fingerprint(leaf)+"\n")
}

func TestSetupOptionsOfTheWebInterface(t *testing.T) {
	a, cmd, _, _ := commandWith(unreadable{t}, false)

	o, err := a.setupOptions(cmd, setupFlags{webCert: "own", webCertFile: "a.crt", webKeyFile: "a.key", webListen: "192.0.2.10"})
	require.NoError(t, err)
	require.Equal(t, setup.Options{WebCert: "own", WebCertFile: "a.crt", WebKeyFile: "a.key", WebListen: "192.0.2.10"}, o)

	o, err = a.setupOptions(cmd, setupFlags{noWeb: true})
	require.NoError(t, err)
	require.Equal(t, setup.Options{NoWeb: true}, o)
}

func TestTheCommandsOfTheWebCertificateAreThere(t *testing.T) {
	root := newRootCmdWith(testEnv())
	for _, args := range [][]string{{"web", "cert"}, {"web", "cert", "renew"}, {"web", "cert", "import"}} {
		cmd, _, err := root.Find(args)
		require.NoError(t, err)
		require.Equal(t, args[len(args)-1], cmd.Name())
	}
	imp, _, err := root.Find([]string{"web", "cert", "import"})
	require.NoError(t, err)
	require.Error(t, imp.Args(imp, []string{"only.crt"}), "a certificate and its key")
}
