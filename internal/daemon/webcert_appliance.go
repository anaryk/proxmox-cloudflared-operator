package daemon

import (
	"bytes"
	"cmp"
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// applianceWebKeeper keeps what pco-web.service loads in the appliance: a key
// of its own and a self-signed certificate, made at the first start of the
// daemon and again 30 days before it expires, unless the admin imported one
// of their own; and the node's API as this daemon reaches it, which pco-web
// signs users in at. The node's key is never among them. pco-web reads them
// as it starts, so it is restarted when what it loaded is not what the files
// hold.
type applianceWebKeeper struct {
	*webKeeper
	install  store.ApplianceInstall
	hostname func() (string, error)
	net0     string // the file of net0's address
}

func newApplianceWebKeeper(deps Deps, app ApplianceDeps, install store.ApplianceInstall, note func(string), log zerolog.Logger) *applianceWebKeeper {
	hostname := app.Hostname
	if hostname == nil {
		hostname = os.Hostname
	}
	return &applianceWebKeeper{
		webKeeper: &webKeeper{
			fqdn: func() string {
				name, err := hostname()
				if err != nil {
					return ""
				}
				etcHosts, _ := os.ReadFile("/etc/hosts")
				return webcert.FQDN(name, etcHosts)
			},
			dir:     cmp.Or(deps.WebDir, webcert.Dir),
			env:     cmp.Or(deps.WebEnv, webcert.EnvFile),
			loaded:  cmp.Or(deps.WebLoaded, webCredentials),
			restart: deps.RestartWeb,
			note:    note,
			now:     deps.Now,
			rand:    crand.Reader,
			log:     log,
		},
		install:  install,
		hostname: hostname,
		net0:     cmp.Or(app.Net0, webcert.Net0File),
	}
}

func (k *applianceWebKeeper) keep(ctx context.Context, every time.Duration) {
	keepChecking(ctx, every, k.check)
}

// check writes the API first: once the certificate is there, which the
// installer waits for before it starts pco-web, everything pco-web loads is.
func (k *applianceWebKeeper) check(ctx context.Context) {
	k.writeAPI()
	k.ensureCert()
	k.restartIfStale(ctx, webcert.CertName, webcert.KeyName, webcert.APIName)
}

// ensureCert makes the key and the certificate when there are none, or when
// the appliance's own are due; the admin's own are left alone.
func (k *applianceWebKeeper) ensureCert() {
	const what = "making the certificate of the web interface"
	mode, err := webcert.ReadMode(k.dir)
	if err != nil {
		k.failed(what, err)
		return
	}
	if mode == webcert.ModeOwn {
		k.recovered(what)
		return
	}
	why := webcert.SelfSignedDue(k.dir, k.now())
	if why == "" {
		k.recovered(what)
		return
	}
	names, err := k.names()
	if err != nil {
		k.failed(what, err)
		return
	}
	leaf, err := webcert.MakeSelfSigned(k.dir, names, k.now(), k.rand)
	if err != nil {
		k.failed(what, err)
		return
	}
	k.recovered(what)
	msg := "web certificate renewed: " + why + ", fingerprint " + webcert.Fingerprint(leaf)
	if why == webcert.NoCertificate {
		msg = "web certificate made for " + names.String() + ", fingerprint " + webcert.Fingerprint(leaf)
	}
	k.log.Info().Msg(msg)
	k.note(msg)
}

// names are the appliance's host name, its FQDN, the address pco-web listens
// on and the names of PCO_WEB_HOSTS.
func (k *applianceWebKeeper) names() (webcert.Names, error) {
	hostname, err := k.hostname()
	if err != nil {
		return webcert.Names{}, fmt.Errorf("reading the host name: %w", err)
	}
	return webcert.ApplianceNamesFrom(hostname, k.fqdn(), k.env, k.net0)
}

// writeAPI writes the node's API as the install has it: its first endpoint,
// verified under its server name against the CA the installer pushed and the
// system roots, as the daemon does.
func (k *applianceWebKeeper) writeAPI() {
	const what = "writing the API of the node for the web interface"
	if len(k.install.Endpoints) == 0 {
		k.failed(what, errors.New("the install records no endpoint of the API"))
		return
	}
	ca, err := os.ReadFile(k.install.CAFile)
	if err != nil {
		k.failed(what, fmt.Errorf("reading the CA of the API: %w", err))
		return
	}
	ep := k.install.Endpoints[0]
	data := webcert.API{
		URL: "https://" + ep.Address + "/api2/json", ServerName: ep.ServerName, CA: string(ca), Node: k.install.Node,
	}.Encode()
	path := filepath.Join(k.dir, webcert.APIName)
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, data) {
		k.recovered(what)
		return
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		k.failed(what, err)
		return
	}
	if err := os.MkdirAll(k.dir, 0o755); err != nil {
		k.failed(what, err)
		return
	}
	if err := atomicfile.Write(path, data, atomicfile.Options{Mode: 0o644}); err != nil {
		k.failed(what, err)
		return
	}
	k.recovered(what)
	k.log.Info().Str("api", ep.Address).Str("serverName", ep.ServerName).Msg("the web interface signs users in at the API of the install")
}

// facts are what the doctor reads of the web interface of the appliance: its
// certificate, the address it listens on and net0's.
func (k *applianceWebKeeper) facts(context.Context) (doctor.WebCert, bool) {
	w := doctor.WebCert{Appliance: true}
	mode, merr := webcert.ReadMode(k.dir)
	certPEM, keyPEM, err := webcert.ReadPair(k.dir)
	w.Mode, w.Cert, w.Key, w.Err = mode, certPEM, keyPEM, errors.Join(merr, err)
	w.Net0, w.Net0Err = webcert.ReadNet0(k.net0)
	env, _, err := webcert.ReadEnv(k.env)
	if err != nil {
		w.ListenErr = fmt.Errorf("reading %s: %w", k.env, err)
	}
	w.Listen = webcert.ApplianceListen(env, k.net0)
	return w, true
}
