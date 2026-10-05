package setup

import (
	"bytes"
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// pveproxyThreat is what --web-cert pveproxy gives away.
const pveproxyThreat = "--web-cert pveproxy: pco-web holds pveproxy's own key, so whoever takes over pco-web can pose as " +
	"this node to every browser that trusts it, on any port, and on port 8006 while pveproxy is down, and collect the " +
	"Proxmox VE passwords typed there; the default, --web-cert ca, gives pco-web a key of its own"

// webPlan is what the web step works from.
type webPlan struct {
	mode   string        // of the certificate
	listen string        // PCO_WEB_LISTEN
	fqdn   string        // of the node; empty when it has none
	names  webcert.Names // what a leaf of the node is for
}

// ensureWeb sets up the web interface: the environment of its unit, the
// certificate in the mode chosen, the link to the certificate pveproxy
// serves, which pco-web pins, and the unit.
func (r *run) ensureWeb(ctx context.Context) error {
	switch {
	case r.o.NoWeb:
		r.ask.Info("web interface: skipped (--no-web)")
		return nil
	case !r.unitInstalled(webUnit):
		r.ask.Info("web interface: %s is not installed, as when pco runs from a build tree; skipped", webUnit)
		return nil
	}
	p, err := r.planWeb(ctx)
	if err != nil {
		return err
	}
	envChanged, err := r.ensureWebEnv(p.listen)
	if err != nil {
		return err
	}
	if err := r.ensureWebDir(); err != nil {
		return err
	}
	certChanged, err := r.ensureWebCert(p)
	if err != nil {
		return err
	}
	pinChanged, err := r.ensureWebPin()
	if err != nil {
		return err
	}
	wasEnabled := r.manifest.WebEnabled
	if err := r.record(func(m *Manifest) { m.WebCert = p.mode }); err != nil {
		return err
	}
	if err := r.startWeb(ctx, wasEnabled && (envChanged || certChanged || pinChanged)); err != nil {
		return err
	}
	r.showWeb(ctx, p)
	return nil
}

// planWeb reads the mode, the listen address and the names of the node: the
// mode given, or the one of before, or ca; the address given, or the one in
// the environment file, or the node's in the cluster status.
func (r *run) planWeb(ctx context.Context) (webPlan, error) {
	p := webPlan{mode: cmp.Or(r.o.WebCert, r.manifest.WebCert, webcert.ModeCA)}
	env, _, err := webcert.ReadEnv(r.host.webEnv)
	if err != nil {
		return p, fmt.Errorf("reading %s: %w", r.host.webEnv, err)
	}
	addrs, err := r.nodeAddrs(ctx)
	if err != nil {
		return p, err
	}
	switch {
	case r.o.WebListen != "":
		if p.listen, err = listenAddress(r.o.WebListen); err != nil {
			return p, err
		}
	case env.Listen != "":
		p.listen = env.Listen
	case len(addrs) > 0:
		p.listen = net.JoinHostPort(addrs[0].String(), webcert.Port)
	default:
		return p, fmt.Errorf("the cluster status lists no address of node %s: give the address pco-web listens on with --web-listen", r.o.Node)
	}
	name, err := r.host.hostname()
	if err != nil {
		return p, fmt.Errorf("reading the host name: %w", err)
	}
	p.fqdn = r.host.fqdn(name)
	p.names, err = webcert.NodeNames(r.o.Node, p.fqdn, addrs, p.listen, env.Hosts)
	return p, err
}

// listenAddress reads --web-listen: an address, to which the port of the web
// interface is added, or an address and a port.
func listenAddress(s string) (string, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		return net.JoinHostPort(a.String(), webcert.Port), nil
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil || ap.Port() == 0 {
		return "", fmt.Errorf("--web-listen %q: want an address, such as 192.0.2.10, or an address and a port, such as 192.0.2.10:%s", s, webcert.Port)
	}
	return ap.String(), nil
}

// clusterRow is an entry of the cluster status.
type clusterRow struct {
	Type  string  `json:"type"`
	Name  string  `json:"name"`
	IP    string  `json:"ip"`
	Local pveBool `json:"local"`
}

// nodeAddrs returns the address the cluster status lists for this node, read
// as the daemon reads it: IPv4 only.
func (r *run) nodeAddrs(ctx context.Context) ([]netip.Addr, error) {
	var rows []clusterRow
	if err := r.query(ctx, &rows, "pvesh", "get", "/cluster/status", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("reading the cluster status: %w", err)
	}
	var out []netip.Addr
	for _, row := range rows {
		if row.Type != "node" || !bool(row.Local) && row.Name != r.o.Node {
			continue
		}
		if a, err := netip.ParseAddr(row.IP); err == nil && a.Is4() {
			out = append(out, a)
		}
	}
	return out, nil
}

// ensureWebEnv writes PCO_WEB_LISTEN into the environment file of the unit,
// and keeps what else the admin wrote there.
func (r *run) ensureWebEnv(listen string) (bool, error) {
	path := r.host.webEnv
	have, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := r.record(func(m *Manifest) { m.WebEnv = true }); err != nil {
			return false, err
		}
		if err := atomicfile.Write(path, []byte("PCO_WEB_LISTEN="+listen+"\n"), atomicfile.Options{Mode: 0o644}); err != nil {
			r.takeBack(func() (bool, error) { return exists(path) }, func(m *Manifest) { m.WebEnv = false })
			return false, fmt.Errorf("writing %s: %w", path, err)
		}
		r.ask.Info("web interface: wrote %s, listening on %s", path, listen)
		return true, nil
	case err != nil:
		return false, fmt.Errorf("reading %s: %w", path, err)
	}
	if webcert.ParseEnv(have)["PCO_WEB_LISTEN"] == listen {
		r.ask.Info("web interface: listens on %s, nothing needed", listen)
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := atomicfile.Write(path, setEnv(have, "PCO_WEB_LISTEN", listen), atomicfile.Options{Mode: info.Mode().Perm()}); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	r.ask.Info("web interface: %s now listens on %s", path, listen)
	return true, nil
}

// setEnv sets key to value in an environment file: on the line that sets it,
// or on a line of its own at the end.
func setEnv(b []byte, key, value string) []byte {
	var out bytes.Buffer
	set := false
	for line := range strings.Lines(string(b)) {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == key {
			if !set {
				out.WriteString(key + "=" + value + "\n")
				set = true
			}
			continue
		}
		out.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			out.WriteString("\n")
		}
	}
	if !set {
		out.WriteString(key + "=" + value + "\n")
	}
	return out.Bytes()
}

// ensureWebDir makes the directory of the certificate, which only root may
// read.
func (r *run) ensureWebDir() error {
	for _, d := range []struct {
		path string
		mode fs.FileMode
	}{{filepath.Dir(r.host.webDir), 0o755}, {r.host.webDir, 0o700}} {
		there, err := exists(d.path)
		switch {
		case err != nil:
			return err
		case there:
			continue
		}
		if err := r.record(func(m *Manifest) { m.addWebTLS(d.path) }); err != nil {
			return err
		}
		if err := os.Mkdir(d.path, d.mode); err != nil {
			r.untrack(d.path)
			return fmt.Errorf("creating %s: %w", d.path, err)
		}
	}
	return nil
}

// untrack takes back the note of a path setup failed to make, when it is not
// there.
func (r *run) untrack(paths ...string) {
	r.takeBack(func() (bool, error) {
		for _, path := range paths {
			if there, err := lexists(path); err != nil || there {
				return true, err
			}
		}
		return false, nil
	}, func(m *Manifest) { m.WebTLS = without(m.WebTLS, paths) })
}

func (s *Setup) webPath(name string) string { return filepath.Join(s.host.webDir, name) }

func (r *run) ensureWebCert(p webPlan) (bool, error) {
	switch p.mode {
	case webcert.ModeOwn:
		return r.webCertOwn(p)
	case webcert.ModePVEProxy:
		return r.webCertPVEProxy()
	}
	return r.webCertCA(p)
}

// webCertCA keeps a leaf of the cluster CA that is due for nothing, and makes
// a new key and leaf otherwise.
func (r *run) webCertCA(p webPlan) (bool, error) {
	ca, err := readCert(r.host.clusterCA)
	if err != nil {
		return false, fmt.Errorf("reading the cluster CA: %w", err)
	}
	if leaf := r.currentLeaf(); leaf != nil && r.manifest.WebCert == webcert.ModeCA {
		if due, _ := webcert.Due(leaf, ca, p.names, r.now()); !due {
			r.ask.Info("web certificate: kept, signed by the cluster CA and valid until %s", dateOf(leaf.NotAfter))
			return false, nil
		}
	}
	if err := r.issueWebCA(ca, p.names); err != nil {
		return false, err
	}
	r.ask.Info("web certificate: made a key of its own and a certificate for %s, signed by the cluster CA, valid for 90 days", p.names)
	return true, nil
}

// issueWebCA makes a new key and a leaf for names, signed by the cluster CA,
// whose key is read for it and dropped.
func (r *run) issueWebCA(ca *x509.Certificate, names webcert.Names) error {
	b, err := os.ReadFile(r.host.clusterCAKey)
	if err != nil {
		return fmt.Errorf("reading the key of the cluster CA: %w", err)
	}
	caKey, err := webcert.ParseKey(b)
	if err != nil {
		return fmt.Errorf("reading the key of the cluster CA %s: %w", r.host.clusterCAKey, err)
	}
	certPEM, keyPEM, err := webcert.Issue(ca, caKey, names, r.now(), webcert.Lifetime, r.rand)
	if err != nil {
		return fmt.Errorf("making the certificate of the web interface: %w", err)
	}
	return r.writeWebPair(certPEM, keyPEM)
}

// currentLeaf returns the leaf of tls.crt when tls.crt and tls.key are files,
// not links, and the key is the leaf's; nil otherwise.
func (r *run) currentLeaf() *x509.Certificate {
	for _, name := range []string{webcert.CertName, webcert.KeyName} {
		if info, err := os.Lstat(r.webPath(name)); err != nil || !info.Mode().IsRegular() {
			return nil
		}
	}
	certPEM, keyPEM, err := webcert.ReadPair(r.host.webDir)
	if err != nil {
		return nil
	}
	leaf, err := webcert.ParseCert(certPEM)
	if err != nil {
		return nil
	}
	if key, err := webcert.ParseKey(keyPEM); err != nil || !webcert.Matches(leaf, key) {
		return nil
	}
	return leaf
}

// webCertOwn copies in the admin's certificate and key once they pass the
// checks, or keeps the ones copied in before.
func (r *run) webCertOwn(p webPlan) (bool, error) {
	if r.o.WebCertFile == "" {
		certPEM, keyPEM, err := webcert.ReadPair(r.host.webDir)
		if err != nil {
			return false, fmt.Errorf("the certificate of the web interface: %w; give one with --web-cert own "+
				"--web-cert-file <crt> --web-key-file <key>", err)
		}
		if err := webcert.Check(certPEM, keyPEM, p.names, r.now()); err != nil {
			r.ask.Warn("web certificate: the admin's own, kept, but %v; pco web cert import <crt> <key> puts another in place", err)
			return false, nil
		}
		r.ask.Info("web certificate: the admin's own, kept")
		return false, nil
	}
	certPEM, err := os.ReadFile(r.o.WebCertFile)
	if err != nil {
		return false, fmt.Errorf("reading the certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(r.o.WebKeyFile)
	if err != nil {
		return false, fmt.Errorf("reading the key: %w", err)
	}
	if err := webcert.Check(certPEM, keyPEM, p.names, r.now()); err != nil {
		return false, fmt.Errorf("%s and %s: %w", r.o.WebCertFile, r.o.WebKeyFile, err)
	}
	if haveCert, haveKey, err := webcert.ReadPair(r.host.webDir); err == nil && r.currentLeaf() != nil &&
		bytes.Equal(haveCert, certPEM) && bytes.Equal(haveKey, keyPEM) {
		r.ask.Info("web certificate: the admin's own from %s, nothing needed", r.o.WebCertFile)
		return false, nil
	}
	if err := r.writeWebPair(certPEM, keyPEM); err != nil {
		return false, err
	}
	r.ask.Info("web certificate: the admin's own, copied in from %s and %s", r.o.WebCertFile, r.o.WebKeyFile)
	return true, nil
}

// writeWebPair puts a certificate and its key in place of tls.crt and
// tls.key.
func (r *run) writeWebPair(certPEM, keyPEM []byte) error {
	crt, key := r.webPath(webcert.CertName), r.webPath(webcert.KeyName)
	if err := r.record(func(m *Manifest) { m.addWebTLS(crt); m.addWebTLS(key) }); err != nil {
		return err
	}
	if err := webcert.WriteAtomic(r.host.webDir, certPEM, keyPEM); err != nil {
		r.untrack(crt, key)
		return fmt.Errorf("writing the certificate of the web interface into %s: %w", r.host.webDir, err)
	}
	return nil
}

// webCertPVEProxy links tls.crt and tls.key to the pair pveproxy serves, and
// says what that gives away.
func (r *run) webCertPVEProxy() (bool, error) {
	crt, _, err := pve.ServedCert(r.host.nodeCertDir)
	if err != nil {
		return false, fmt.Errorf("finding the certificate pveproxy serves: %w", err)
	}
	key := webcert.KeyOf(crt)
	if _, err := os.Stat(key); err != nil {
		return false, fmt.Errorf("the key of %s: %w", crt, err)
	}
	r.ask.Warn("%s", pveproxyThreat)
	changedCrt, err := r.ensureLink(r.webPath(webcert.CertName), crt)
	if err != nil {
		return false, err
	}
	changedKey, err := r.ensureLink(r.webPath(webcert.KeyName), key)
	if err != nil {
		return false, err
	}
	if !changedCrt && !changedKey {
		r.ask.Info("web certificate: pveproxy's, nothing needed")
		return false, nil
	}
	r.ask.Info("web certificate: links to %s and %s, which pveproxy serves", crt, key)
	return true, nil
}

// ensureWebPin links pveproxy.crt to the certificate pveproxy serves.
func (r *run) ensureWebPin() (bool, error) {
	crt, _, err := pve.ServedCert(r.host.nodeCertDir)
	if err != nil {
		return false, fmt.Errorf("finding the certificate pveproxy serves: %w", err)
	}
	return r.ensureLink(r.webPath(webcert.PinName), crt)
}

// ensureLink makes path a link to target, unless it is one.
func (r *run) ensureLink(path, target string) (bool, error) {
	if have, err := os.Readlink(path); err == nil && have == target {
		return false, nil
	}
	if err := r.record(func(m *Manifest) { m.addWebTLS(path) }); err != nil {
		return false, err
	}
	if err := webcert.LinkAtomic(path, target); err != nil {
		r.untrack(path)
		return false, fmt.Errorf("linking %s to %s: %w", path, target, err)
	}
	return true, nil
}

// startWeb enables and starts the unit. One that was enabled before and runs
// is restarted when its files changed: it reads its credentials as it starts.
func (r *run) startWeb(ctx context.Context, restart bool) error {
	if restart {
		if _, err := r.run.Run(ctx, "systemctl", "try-restart", webUnit); err != nil {
			return fmt.Errorf("restarting %s: %w", webUnit, err)
		}
	}
	if err := r.record(func(m *Manifest) { m.WebEnabled = true }); err != nil {
		return err
	}
	if _, err := r.run.Run(ctx, "systemctl", "enable", "--now", webUnit); err != nil {
		return fmt.Errorf("starting %s: %w", webUnit, err)
	}
	r.ask.Info("%s: enabled and running", webUnit)
	return nil
}

// showWeb says where the web interface is, how to check its certificate and
// what the firewall needs.
func (r *run) showWeb(ctx context.Context, p webPlan) {
	host, port, err := net.SplitHostPort(p.listen)
	if err != nil {
		host, port = p.listen, webcert.Port
	}
	if a, err := netip.ParseAddr(host); err != nil || !a.IsLoopback() {
		// The name of the node, under which the browser has the ticket of
		// Proxmox VE as well.
		host = cmp.Or(p.fqdn, r.o.Node)
	}
	r.ask.Info("web interface: https://%s/", net.JoinHostPort(host, port))
	if leaf, err := readCert(r.webPath(webcert.CertName)); err == nil {
		r.ask.Info("web certificate: SHA-256 fingerprint %s", webcert.Fingerprint(leaf))
	}
	if p.mode == webcert.ModeCA {
		r.ask.Info("web certificate: browsers trust it once the cluster CA, %s, is imported, as for port 8006", r.host.clusterCA)
	}
	r.showFirewall(ctx, port)
}

// showFirewall prints the rule that lets browsers reach the web interface
// when the firewall of Proxmox VE is on.
func (r *run) showFirewall(ctx context.Context, port string) {
	var options struct {
		Enable pveBool `json:"enable"`
	}
	err := r.query(ctx, &options, "pvesh", "get", "/cluster/firewall/options", "--output-format", "json")
	switch {
	case err != nil:
		r.ask.Warn("whether the Proxmox VE firewall is on could not be read: %v; if it is, allow TCP port %s to this node", err, port)
	case bool(options.Enable):
		r.ask.Info("firewall: the Proxmox VE firewall is on; allow TCP port %s to this node, for example with "+
			"pvesh create /nodes/%s/firewall/rules --type in --action ACCEPT --proto tcp --dport %s --enable 1", port, r.o.Node, port)
	}
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cert, err := webcert.ParseCert(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cert, nil
}

func dateOf(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// lexists reports whether there is a file, a directory or a link at path.
func lexists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}
