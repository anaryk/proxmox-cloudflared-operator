package daemon

import (
	"bytes"
	"cmp"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

const (
	// webService is the unit of the web interface.
	webService = "pco-web.service"
	// webCredentials is where systemd puts the credentials pco-web.service
	// loaded as it started.
	webCredentials = "/run/credentials/" + webService
	// webCheckEvery is how often the certificates of the web interface are
	// looked at.
	webCheckEvery = time.Minute
	systemctlBin  = "/usr/bin/systemctl"
)

// webKeeper keeps what pco-web.service loads in step with the node while
// setup has the web interface set up: the link to the certificate pveproxy
// serves, which pco-web pins its sign-ins to; in mode ca, a leaf of the
// cluster CA that is due for renewal; in mode pveproxy, the links to the pair
// pveproxy serves. pco-web reads its credentials as it starts, so it is
// restarted when what it loaded is not what the files hold, once for each
// change. The key of the cluster CA is read here, by root, and never by
// pco-web.
type webKeeper struct {
	setup func() (setup.WebSetup, error)
	node  string
	fqdn  func() string
	addrs func(ctx context.Context) ([]netip.Addr, error) // of the node in the cluster status

	dir, env   string // the certificate of the web interface and the environment file of its unit
	ca, caKey  string // the cluster CA
	local      string // the certificates pveproxy serves
	loaded     string // what pco-web.service loaded
	restart    func(ctx context.Context) error
	note       func(msg string)
	now        func() time.Time
	rand       io.Reader
	log        zerolog.Logger
	failedLast map[string]string // the failure said last, by what failed

	restartedFor string // digest of the files pco-web was last restarted for; empty once it loads them
	unchanged    bool   // said that a restart left pco-web on what it loaded before
}

// newWebKeeper returns the keeper of the node the daemon runs on.
func newWebKeeper(cfg Config, deps Deps, client *pve.Client, note func(string), log zerolog.Logger) *webKeeper {
	return &webKeeper{
		setup:   func() (setup.WebSetup, error) { return setup.ReadWebSetup(cfg.Paths.Local) },
		node:    cfg.Node,
		fqdn:    webcert.NodeFQDN,
		addrs:   func(ctx context.Context) ([]netip.Addr, error) { return nodeAddrs(ctx, client, cfg.Node) },
		dir:     cmp.Or(deps.WebDir, webcert.Dir),
		env:     cmp.Or(deps.WebEnv, webcert.EnvFile),
		ca:      cmp.Or(deps.ClusterCA, webcert.ClusterCA),
		caKey:   cmp.Or(deps.ClusterCAKey, webcert.ClusterCAKey),
		local:   cmp.Or(deps.PVECertDir, pve.DefaultNodeCertDir),
		loaded:  cmp.Or(deps.WebLoaded, webCredentials),
		restart: deps.RestartWeb,
		note:    note,
		now:     deps.Now,
		rand:    crand.Reader,
		log:     log,
	}
}

// nodeAddrs returns the address the cluster status lists for this node, as
// setup reads it.
func nodeAddrs(ctx context.Context, client *pve.Client, node string) ([]netip.Addr, error) {
	st, err := client.ClusterStatus(ctx)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, n := range st.Nodes {
		if (n.Local || n.Name == node) && n.Addr.IsValid() {
			out = append(out, n.Addr)
		}
	}
	return out, nil
}

// tryRestartWeb restarts pco-web.service if it runs, without waiting for it.
func tryRestartWeb(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, systemctlBin, "try-restart", "--no-block", "--", webService).CombinedOutput()
	if err != nil {
		if detail := strings.TrimSpace(string(out)); detail != "" {
			return fmt.Errorf("systemctl try-restart %s: %w: %s", webService, err, detail)
		}
		return fmt.Errorf("systemctl try-restart %s: %w", webService, err)
	}
	return nil
}

// keep checks at once and then every interval, until ctx ends.
func (k *webKeeper) keep(ctx context.Context, every time.Duration) { keepChecking(ctx, every, k.check) }

func keepChecking(ctx context.Context, every time.Duration, check func(context.Context)) {
	check(ctx)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			check(ctx)
		}
	}
}

// check looks once.
func (k *webKeeper) check(ctx context.Context) {
	web, err := k.setup()
	if err != nil {
		k.failed("reading what setup did for the web interface", err)
		return
	}
	if !web.Enabled {
		return
	}
	served, _, err := pve.ServedCert(k.local)
	if err != nil {
		k.failed("finding the certificate pveproxy serves", err)
		return
	}
	k.repoint(webcert.PinName, served)
	switch web.Mode {
	case webcert.ModeCA:
		k.renew(ctx)
	case webcert.ModePVEProxy:
		k.repoint(webcert.CertName, served)
		k.repoint(webcert.KeyName, webcert.KeyOf(served))
	}
	k.restartIfStale(ctx, webcert.CertName, webcert.KeyName, webcert.PinName)
}

// repoint makes the link name lead to target, when it does not.
func (k *webKeeper) repoint(name, target string) {
	path := filepath.Join(k.dir, name)
	if have, err := os.Readlink(path); err == nil && have == target {
		return
	}
	if err := webcert.LinkAtomic(path, target); err != nil {
		k.failed("pointing "+path+" to "+target, err)
		return
	}
	k.log.Info().Str("link", path).Str("to", target).Msg("pveproxy serves another certificate; the link follows it")
}

// renew makes a new key and leaf of the cluster CA when the one there is due:
// it expires within 30 days, another CA signed it, it does not name every name
// and address the node has now, or tls.crt is a link to somebody else's file.
func (k *webKeeper) renew(ctx context.Context) {
	const what = "renewing the certificate of the web interface"
	ca, err := readCertFile(k.ca)
	if err != nil {
		k.failed(what, fmt.Errorf("reading the cluster CA: %w", err))
		return
	}
	names, err := k.names(ctx)
	if err != nil {
		k.failed(what, err)
		return
	}
	why := k.due(ca, names)
	if why == "" {
		k.recovered(what)
		return
	}
	b, err := os.ReadFile(k.caKey)
	if err != nil {
		k.failed(what, fmt.Errorf("reading the key of the cluster CA: %w", err))
		return
	}
	caKey, err := webcert.ParseKey(b)
	if err != nil {
		k.failed(what, fmt.Errorf("reading the key of the cluster CA: %w", err))
		return
	}
	certPEM, keyPEM, err := webcert.Issue(ca, caKey, names, k.now(), webcert.Lifetime, k.rand)
	if err == nil {
		err = webcert.WriteAtomic(k.dir, certPEM, keyPEM)
	}
	if err != nil {
		k.failed(what, err)
		return
	}
	k.recovered(what)
	leaf, err := webcert.ParseCert(certPEM)
	if err != nil {
		k.failed(what, err)
		return
	}
	msg := "web certificate renewed: " + why + ", fingerprint " + webcert.Fingerprint(leaf)
	k.log.Info().Msg(msg)
	k.note(msg)
}

// due says why the pair there must be made anew; empty when it need not.
func (k *webKeeper) due(ca *x509.Certificate, names webcert.Names) string {
	if info, err := os.Lstat(filepath.Join(k.dir, webcert.CertName)); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return webcert.CertName + " is a link, not a leaf of its own"
	}
	certPEM, keyPEM, err := webcert.ReadPair(k.dir)
	if err != nil {
		return "it could not be read"
	}
	leaf, err := webcert.ParseCert(certPEM)
	if err != nil {
		return "tls.crt holds no certificate"
	}
	if key, err := webcert.ParseKey(keyPEM); err != nil || !webcert.Matches(leaf, key) {
		return "tls.key is not its key"
	}
	_, why := webcert.Due(leaf, ca, names, k.now())
	return why
}

// names are the names the node's leaf is for now.
func (k *webKeeper) names(ctx context.Context) (webcert.Names, error) {
	env, _, err := webcert.ReadEnv(k.env)
	if err != nil {
		return webcert.Names{}, fmt.Errorf("reading %s: %w", k.env, err)
	}
	addrs, err := k.addrs(ctx)
	if err != nil {
		return webcert.Names{}, fmt.Errorf("reading the cluster status: %w", err)
	}
	return webcert.NodeNames(k.node, k.fqdn(), addrs, env.Listen, env.Hosts)
}

// restartIfStale restarts pco-web when what it loaded as it started of the
// files names is not what they hold now. One that does not run reads them as
// it starts. A restart is not repeated for the files it was made for: when
// pco-web still loads something else after it, a drop-in of the unit points
// the credentials elsewhere, and more restarts would change nothing.
func (k *webKeeper) restartIfStale(ctx context.Context, names ...string) {
	const what = "restarting " + webService
	stale := false
	var files []byte
	for _, name := range names {
		loaded, err := os.ReadFile(filepath.Join(k.loaded, name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return
		case err != nil:
			k.failed("reading what "+webService+" loaded", err)
			return
		}
		have, err := os.ReadFile(filepath.Join(k.dir, name))
		if err != nil {
			k.failed("reading "+filepath.Join(k.dir, name), err)
			return
		}
		stale = stale || !bytes.Equal(loaded, have)
		files = fmt.Appendf(files, "%s %d\n%s", name, len(have), have)
	}
	if !stale {
		k.restartedFor, k.unchanged = "", false
		return
	}
	sum := sha256.Sum256(files)
	digest := string(sum[:])
	if digest == k.restartedFor {
		if !k.unchanged {
			k.unchanged = true
			k.log.Warn().Msg("restarted for this change already; pco-web still loads something else, check the unit's drop-ins")
		}
		return
	}
	if err := k.restart(ctx); err != nil {
		k.failed(what, err)
		return
	}
	k.restartedFor, k.unchanged = digest, false
	k.recovered(what)
	k.log.Info().Msg(webService + " is restarted, so that it loads the certificates as they are now")
}

// failed logs a failure, once until it changes or what failed succeeds:
// the keeper looks every minute.
func (k *webKeeper) failed(what string, err error) {
	if k.failedLast == nil {
		k.failedLast = make(map[string]string)
	}
	if k.failedLast[what] == err.Error() {
		return
	}
	k.failedLast[what] = err.Error()
	k.log.Warn().Err(err).Msg(what + " failed")
}

func (k *webKeeper) recovered(what string) { delete(k.failedLast, what) }

func readCertFile(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return webcert.ParseCert(b)
}

// facts are what the doctor reads of the certificate of the web interface.
func (k *webKeeper) facts(context.Context) (doctor.WebCert, bool) {
	web, err := k.setup()
	switch {
	case err != nil:
		return doctor.WebCert{Err: err}, true
	case !web.Enabled:
		return doctor.WebCert{}, false
	}
	certPEM, keyPEM, err := webcert.ReadPair(k.dir)
	return doctor.WebCert{Mode: web.Mode, Cert: certPEM, Key: keyPEM, Err: err}, true
}
