package daemon

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// webRig is a node as setup leaves it for the web interface, with pco-web
// running on what it loaded at its start.
type webRig struct {
	t                  *testing.T
	k                  *webKeeper
	web                setup.WebSetup
	dir, local, loaded string
	env, caFile, caKey string
	now                time.Time
	addrs              []netip.Addr
	restarts           int
	notes              []string
	logs               bytes.Buffer
	running            bool
	restartErr         error
}

func newWebRig(t *testing.T, mode string) *webRig {
	t.Helper()
	base := t.TempDir()
	r := &webRig{
		t: t, web: setup.WebSetup{Enabled: true, Mode: mode}, now: t0, running: true,
		dir: filepath.Join(base, "web"), local: filepath.Join(base, "local"), loaded: filepath.Join(base, "credentials"),
		env: filepath.Join(base, "pco-web"), caFile: filepath.Join(base, "pve-root-ca.pem"), caKey: filepath.Join(base, "pve-root-ca.key"),
		addrs: []netip.Addr{netip.MustParseAddr("192.0.2.10")},
	}
	for _, dir := range []string{r.dir, r.local} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	r.newCA()
	r.pveproxyCert("pve-ssl")
	r.writeEnv("PCO_WEB_LISTEN=192.0.2.10:8643\n")
	r.k = &webKeeper{
		setup:   func() (setup.WebSetup, error) { return r.web, nil },
		node:    "pve1",
		fqdn:    func() string { return "pve1.example.lan" },
		addrs:   func(context.Context) ([]netip.Addr, error) { return r.addrs, nil },
		dir:     r.dir,
		env:     r.env,
		ca:      r.caFile,
		caKey:   r.caKey,
		local:   r.local,
		loaded:  r.loaded,
		restart: r.restart,
		note:    func(msg string) { r.notes = append(r.notes, msg) },
		now:     func() time.Time { return r.now },
		rand:    rand.Reader,
		log:     zerolog.New(&r.logs),
	}

	pin := filepath.Join(r.local, "pve-ssl.pem")
	require.NoError(t, webcert.LinkAtomic(filepath.Join(r.dir, webcert.PinName), pin))
	switch mode {
	case webcert.ModeCA:
		names := webcert.NodeNames("pve1", "pve1.example.lan", r.addrs, "192.0.2.10:8643", nil)
		ca, key := r.readCA()
		certPEM, keyPEM, err := webcert.Issue(ca, key, names, t0, webcert.Lifetime, rand.Reader)
		require.NoError(t, err)
		require.NoError(t, webcert.WriteAtomic(r.dir, certPEM, keyPEM))
	case webcert.ModeOwn:
		certPEM, keyPEM, err := webcert.SelfSigned(webcert.Names{DNS: []string{"pve1.example.lan"}}, t0, 40*24*time.Hour, rand.Reader)
		require.NoError(t, err)
		require.NoError(t, webcert.WriteAtomic(r.dir, certPEM, keyPEM))
	case webcert.ModePVEProxy:
		require.NoError(t, webcert.LinkAtomic(filepath.Join(r.dir, webcert.CertName), pin))
		require.NoError(t, webcert.LinkAtomic(filepath.Join(r.dir, webcert.KeyName), filepath.Join(r.local, "pve-ssl.key")))
	}
	require.NoError(t, r.restart(context.Background()))
	r.restarts = 0
	return r
}

// restart is pco-web started again: systemd copies its credentials anew.
func (r *webRig) restart(context.Context) error {
	if r.restartErr != nil {
		return r.restartErr
	}
	r.restarts++
	if !r.running {
		return nil
	}
	require.NoError(r.t, os.MkdirAll(r.loaded, 0o700))
	for _, name := range []string{webcert.CertName, webcert.KeyName, webcert.PinName} {
		b, err := os.ReadFile(filepath.Join(r.dir, name))
		require.NoError(r.t, err)
		require.NoError(r.t, os.WriteFile(filepath.Join(r.loaded, name), b, 0o600))
	}
	return nil
}

func (r *webRig) newCA() {
	r.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(r.t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Proxmox Virtual Environment"},
		NotBefore: t0.AddDate(-1, 0, 0), NotAfter: t0.AddDate(9, 0, 0),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	require.NoError(r.t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(r.t, err)
	require.NoError(r.t, os.WriteFile(r.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o640))
	require.NoError(r.t, os.WriteFile(r.caKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
}

func (r *webRig) readCA() (*x509.Certificate, *ecdsa.PrivateKey) {
	r.t.Helper()
	b, err := os.ReadFile(r.caFile)
	require.NoError(r.t, err)
	ca, err := webcert.ParseCert(b)
	require.NoError(r.t, err)
	b, err = os.ReadFile(r.caKey)
	require.NoError(r.t, err)
	key, err := webcert.ParseKey(b)
	require.NoError(r.t, err)
	return ca, key.(*ecdsa.PrivateKey)
}

func (r *webRig) pveproxyCert(name string) {
	r.t.Helper()
	certPEM, keyPEM, err := webcert.SelfSigned(webcert.Names{DNS: []string{"pve1"}}, r.now, 365*24*time.Hour, rand.Reader)
	require.NoError(r.t, err)
	require.NoError(r.t, os.WriteFile(filepath.Join(r.local, name+".pem"), certPEM, 0o640))
	require.NoError(r.t, os.WriteFile(filepath.Join(r.local, name+".key"), keyPEM, 0o640))
}

func (r *webRig) writeEnv(content string) {
	r.t.Helper()
	require.NoError(r.t, os.WriteFile(r.env, []byte(content), 0o644))
}

func (r *webRig) check() { r.k.check(context.Background()) }

func (r *webRig) leaf() *x509.Certificate {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, webcert.CertName))
	require.NoError(r.t, err)
	leaf, err := webcert.ParseCert(b)
	require.NoError(r.t, err)
	return leaf
}

func (r *webRig) linkOf(name string) string {
	r.t.Helper()
	target, err := os.Readlink(filepath.Join(r.dir, name))
	require.NoError(r.t, err)
	return target
}

func TestTheWebKeeperChangesNothingWhileNothingChanges(t *testing.T) {
	for _, mode := range []string{webcert.ModeCA, webcert.ModeOwn, webcert.ModePVEProxy} {
		t.Run(mode, func(t *testing.T) {
			r := newWebRig(t, mode)
			before, err := os.ReadFile(filepath.Join(r.dir, webcert.CertName))
			require.NoError(t, err)

			for range 3 {
				r.check()
				r.now = r.now.Add(time.Minute)
			}

			after, err := os.ReadFile(filepath.Join(r.dir, webcert.CertName))
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Zero(t, r.restarts)
			require.Empty(t, r.notes)
			require.Empty(t, r.logs.String())
		})
	}
}

func TestTheWebKeeperFollowsACertificateInstalledForPveproxy(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	r.pveproxyCert("pveproxy-ssl")

	r.check()
	r.check()

	require.Equal(t, filepath.Join(r.local, "pveproxy-ssl.pem"), r.linkOf(webcert.PinName))
	require.Equal(t, 1, r.restarts, "one restart for one change")
}

func TestTheWebKeeperRestartsWhenPveproxyRenewsItsCertificateInPlace(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	r.now = t0.Add(24 * time.Hour)
	r.pveproxyCert("pve-ssl")

	r.check()
	r.check()

	require.Equal(t, filepath.Join(r.local, "pve-ssl.pem"), r.linkOf(webcert.PinName), "the same file")
	require.Equal(t, 1, r.restarts)
}

func TestTheWebKeeperRenewsADueLeafOnce(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	old := r.leaf()
	r.now = t0.Add(61 * 24 * time.Hour)

	r.check()
	r.check()

	leaf := r.leaf()
	require.NotEqual(t, old.SerialNumber, leaf.SerialNumber)
	require.Equal(t, r.now.Add(webcert.Lifetime), leaf.NotAfter)
	require.Equal(t, []string{"web certificate renewed: it expires at " + old.NotAfter.Format(time.RFC3339) +
		", in less than 30 days, fingerprint " + webcert.Fingerprint(leaf)}, r.notes)
	require.Equal(t, 1, r.restarts)
	ca, _ := r.readCA()
	require.NoError(t, leaf.CheckSignatureFrom(ca))
}

func TestTheWebKeeperRenewsWhenTheNamesChange(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(r *webRig)
		why    string
		covers string
	}{
		{"a new listen address", func(r *webRig) { r.writeEnv("PCO_WEB_LISTEN=198.51.100.4:8643\n") },
			"it does not name 198.51.100.4", "198.51.100.4"},
		{"a new address of the node", func(r *webRig) { r.addrs = []netip.Addr{netip.MustParseAddr("192.0.2.11")} },
			"it does not name 192.0.2.11", "192.0.2.11"},
		{"another host name", func(r *webRig) { r.writeEnv("PCO_WEB_LISTEN=192.0.2.10:8643\nPCO_WEB_HOSTS=pve.example.org\n") },
			"it does not name pve.example.org", "pve.example.org"},
		{"another cluster CA", func(r *webRig) { r.newCA() }, "it is not signed by the cluster CA", "pve1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newWebRig(t, webcert.ModeCA)
			tt.change(r)

			r.check()
			r.check()

			require.Len(t, r.notes, 1)
			require.True(t, strings.HasPrefix(r.notes[0], "web certificate renewed: "+tt.why+", fingerprint "), r.notes[0])
			require.NoError(t, r.leaf().VerifyHostname(tt.covers))
			require.Equal(t, 1, r.restarts)
		})
	}
}

func TestTheWebKeeperRepointsThePairOfPveproxy(t *testing.T) {
	r := newWebRig(t, webcert.ModePVEProxy)
	r.pveproxyCert("pveproxy-ssl")

	r.check()
	r.check()

	for name, target := range map[string]string{
		webcert.CertName: "pveproxy-ssl.pem", webcert.KeyName: "pveproxy-ssl.key", webcert.PinName: "pveproxy-ssl.pem",
	} {
		require.Equal(t, filepath.Join(r.local, target), r.linkOf(name), name)
	}
	require.Equal(t, 1, r.restarts)
	require.Empty(t, r.notes)
}

func TestTheWebKeeperLeavesTheAdminsOwnCertificate(t *testing.T) {
	r := newWebRig(t, webcert.ModeOwn)
	before := r.leaf()
	r.now = t0.Add(39 * 24 * time.Hour)

	r.check()

	require.Equal(t, before.Raw, r.leaf().Raw, "pco does not renew it")
	require.Zero(t, r.restarts)
}

func TestTheWebKeeperDoesNothingWithoutTheWebInterface(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	r.web = setup.WebSetup{}
	r.pveproxyCert("pveproxy-ssl")
	r.now = t0.Add(80 * 24 * time.Hour)

	r.check()

	require.Equal(t, filepath.Join(r.local, "pve-ssl.pem"), r.linkOf(webcert.PinName))
	require.Zero(t, r.restarts)
	require.Empty(t, r.notes)
}

func TestTheWebKeeperRestartsNoWebInterfaceThatDoesNotRun(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	require.NoError(t, os.RemoveAll(r.loaded))
	r.running = false
	r.pveproxyCert("pveproxy-ssl")

	r.check()

	require.Equal(t, filepath.Join(r.local, "pveproxy-ssl.pem"), r.linkOf(webcert.PinName), "the link follows all the same")
	require.Zero(t, r.restarts)
}

// A restart that fails is tried again at the next check, which still finds
// pco-web on what it loaded before.
func TestTheWebKeeperTriesAFailedRestartAgain(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	r.pveproxyCert("pveproxy-ssl")
	r.restartErr = os.ErrPermission

	r.check()
	r.restartErr = nil
	r.check()
	r.check()

	require.Equal(t, 1, r.restarts)
	require.Equal(t, 1, strings.Count(r.logs.String(), "restarting pco-web.service failed"))
}

func TestTheWebKeeperThatCannotRenewSaysSoOnce(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)
	old := r.leaf()
	require.NoError(t, os.Remove(r.caKey))
	r.now = t0.Add(61 * 24 * time.Hour)

	for range 3 {
		r.check()
	}

	require.Equal(t, old.Raw, r.leaf().Raw)
	require.Zero(t, r.restarts)
	require.Empty(t, r.notes)
	require.Equal(t, 1, strings.Count(r.logs.String(), "renewing the certificate of the web interface failed"), r.logs.String())
}

func TestTheWebKeeperGivesTheDoctorTheCertificate(t *testing.T) {
	r := newWebRig(t, webcert.ModeCA)

	w, enabled := r.k.facts(context.Background())

	require.True(t, enabled)
	require.Equal(t, webcert.ModeCA, w.Mode)
	require.NoError(t, w.Err)
	cert, err := os.ReadFile(filepath.Join(r.dir, webcert.CertName))
	require.NoError(t, err)
	require.Equal(t, cert, w.Cert)
	require.NotEmpty(t, w.Key)

	r.web = setup.WebSetup{}
	_, enabled = r.k.facts(context.Background())
	require.False(t, enabled)

	r.web = setup.WebSetup{Enabled: true, Mode: webcert.ModeCA}
	require.NoError(t, os.Remove(filepath.Join(r.dir, webcert.KeyName)))
	w, enabled = r.k.facts(context.Background())
	require.True(t, enabled)
	require.Error(t, w.Err)
	require.IsType(t, doctor.WebCert{}, w)
}
