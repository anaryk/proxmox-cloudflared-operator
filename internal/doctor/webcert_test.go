package doctor

import (
	"crypto/rand"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

func pair(t *testing.T, life time.Duration) (certPEM, keyPEM []byte) {
	t.Helper()
	certPEM, keyPEM, err := webcert.SelfSigned(webcert.Names{DNS: []string{"pve1.example.lan"}}, now.Add(-time.Hour), life, rand.Reader)
	require.NoError(t, err)
	return certPEM, keyPEM
}

func TestTheWebCertificate(t *testing.T) {
	good, goodKey := pair(t, 90*24*time.Hour)
	leaf, err := webcert.ParseCert(good)
	require.NoError(t, err)
	fp := webcert.Fingerprint(leaf)
	until := leaf.NotAfter.Format(time.RFC3339)
	soon, soonKey := pair(t, 20*24*time.Hour)
	soonLeaf, err := webcert.ParseCert(soon)
	require.NoError(t, err)
	gone, goneKey := pair(t, 30*time.Minute)
	goneLeaf, err := webcert.ParseCert(gone)
	require.NoError(t, err)
	_, otherKey := pair(t, 90*24*time.Hour)

	for _, tt := range []struct {
		name string
		web  WebCert
		want Finding
	}{
		{"a leaf of the cluster CA", WebCert{Mode: webcert.ModeCA, Cert: good, Key: goodKey},
			Finding{Level: LevelOK, Detail: "mode ca, valid until " + until + ", SHA-256 fingerprint " + fp}},
		{"the admin's own", WebCert{Mode: webcert.ModeOwn, Cert: good, Key: goodKey},
			Finding{Level: LevelOK, Detail: "mode own, valid until " + until + ", SHA-256 fingerprint " + fp}},
		{"the admin's own, expiring", WebCert{Mode: webcert.ModeOwn, Cert: soon, Key: soonKey},
			Finding{Level: LevelWarn, Detail: "mode own, valid until " + soonLeaf.NotAfter.Format(time.RFC3339) + ", SHA-256 fingerprint " +
				webcert.Fingerprint(soonLeaf) + "; it expires in 19 days", Fix: "pco web cert import <crt> <key>"}},
		{"a leaf of the cluster CA that was not renewed", WebCert{Mode: webcert.ModeCA, Cert: soon, Key: soonKey},
			Finding{Level: LevelWarn, Detail: "mode ca, valid until " + soonLeaf.NotAfter.Format(time.RFC3339) + ", SHA-256 fingerprint " +
				webcert.Fingerprint(soonLeaf) + "; it expires in 19 days, and pco renews it 30 days before",
				Fix: "journalctl -u pco says why it was not renewed; pco web cert renew"}},
		{"pveproxy's, expiring", WebCert{Mode: webcert.ModePVEProxy, Cert: soon, Key: soonKey},
			Finding{Level: LevelOK, Detail: "mode pveproxy, valid until " + soonLeaf.NotAfter.Format(time.RFC3339) + ", SHA-256 fingerprint " +
				webcert.Fingerprint(soonLeaf)}},
		{"expired", WebCert{Mode: webcert.ModeOwn, Cert: gone, Key: goneKey},
			Finding{Level: LevelFail, Detail: "mode own: the certificate expired at " + goneLeaf.NotAfter.Format(time.RFC3339),
				Fix: "pco web cert import <crt> <key>"}},
		{"a key of another certificate", WebCert{Mode: webcert.ModeCA, Cert: good, Key: otherKey},
			Finding{Level: LevelFail, Detail: "mode ca: tls.key does not match tls.crt", Fix: "pco web cert renew"}},
		{"unreadable", WebCert{Mode: webcert.ModePVEProxy, Err: errors.New("open /etc/pco/web/tls.key: no such file or directory")},
			Finding{Level: LevelFail, Detail: "mode pveproxy: the certificate cannot be read: open /etc/pco/web/tls.key: no such file or directory",
				Fix: "pco setup --repair"}},
		{"not a certificate", WebCert{Mode: webcert.ModeCA, Cert: goodKey, Key: goodKey},
			Finding{Level: LevelFail, Detail: "mode ca: tls.crt: no certificate in the PEM data", Fix: "pco web cert renew"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyEnv()
			env.web = &tt.web

			findings := Run(t.Context(), healthyState(), env)

			i := slices.IndexFunc(findings, func(f Finding) bool { return f.Check == "web certificate" })
			require.GreaterOrEqual(t, i, 0)
			tt.want.Check = "web certificate"
			require.Equal(t, tt.want, findings[i])
		})
	}
}

func TestNoWebCertificateWithoutTheWebInterface(t *testing.T) {
	findings := Run(t.Context(), healthyState(), healthyEnv())

	require.False(t, slices.ContainsFunc(findings, func(f Finding) bool { return f.Check == "web certificate" }))
}

func TestTheSelfSignedCertificateOfTheAppliance(t *testing.T) {
	soon, soonKey := pair(t, 20*24*time.Hour)
	leaf, err := webcert.ParseCert(soon)
	require.NoError(t, err)
	got := checkWebCert(WebCert{Mode: webcert.ModeSelfSigned, Cert: soon, Key: soonKey, Appliance: true}, now)

	require.Equal(t, Finding{Check: "web certificate", Level: LevelWarn,
		Detail: "mode self-signed, valid until " + leaf.NotAfter.Format(time.RFC3339) + ", SHA-256 fingerprint " +
			webcert.Fingerprint(leaf) + "; it expires in 19 days, and pco renews it 30 days before",
		Fix: "journalctl -u pco says why it was not renewed; pco web cert renew"}, got)
}

func TestWhereTheAppliancesWebInterfaceListens(t *testing.T) {
	net0 := netip.MustParseAddr("10.92.0.150")
	fix := "set PCO_WEB_LISTEN=<net0's address>:8643 in /etc/default/pco-web, then systemctl restart pco-web"
	for _, tt := range []struct {
		name string
		web  WebCert
		want Finding
	}{
		{"net0's address", WebCert{Listen: "10.92.0.150:8643", Net0: net0},
			Finding{Level: LevelOK, Detail: "pco-web listens on 10.92.0.150:8643, net0's address"}},
		{"every address", WebCert{Listen: "0.0.0.0:8643", Net0: net0},
			Finding{Level: LevelWarn, Detail: "pco-web listens on 0.0.0.0:8643, which is not net0's (10.92.0.150): guests on a leg may reach the sign-in page", Fix: fix}},
		{"a leg's address", WebCert{Listen: "10.92.1.1:8643", Net0: net0},
			Finding{Level: LevelWarn, Detail: "pco-web listens on 10.92.1.1:8643, which is not net0's (10.92.0.150): guests on a leg may reach the sign-in page", Fix: fix}},
		{"net0 unknown", WebCert{Listen: "10.92.0.150:8643", Net0Err: errors.New("open /etc/pco/net0: no such file or directory")},
			Finding{Level: LevelWarn, Detail: "net0's address is not known (open /etc/pco/net0: no such file or directory), so pco-web refuses to start",
				Fix: "write net0's address into /etc/pco/net0, then systemctl restart pco-web"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := healthyEnv()
			web := tt.web
			web.Appliance = true
			env.web = &web

			findings := Run(t.Context(), healthyState(), env)

			i := slices.IndexFunc(findings, func(f Finding) bool { return f.Check == "web listen" })
			require.GreaterOrEqual(t, i, 0)
			tt.want.Check = "web listen"
			require.Equal(t, tt.want, findings[i])
		})
	}
	findings := Run(t.Context(), healthyState(), healthyEnv())
	require.False(t, slices.ContainsFunc(findings, func(f Finding) bool { return f.Check == "web listen" }), "a node has no such check")
}
