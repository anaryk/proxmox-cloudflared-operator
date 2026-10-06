package daemon

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// applianceWebRig is the container as pco appliance install leaves it for
// the web interface: net0's address, the environment of the unit, and the CA
// of the node's API it pushed.
type applianceWebRig struct {
	t                *testing.T
	k                *applianceWebKeeper
	install          store.ApplianceInstall
	dir, loaded, env string
	net0, caFile     string
	now              time.Time
	restarts         int
	restartErr       error
	notes, listens   []string
	logs             bytes.Buffer
	running          bool
	eth0             *fakeEth0
}

func newApplianceWebRig(t *testing.T) *applianceWebRig {
	t.Helper()
	base := t.TempDir()
	r := &applianceWebRig{
		t: t, now: t0,
		dir: filepath.Join(base, "web"), loaded: filepath.Join(base, "credentials"),
		env: filepath.Join(base, "pco-web"), net0: filepath.Join(base, "net0"), caFile: filepath.Join(base, "pve-ca.pem"),
		eth0: &fakeEth0{addrs: []netip.Addr{netip.MustParseAddr("10.92.0.150")}},
	}
	require.NoError(t, os.WriteFile(r.net0, []byte("10.92.0.150\n"), 0o644))
	require.NoError(t, os.WriteFile(r.env, []byte("PCO_WEB_LISTEN=10.92.0.150:8643\n"), 0o644))
	ca, _, err := webcert.SelfSigned(webcert.Names{DNS: []string{"pve1"}}, t0, 10*365*24*time.Hour, rand.Reader)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(r.caFile, ca, 0o644))
	r.install = store.ApplianceInstall{
		VMID: 9250, Node: "pve1", MACs: []string{applianceMAC},
		Endpoints: []store.Endpoint{{Address: "192.0.2.10:8006", ServerName: "pve1"}}, CAFile: r.caFile,
	}
	r.k = newApplianceWebKeeper(
		Deps{WebDir: r.dir, WebEnv: r.env, WebLoaded: r.loaded, RestartWeb: r.restart, Now: func() time.Time { return r.now }},
		ApplianceDeps{Hostname: func() (string, error) { return "pco", nil }, Net0: r.net0, Net0Addrs: r.eth0.Addrs, WatchAddrs: r.eth0.Watch},
		r.install, func(msg string) { r.notes = append(r.notes, msg) }, func(msg string) { r.listens = append(r.listens, msg) },
		zerolog.New(&r.logs),
	)
	r.k.fqdn = func() string { return "" }
	return r
}

// restart is pco-web started again: systemd copies its credentials anew.
func (r *applianceWebRig) restart(context.Context) error {
	if r.restartErr != nil {
		return r.restartErr
	}
	r.restarts++
	if !r.running {
		return nil
	}
	r.load()
	return nil
}

// load is pco-web starting with the files as they are.
func (r *applianceWebRig) load() {
	r.t.Helper()
	r.running = true
	require.NoError(r.t, os.MkdirAll(r.loaded, 0o700))
	for _, name := range []string{webcert.CertName, webcert.KeyName, webcert.APIName} {
		b, err := os.ReadFile(filepath.Join(r.dir, name))
		require.NoError(r.t, err)
		require.NoError(r.t, os.WriteFile(filepath.Join(r.loaded, name), b, 0o600))
	}
}

func (r *applianceWebRig) leaf() *x509.Certificate {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, webcert.CertName))
	require.NoError(r.t, err)
	leaf, err := webcert.ParseCert(b)
	require.NoError(r.t, err)
	return leaf
}

func TestTheApplianceMakesItsCertificateAtItsFirstStart(t *testing.T) {
	r := newApplianceWebRig(t)

	r.k.check(context.Background())

	leaf := r.leaf()
	require.NoError(t, leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature), "self-signed")
	require.Equal(t, leaf.RawSubject, leaf.RawIssuer)
	require.False(t, leaf.IsCA)
	require.Equal(t, []string{"pco"}, leaf.DNSNames)
	require.Len(t, leaf.IPAddresses, 1)
	require.True(t, leaf.IPAddresses[0].Equal(net.ParseIP("10.92.0.150")))
	require.Equal(t, t0.Add(397*24*time.Hour), leaf.NotAfter)
	require.IsType(t, &ecdsa.PublicKey{}, leaf.PublicKey)
	require.Equal(t, elliptic.P256(), leaf.PublicKey.(*ecdsa.PublicKey).Curve)
	certPEM, keyPEM, err := webcert.ReadPair(r.dir)
	require.NoError(t, err)
	key, err := webcert.ParseKey(keyPEM)
	require.NoError(t, err)
	require.True(t, webcert.Matches(leaf, key))
	info, err := os.Stat(filepath.Join(r.dir, webcert.KeyName))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	mode, err := webcert.ReadMode(r.dir)
	require.NoError(t, err)
	require.Equal(t, webcert.ModeSelfSigned, mode)
	require.Equal(t, []string{"web certificate made for pco, 10.92.0.150, fingerprint " + webcert.Fingerprint(leaf)}, r.notes)

	api, err := webcert.ReadAPI(filepath.Join(r.dir, webcert.APIName))
	require.NoError(t, err)
	ca, err := os.ReadFile(r.caFile)
	require.NoError(t, err)
	require.Equal(t, webcert.API{URL: "https://192.0.2.10:8006/api2/json", ServerName: "pve1", CA: string(ca), Node: "pve1"}, api)
	require.Zero(t, r.restarts, "pco-web does not run yet: it reads them as it starts")

	r.load()
	r.k.check(context.Background())
	require.Zero(t, r.restarts)
	require.Len(t, r.notes, 1)
	after, _, err := webcert.ReadPair(r.dir)
	require.NoError(t, err)
	require.Equal(t, certPEM, after, "made once")
}

func TestTheApplianceRenewsItsCertificate30DaysBeforeItEnds(t *testing.T) {
	r := newApplianceWebRig(t)
	r.k.check(context.Background())
	r.load()
	old := r.leaf()

	r.now = old.NotAfter.Add(-31 * 24 * time.Hour)
	r.k.check(context.Background())
	require.Len(t, r.notes, 1)
	require.Zero(t, r.restarts)

	r.now = old.NotAfter.Add(-29 * 24 * time.Hour)
	r.k.check(context.Background())

	renewed := r.leaf()
	require.NotEqual(t, webcert.Fingerprint(old), webcert.Fingerprint(renewed))
	require.Equal(t, r.now.Add(397*24*time.Hour), renewed.NotAfter)
	require.Equal(t, "web certificate renewed: it expires at "+old.NotAfter.UTC().Format(time.RFC3339)+
		", in less than 30 days, fingerprint "+webcert.Fingerprint(renewed), r.notes[1])
	require.Equal(t, 1, r.restarts, "pco-web serves the new one")
}

func TestTheApplianceLeavesTheAdminsCertificateAlone(t *testing.T) {
	r := newApplianceWebRig(t)
	names := webcert.Names{DNS: []string{"pco.example.org"}}
	names.Addrs = append(names.Addrs, netip.MustParseAddr("10.92.0.150"))
	certPEM, keyPEM, err := webcert.SelfSigned(names, t0, 10*24*time.Hour, rand.Reader)
	require.NoError(t, err)
	_, err = webcert.ImportOwn(r.dir, certPEM, keyPEM, names, t0)
	require.NoError(t, err)

	r.k.check(context.Background())

	have, _, err := webcert.ReadPair(r.dir)
	require.NoError(t, err)
	require.Equal(t, certPEM, have, "due by its date, and still the admin's")
	require.Empty(t, r.notes)
}

func TestAnotherEndpointRestartsPcoWeb(t *testing.T) {
	r := newApplianceWebRig(t)
	r.k.check(context.Background())
	r.load()

	r.k.install.Endpoints[0] = store.Endpoint{Address: "192.0.2.11:8006", ServerName: "pve1.example.org"}
	r.k.check(context.Background())

	api, err := webcert.ReadAPI(filepath.Join(r.loaded, webcert.APIName))
	require.NoError(t, err)
	require.Equal(t, "https://192.0.2.11:8006/api2/json", api.URL)
	require.Equal(t, "pve1.example.org", api.ServerName)
	require.Equal(t, 1, r.restarts)
}

func TestTheFactsOfTheAppliancesWebInterface(t *testing.T) {
	r := newApplianceWebRig(t)
	r.k.check(context.Background())

	w, enabled := r.k.facts(context.Background())

	require.True(t, enabled)
	require.True(t, w.Appliance)
	require.Equal(t, webcert.ModeSelfSigned, w.Mode)
	require.NoError(t, w.Err)
	require.Equal(t, "10.92.0.150:8643", w.Listen)
	require.Equal(t, netip.MustParseAddr("10.92.0.150"), w.Net0)

	require.Equal(t, []netip.Addr{netip.MustParseAddr("10.92.0.150")}, w.Live)
	require.Equal(t, 9250, w.VMID)

	require.NoError(t, os.WriteFile(r.env, nil, 0o644))
	w, _ = r.k.facts(context.Background())
	require.Equal(t, "10.92.0.150:8643", w.Listen, "without PCO_WEB_LISTEN, net0's address")
}

func TestTheApplianceFollowsANewAddressOfNet0(t *testing.T) {
	r := newApplianceWebRig(t)
	r.k.check(context.Background())
	r.load()
	before, _, err := webcert.ReadPair(r.dir)
	require.NoError(t, err)

	r.eth0.set(netip.MustParseAddr("10.92.0.160"))
	r.k.check(context.Background())

	have, err := webcert.ReadNet0(r.net0)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.92.0.160"), have)
	require.Equal(t, 1, r.restarts, "pco-web listens on the new address")
	require.Equal(t, []string{"net0's address changed from 10.92.0.150 to 10.92.0.160: pco-web listens on 10.92.0.160:8643 now"}, r.listens)
	after, _, err := webcert.ReadPair(r.dir)
	require.NoError(t, err)
	require.Equal(t, before, after, "the certificate, and the fingerprint the browser trusts, stay")

	r.k.check(context.Background())
	require.Equal(t, 1, r.restarts, "once")
	r.eth0.set(netip.MustParseAddr("10.92.0.170"), netip.MustParseAddr("10.92.0.160"))
	r.k.check(context.Background())
	require.Equal(t, 1, r.restarts, "an address that is still the card's stays")
}

func TestNet0WithoutAnAddressIsLeftAsItIs(t *testing.T) {
	r := newApplianceWebRig(t)
	r.eth0.set()

	r.k.check(context.Background())

	have, err := webcert.ReadNet0(r.net0)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.92.0.150"), have)
	require.Empty(t, r.listens)
}

func TestAFailedRestartForNet0IsTriedAgain(t *testing.T) {
	r := newApplianceWebRig(t)
	r.k.check(context.Background())
	r.load()
	r.restartErr = errors.New("systemctl: no answer")
	r.eth0.set(netip.MustParseAddr("10.92.0.160"))

	r.k.check(context.Background())
	require.Zero(t, r.restarts)
	require.Contains(t, r.logs.String(), "restarting pco-web.service on net0's new address failed")

	r.restartErr = nil
	r.k.check(context.Background())
	require.Equal(t, 1, r.restarts)
	r.k.check(context.Background())
	require.Equal(t, 1, r.restarts)
}

// The daemon follows a new lease of net0 as soon as netlink says so, and its
// doctor fails while the file and the card disagree.
func TestTheDaemonOfTheApplianceKeepsNet0Current(t *testing.T) {
	a := newApplianceWorld(t)
	a.deps.UnitEnabled = func(_ context.Context, unit string) (bool, error) { return unit == webService, nil }
	a.deps.WebEvery = time.Hour
	d := a.start()
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })
	require.Eventually(t, func() bool { _, _, err := webcert.ReadPair(a.deps.WebDir); return err == nil }, 5*time.Second, 10*time.Millisecond)

	a.eth0.set(netip.MustParseAddr("10.92.0.160"))

	require.Eventually(t, func() bool {
		have, err := webcert.ReadNet0(a.deps.Appliance.Net0)
		return err == nil && have == netip.MustParseAddr("10.92.0.160")
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return a.sysd.restarts(webService) == 1 }, 5*time.Second, 10*time.Millisecond)
	events, err := d.client.Events(t.Context(), time.Time{})
	require.NoError(t, err)
	i := slices.IndexFunc(events, func(e engine.Event) bool { return e.Subject == "web listen" })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, "net0's address changed from 10.92.0.150 to 10.92.0.160: pco-web listens on 10.92.0.160:8643 now", events[i].Message)
	require.Equal(t, "warn", events[i].Level)

	findings, err := d.client.Doctor(t.Context())
	require.NoError(t, err)
	i = slices.IndexFunc(findings, func(f doctor.Finding) bool { return f.Check == "web listen" })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, doctor.Finding{Check: "web listen", Level: doctor.LevelOK, Detail: "pco-web listens on 10.92.0.160:8643, net0's address"}, findings[i])
}

// The daemon of the appliance makes the certificate as it starts, and its
// doctor shows the fingerprint and where pco-web listens.
func TestTheDoctorOfTheApplianceShowsItsWebInterface(t *testing.T) {
	a := newApplianceWorld(t)
	a.deps.UnitEnabled = func(_ context.Context, unit string) (bool, error) { return unit == webService, nil }
	d := a.start()
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })

	var certPEM []byte
	require.Eventually(t, func() bool {
		var err error
		certPEM, _, err = webcert.ReadPair(a.deps.WebDir)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	leaf, err := webcert.ParseCert(certPEM)
	require.NoError(t, err)
	findings, err := d.client.Doctor(t.Context())
	require.NoError(t, err)
	byCheck := map[string]doctor.Finding{}
	for _, f := range findings {
		byCheck[f.Check] = f
	}
	require.Equal(t, doctor.LevelOK, byCheck["web certificate"].Level, byCheck["web certificate"].Detail)
	require.Contains(t, byCheck["web certificate"].Detail, "mode self-signed")
	require.Contains(t, byCheck["web certificate"].Detail, "SHA-256 fingerprint "+webcert.Fingerprint(leaf))
	require.Equal(t, doctor.Finding{Check: "web listen", Level: doctor.LevelOK, Detail: "pco-web listens on 10.92.0.150:8643, net0's address"},
		byCheck["web listen"])
	require.Contains(t, a.logs.String(), "web certificate made for pco, 10.92.0.150")
}

func TestAnApplianceWithoutItsWebInterfaceHasNoWebChecks(t *testing.T) {
	a := newApplianceWorld(t)
	d := a.start()
	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })

	findings, err := d.client.Doctor(t.Context())

	require.NoError(t, err)
	for _, f := range findings {
		require.NotContains(t, []string{"web certificate", "web listen"}, f.Check, "pco-web.service is not enabled")
	}
}
