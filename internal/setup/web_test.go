package setup

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

const testAddr = "192.0.2.10"

// clusterStatus is what pvesh prints of a cluster of two nodes, of which this
// one, pve1, has addr.
func clusterStatus(addr string) string {
	return `[{"type":"cluster","name":"lab","quorate":1,"nodes":2},` +
		`{"type":"node","name":"pve1","ip":"` + addr + `","local":1,"online":1,"nodeid":1},` +
		`{"type":"node","name":"pve2","ip":"192.0.2.20","local":0,"online":1,"nodeid":2}]`
}

// webNode gives the node the unit of the web interface, a cluster CA as
// Proxmox VE makes one and the certificate pveproxy serves, pve-ssl.pem. It
// returns the CA.
func (e *testEnv) webNode() *x509.Certificate {
	e.t.Helper()
	e.installUnit(webUnit)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(e.t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Proxmox Virtual Environment"},
		NotBefore: t0.AddDate(-1, 0, 0), NotAfter: t0.AddDate(9, 0, 0),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(e.t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(e.t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(e.t, err)
	e.writeFile(e.s.host.clusterCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o640)
	e.writeFile(e.s.host.clusterCAKey, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	e.pveproxyCert("pve-ssl")
	return ca
}

// pveproxyCert writes a certificate and key for pveproxy as name.pem and
// name.key, and returns the certificate.
func (e *testEnv) pveproxyCert(name string) []byte {
	e.t.Helper()
	certPEM, keyPEM, err := webcert.SelfSigned(webcert.Names{DNS: []string{testNode}}, t0, 365*24*time.Hour, rand.Reader)
	require.NoError(e.t, err)
	e.writeFile(filepath.Join(e.s.host.nodeCertDir, name+".pem"), certPEM, 0o640)
	e.writeFile(filepath.Join(e.s.host.nodeCertDir, name+".key"), keyPEM, 0o640)
	return certPEM
}

func (e *testEnv) writeFile(path string, b []byte, mode os.FileMode) {
	e.t.Helper()
	require.NoError(e.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(e.t, os.WriteFile(path, b, mode))
}

// ownPair is a certificate and key of the admin's for names, in files.
func (e *testEnv) ownPair(names ...string) (certFile, keyFile string) {
	e.t.Helper()
	certPEM, keyPEM, err := webcert.SelfSigned(webcert.Names{DNS: names}, t0, 200*24*time.Hour, rand.Reader)
	require.NoError(e.t, err)
	dir := e.t.TempDir()
	certFile, keyFile = filepath.Join(dir, "web.crt"), filepath.Join(dir, "web.key")
	e.writeFile(certFile, certPEM, 0o644)
	e.writeFile(keyFile, keyPEM, 0o600)
	return certFile, keyFile
}

func (e *testEnv) webFile(name string) string { return filepath.Join(e.s.host.webDir, name) }

func (e *testEnv) webLeaf() *x509.Certificate {
	e.t.Helper()
	leaf, err := readCert(e.webFile(webcert.CertName))
	require.NoError(e.t, err)
	return leaf
}

func (e *testEnv) read(path string) string {
	e.t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(e.t, err)
	return string(b)
}

func (e *testEnv) linkOf(path string) string {
	e.t.Helper()
	target, err := os.Readlink(path)
	require.NoError(e.t, err, "%s is a link", path)
	return target
}

func requireMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.Equal(t, mode, info.Mode().Perm(), path)
}

func requireTrusted(t *testing.T, leaf, ca *x509.Certificate, name string) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots, CurrentTime: t0})
	require.NoError(t, err, name)
}

func countOfLine(lines []string, line string) int {
	n := 0
	for _, l := range lines {
		if l == line {
			n++
		}
	}
	return n
}

func full() Options { return Options{Yes: true, CloudflareToken: cfToken, Node: testNode} }

func TestSetupGivesTheWebInterfaceALeafOfTheClusterCA(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	ca := e.webNode()
	h := newFakeHost(t)
	e.onHost(h)

	require.NoError(t, e.setup(full()))

	require.Equal(t, "PCO_WEB_LISTEN=192.0.2.10:8643\n", e.read(e.s.host.webEnv))
	requireMode(t, e.s.host.webEnv, 0o644)
	leaf := e.webLeaf()
	for _, name := range []string{"pve1", "pve1.example.com", "192.0.2.10"} {
		requireTrusted(t, leaf, ca, name)
	}
	require.Equal(t, []string{"pve1", "pve1.example.com"}, leaf.DNSNames)
	require.Len(t, leaf.IPAddresses, 1, "the address of this node, not the one of pve2")
	require.Equal(t, t0.Add(webcert.Lifetime), leaf.NotAfter)
	require.False(t, leaf.IsCA)
	requireMode(t, e.webFile(webcert.KeyName), 0o600)
	requireMode(t, e.webFile(webcert.CertName), 0o644)
	requireMode(t, e.s.host.webDir, 0o700)
	require.Equal(t, filepath.Join(e.s.host.nodeCertDir, "pve-ssl.pem"), e.linkOf(e.webFile(webcert.PinName)))

	require.True(t, h.enabled[webUnit])
	require.True(t, h.active[webUnit])
	web := slices.Index(h.ran, "systemctl enable --now pco-web.service")
	require.GreaterOrEqual(t, web, 0, "%v", h.ran)
	require.Less(t, web, slices.Index(h.ran, "systemctl enable --now pco.service"))
	require.NotContains(t, h.ran, "systemctl try-restart pco-web.service", "nothing ran with other files before")

	m := e.manifest()
	require.True(t, m.WebEnabled)
	require.True(t, m.WebEnv)
	require.Equal(t, webcert.ModeCA, m.WebCert)
	require.Equal(t, []string{
		filepath.Dir(e.s.host.webDir), e.s.host.webDir,
		e.webFile(webcert.CertName), e.webFile(webcert.KeyName), e.webFile(webcert.PinName),
	}, m.WebTLS)

	e.requireShown("info: web interface: https://pve1.example.com:8643/")
	e.requireShown("info: web certificate: SHA-256 fingerprint " + webcert.Fingerprint(leaf))
	e.requireShown("once the cluster CA, " + e.s.host.clusterCA + ", is imported, as for port 8006")
	require.NotContains(t, e.ask.text(), "firewall")
	e.requireNoSecret()
}

func TestASecondSetupKeepsTheWebInterfaceAsItIs(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(full()))
	cert, key := e.read(e.webFile(webcert.CertName)), e.read(e.webFile(webcert.KeyName))
	m := e.manifest()

	for _, o := range []Options{{Yes: true, Node: testNode}, {Yes: true, Repair: true, Node: testNode}} {
		h.ran = nil
		require.NoError(t, e.setup(o))

		require.Equal(t, cert, e.read(e.webFile(webcert.CertName)))
		require.Equal(t, key, e.read(e.webFile(webcert.KeyName)))
		require.Equal(t, m, e.manifest())
		require.NotContains(t, h.ran, "systemctl try-restart pco-web.service")
		require.Contains(t, h.ran, "systemctl enable --now pco-web.service")
	}
	e.requireShown("web certificate: kept, signed by the cluster CA and valid until 2026-12-30")
	e.requireShown("web interface: listens on 192.0.2.10:8643, nothing needed")
}

func TestANewListenAddressGetsANewLeafAndARestart(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	ca := e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(full()))
	h.ran = nil

	require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode, WebListen: "198.51.100.4"}))

	require.Equal(t, "PCO_WEB_LISTEN=198.51.100.4:8643\n", e.read(e.s.host.webEnv))
	leaf := e.webLeaf()
	requireTrusted(t, leaf, ca, "198.51.100.4")
	requireTrusted(t, leaf, ca, "192.0.2.10")
	require.Equal(t, 1, countOfLine(h.ran, "systemctl try-restart pco-web.service"))
	require.Less(t, slices.Index(h.ran, "systemctl try-restart pco-web.service"), slices.Index(h.ran, "systemctl enable --now pco-web.service"))
	e.requireShown("now listens on 198.51.100.4:8643")
}

func TestANewAddressOfTheNodeGetsANewLeaf(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	ca := e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(full()))

	h.addr = "192.0.2.11"
	require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}))

	requireTrusted(t, e.webLeaf(), ca, "192.0.2.11")
	require.Equal(t, "PCO_WEB_LISTEN=192.0.2.10:8643\n", e.read(e.s.host.webEnv), "where it listens stays the admin's")
}

func TestTheEnvironmentFileKeepsWhatTheAdminWrote(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	ca := e.webNode()
	e.writeFile(e.s.host.webEnv, []byte("# mine\nPCO_WEB_HOSTS=pve.example.org,203.0.113.9\nPCO_WEB_HSTS=1"), 0o640)
	h := newFakeHost(t)
	e.onHost(h)

	require.NoError(t, e.setup(full()))

	require.Equal(t, "# mine\nPCO_WEB_HOSTS=pve.example.org,203.0.113.9\nPCO_WEB_HSTS=1\nPCO_WEB_LISTEN=192.0.2.10:8643\n",
		e.read(e.s.host.webEnv))
	requireMode(t, e.s.host.webEnv, 0o640)
	requireTrusted(t, e.webLeaf(), ca, "pve.example.org")
	requireTrusted(t, e.webLeaf(), ca, "203.0.113.9")
	require.False(t, e.manifest().WebEnv, "the file was the admin's")

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true, RemoveCloudflared: true}))
	require.FileExists(t, e.s.host.webEnv, "uninstall leaves the admin's file")
}

func TestAHostThatIsNoNameFailsTheWebStep(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	e.writeFile(e.s.host.webEnv, []byte("PCO_WEB_HOSTS=pve.example.org,*.example.org\n"), 0o644)
	h := newFakeHost(t)
	e.onHost(h)

	err := e.setup(full())

	require.EqualError(t, err, `setup step web: PCO_WEB_HOSTS lists "*.example.org", `+
		"which is neither an address nor a host name such as pve.example.org")
	require.NoFileExists(t, e.webFile(webcert.CertName), "no leaf for names that are none")
}

func TestTheWebInterfaceWithTheAdminsOwnCertificate(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	crt, key := e.ownPair("pve1.example.com")
	o := full()
	o.WebCert, o.WebCertFile, o.WebKeyFile = webcert.ModeOwn, crt, key

	require.NoError(t, e.setup(o))

	require.Equal(t, e.read(crt), e.read(e.webFile(webcert.CertName)))
	require.Equal(t, e.read(key), e.read(e.webFile(webcert.KeyName)))
	requireMode(t, e.webFile(webcert.KeyName), 0o600)
	require.Equal(t, webcert.ModeOwn, e.manifest().WebCert)
	e.requireShown("web certificate: the admin's own, copied in from " + crt)
	require.NotContains(t, e.ask.text(), "is imported, as for port 8006")

	require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}))
	require.Equal(t, e.read(crt), e.read(e.webFile(webcert.CertName)), "a second run keeps it")
	require.Equal(t, webcert.ModeOwn, e.manifest().WebCert)
	e.requireShown("web certificate: the admin's own, kept")
}

func TestTheAdminsOwnCertificateIsChecked(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	crt, _ := e.ownPair("pve1.example.com")
	_, otherKey := e.ownPair("pve1.example.com")
	elsewhere, elsewhereKey := e.ownPair("www.example.org")

	for _, tt := range []struct {
		name, crt, key, err string
	}{
		{"a key of another certificate", crt, otherKey, "the key is not the key of the certificate"},
		{"another host", elsewhere, elsewhereKey, "the certificate names none of pve1, pve1.example.com, 192.0.2.10"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := full()
			o.WebCert, o.WebCertFile, o.WebKeyFile = webcert.ModeOwn, tt.crt, tt.key

			err := e.setup(o)

			require.ErrorContains(t, err, "setup step web: ")
			require.ErrorContains(t, err, tt.err)
			require.NoFileExists(t, e.webFile(webcert.CertName))
			require.NotContains(t, h.ran, "systemctl enable --now pco-web.service")
		})
	}
}

func TestTheWebInterfaceWithTheCertificateOfPveproxy(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	o := full()
	o.WebCert = webcert.ModePVEProxy

	require.NoError(t, e.setup(o))

	local := e.s.host.nodeCertDir
	require.Equal(t, filepath.Join(local, "pve-ssl.pem"), e.linkOf(e.webFile(webcert.CertName)))
	require.Equal(t, filepath.Join(local, "pve-ssl.key"), e.linkOf(e.webFile(webcert.KeyName)))
	require.Equal(t, webcert.ModePVEProxy, e.manifest().WebCert)
	e.requireShown("warn: --web-cert pveproxy: pco-web holds pveproxy's own key")

	// An ACME certificate is installed: the links follow it.
	e.pveproxyCert("pveproxy-ssl")
	h.ran = nil
	require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}))
	require.Equal(t, filepath.Join(local, "pveproxy-ssl.pem"), e.linkOf(e.webFile(webcert.CertName)))
	require.Equal(t, filepath.Join(local, "pveproxy-ssl.key"), e.linkOf(e.webFile(webcert.KeyName)))
	require.Equal(t, filepath.Join(local, "pveproxy-ssl.pem"), e.linkOf(e.webFile(webcert.PinName)))
	require.Equal(t, 1, countOfLine(h.ran, "systemctl try-restart pco-web.service"))

	// And back to a key of its own.
	pveproxy := e.read(filepath.Join(local, "pveproxy-ssl.key"))
	require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode, WebCert: webcert.ModeCA}))
	for _, name := range []string{webcert.CertName, webcert.KeyName} {
		info, err := os.Lstat(e.webFile(name))
		require.NoError(t, err)
		require.True(t, info.Mode().IsRegular(), "%s is a file again", name)
	}
	require.NotEqual(t, pveproxy, e.read(e.webFile(webcert.KeyName)))
	require.Equal(t, pveproxy, e.read(filepath.Join(local, "pveproxy-ssl.key")), "pveproxy's key is left as it is")
	require.Equal(t, webcert.ModeCA, e.manifest().WebCert)
}

func TestNoWebLeavesTheWebInterfaceOut(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	o := full()
	o.NoWeb = true

	require.NoError(t, e.setup(o))

	for _, line := range h.ran {
		require.NotContains(t, line, webUnit)
		require.NotContains(t, line, "/cluster/status")
	}
	require.NoFileExists(t, e.s.host.webEnv)
	require.NoDirExists(t, e.s.host.webDir)
	m := e.manifest()
	require.False(t, m.WebEnabled)
	require.Empty(t, m.WebCert)
	require.Empty(t, m.WebTLS)
	e.requireShown("web interface: skipped (--no-web)")
}

func TestSetupWithoutTheUnitOfTheWebInterfaceLeavesItOut(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)

	require.NoError(t, e.setup(full()))

	require.NoFileExists(t, e.s.host.webEnv)
	require.False(t, h.enabled[webUnit])
	e.requireShown("web interface: pco-web.service is not installed")
}

func TestSetupShowsTheRuleTheFirewallNeeds(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	h.firewall = true
	e.onHost(h)

	require.NoError(t, e.setup(full()))

	e.requireShown("firewall: the Proxmox VE firewall is on; allow TCP port 8643 to this node, for example with " +
		"pvesh create /nodes/pve1/firewall/rules --type in --action ACCEPT --proto tcp --dport 8643 --enable 1")
}

func TestSetupWithoutAnAddressOfTheNodeAsksForOne(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	h.addr = "2001:db8::10"
	e.onHost(h)

	err := e.setup(full())

	require.EqualError(t, err, "setup step web: the cluster status lists no address of node pve1: "+
		"give the address pco-web listens on with --web-listen")
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode, WebListen: "[2001:db8::10]:9443"}))
	require.Equal(t, "PCO_WEB_LISTEN=[2001:db8::10]:9443\n", e.read(e.s.host.webEnv))
	e.requireShown("web interface: https://pve1.example.com:9443/")
}

// The web step asks Proxmox the same in every mode; what differs is on disk.
func TestTheWebStepRunsTheSameCommandsInEveryMode(t *testing.T) {
	for _, mode := range []string{webcert.ModeCA, webcert.ModeOwn, webcert.ModePVEProxy} {
		t.Run(mode, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			e.webNode()
			o := full()
			o.WebCert = mode
			if mode == webcert.ModeOwn {
				o.WebCertFile, o.WebKeyFile = e.ownPair("pve1.example.com")
			}
			e.script(preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(),
				tagsAdded("", "cf-tunnel;cf-tunnel-managed"), cloudflaredInstalled(), daemonIs("inactive"),
				[]call{
					{line: "pvesh get /cluster/status --output-format json", out: clusterStatus(testAddr)},
					{line: "systemctl enable --now pco-web.service"},
					{line: "pvesh get /cluster/firewall/options --output-format json", out: `{"policy_in":"DROP"}`},
				},
				serviceStarted())

			require.NoError(t, e.setup(o))
			e.done()

			require.Equal(t, "PCO_WEB_LISTEN=192.0.2.10:8643\n", e.read(e.s.host.webEnv))
			require.Equal(t, mode, e.manifest().WebCert)
			_, err := webcert.ParseCert([]byte(e.read(e.webFile(webcert.CertName))))
			require.NoError(t, err)
		})
	}
}

func TestOptionsOfTheWebInterface(t *testing.T) {
	for _, tt := range []struct {
		name string
		o    Options
		err  string
	}{
		{"an unknown mode", Options{WebCert: "acme"}, `--web-cert "acme": want ca, own or pveproxy`},
		{"own without files", Options{WebCert: "own"},
			"--web-cert own takes the certificate and its key from --web-cert-file and --web-key-file"},
		{"a certificate without its key", Options{WebCert: "own", WebCertFile: "a.crt"}, "--web-cert-file and --web-key-file go together"},
		{"files for another mode", Options{WebCertFile: "a.crt", WebKeyFile: "a.key"}, "--web-cert-file and --web-key-file go with --web-cert own"},
		{"no web and a mode", Options{NoWeb: true, WebCert: "ca"}, "--no-web goes with none of"},
		{"no web and an address", Options{NoWeb: true, WebListen: "192.0.2.1"}, "--no-web goes with none of"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)

			require.ErrorContains(t, e.setup(tt.o), tt.err)
		})
	}

	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	e.onHost(newFakeHost(t))
	require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode, WebListen: "pve1:8643"}), `--web-listen "pve1:8643": want an address`)
}

func TestUninstallRemovesWhatSetupMadeForTheWebInterface(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(full()))
	pveSSL := e.read(filepath.Join(e.s.host.nodeCertDir, "pve-ssl.pem"))
	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}))

	require.Contains(t, h.ran, "systemctl disable --now pco-web.service")
	require.False(t, h.enabled[webUnit])
	require.NoFileExists(t, e.s.host.webEnv)
	require.NoDirExists(t, filepath.Dir(e.s.host.webDir))
	require.Equal(t, pveSSL, e.read(filepath.Join(e.s.host.nodeCertDir, "pve-ssl.pem")), "what the pin led to stays")
	require.FileExists(t, e.s.host.clusterCAKey)
	e.requireShown("  the web interface: pco-web.service is stopped and disabled")
	e.requireShown("  the certificate of the web interface: " + filepath.Dir(e.s.host.webDir))
	e.requireNothingLeft(h)
}

func TestUninstallKeepsWhatTheAdminPutBesideTheWebCertificate(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	o := full()
	o.WebCert = webcert.ModePVEProxy
	require.NoError(t, e.setup(o))
	e.writeFile(e.webFile("notes.txt"), []byte("mine\n"), 0o600)

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}))

	require.FileExists(t, e.webFile("notes.txt"))
	for _, name := range []string{webcert.CertName, webcert.KeyName, webcert.PinName} {
		_, err := os.Lstat(e.webFile(name))
		require.ErrorIs(t, err, os.ErrNotExist, name)
	}
	require.FileExists(t, filepath.Join(e.s.host.nodeCertDir, "pve-ssl.key"), "the links go, pveproxy's files stay")
	e.requireShown(e.s.host.webDir + " is kept: it holds files setup did not make")
}

func TestUninstallTouchesNothingOutsideTheWebDirectory(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(full()))
	m := e.manifest()
	m.WebTLS = append(m.WebTLS, e.s.host.clusterCAKey)
	require.NoError(t, writeManifest(filepath.Join(e.paths.Local, manifestName), m))

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}))

	require.FileExists(t, e.s.host.clusterCAKey)
	e.requireShown("the manifest lists " + e.s.host.clusterCAKey + ", which is not below")
}

// The manifest is a file on the node: a path that goes up and down again
// has to be judged by where it leads.
func TestUninstallTouchesNothingBeyondADotDotOfTheWebDirectory(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(full()))
	outside := filepath.Join(filepath.Dir(filepath.Dir(e.s.host.webDir)), "keep.txt")
	e.writeFile(outside, []byte("mine\n"), 0o600)
	m := e.manifest()
	m.WebTLS = append(m.WebTLS, e.s.host.webDir+"/../../keep.txt")
	require.NoError(t, writeManifest(filepath.Join(e.paths.Local, manifestName), m))

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}))

	require.FileExists(t, outside)
	e.requireShown("the manifest lists " + e.s.host.webDir + "/../../keep.txt, which is not below")
}

func TestTheCommandsOfTheWebCertificate(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	ca := e.webNode()
	h := newFakeHost(t)
	e.onHost(h)
	ctx := context.Background()
	_, err := e.s.WebCert()
	require.ErrorIs(t, err, errNoWeb)
	require.NoError(t, e.setup(full()))
	first := e.webLeaf()

	st, err := e.s.WebCert()
	require.NoError(t, err)
	require.Equal(t, webcert.ModeCA, st.Mode)
	require.Equal(t, first.Raw, st.Leaf.Raw)

	h.ran = nil
	st, err = e.s.RenewWebCert(ctx)
	require.NoError(t, err)
	require.NotEqual(t, first.SerialNumber, st.Leaf.SerialNumber, "a new leaf")
	requireTrusted(t, st.Leaf, ca, "pve1.example.com")
	require.Equal(t, []string{"pvesh get /cluster/status --output-format json", "systemctl try-restart pco-web.service"}, h.ran)

	crt, key := e.ownPair("pve1.example.com")
	h.ran = nil
	st, err = e.s.ImportWebCert(ctx, crt, key)
	require.NoError(t, err)
	require.Equal(t, webcert.ModeOwn, st.Mode)
	require.Equal(t, e.read(crt), e.read(e.webFile(webcert.CertName)))
	require.Contains(t, h.ran, "systemctl try-restart pco-web.service")
	require.Equal(t, webcert.ModeOwn, e.manifest().WebCert)

	_, err = e.s.RenewWebCert(ctx)
	require.ErrorContains(t, err, "the admin's own (mode own), which pco does not renew")

	_, otherKey := e.ownPair("pve1.example.com")
	_, err = e.s.ImportWebCert(ctx, crt, otherKey)
	require.ErrorContains(t, err, "the key is not the key of the certificate")
	require.Equal(t, e.read(key), e.read(e.webFile(webcert.KeyName)), "a refused pair changes nothing")
}
