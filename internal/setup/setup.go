// Package setup prepares a Proxmox VE node for pco and takes it away again:
// the role, user and API token the daemon reads Proxmox with, the gate tags,
// the cloudflared package, the first Cloudflare token, the identity of the
// install and the unit of the daemon.
//
// Every step looks at what is there before it changes anything, so a run that
// stopped half way is finished by running it again, and a second run changes
// nothing. What setup creates is noted in a manifest on the node as soon as it
// is made, and uninstall removes only what the manifest lists.
//
// Neither the secret of the Proxmox token nor a Cloudflare token is ever
// printed, put into an error or passed to a command.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The objects setup makes in Proxmox and on the node.
const (
	roleID      = "PCO"
	userID      = "pco@pve"
	tokenName   = "pco"
	tokenID     = userID + "!" + tokenName
	userComment = "pco operator"

	serviceUnit = "pco.service"
	egressUnit  = "pco-egress.service"

	// credentialLabel is the label of the credential setup stores.
	credentialLabel = "setup"
)

// gateTags are the tags setup registers, so that only an admin may set them.
func gateTags() []string { return []string{"cf-tunnel", "cf-tunnel-managed"} }

// ErrAborted is the error of an uninstall the operator did not confirm.
var ErrAborted = errors.New("aborted: nothing was changed")

// Options are the answers to the questions of setup that were given in
// advance.
type Options struct {
	Yes                bool   // accept defaults, never prompt
	RegisterTags       *bool  // nil: ask (default yes)
	InstallCloudflared *bool  // nil: ask (default yes when cloudflared is missing)
	CloudflareToken    string // from --cf-token-file or stdin; empty: ask, may be skipped
	Repair             bool
	Recover            bool
	InstallID          string // with Recover
	Node               string // default: hostname
}

func (o Options) check() error {
	switch {
	case o.Repair && o.Recover:
		return errors.New("--repair and --recover do not go together")
	case o.Repair && o.CloudflareToken != "":
		return errors.New("--repair does not store a Cloudflare token: add it with pco credential add")
	case o.InstallID != "" && !o.Recover:
		return errors.New("--install-id goes with --recover")
	case o.InstallID != "" && !validInstallID(o.InstallID):
		return fmt.Errorf("install id %q: want 12 lower-case hex characters", o.InstallID)
	}
	return nil
}

// Setup sets up and removes pco on the node it runs on.
type Setup struct {
	run       Runner
	ask       Prompter
	st        *store.Store
	newClient func(token string) (cfapi.API, error)
	now       func() time.Time
	rand      io.Reader
	host      host
}

// host is where setup finds the parts of the node it works on.
type host struct {
	pveDir   string   // the mount point of the cluster filesystem
	keyring  string   // the key of Cloudflare's apt repository
	sources  string   // the apt source of cloudflared
	unitDirs []string // where the units of the package may be installed
	euid     func() int
	hostname func() (string, error)
	// checkToken makes one read of the Proxmox API with a token.
	checkToken func(ctx context.Context, tok store.PVEToken) error
}

func nodeHost() host {
	return host{
		pveDir:     "/etc/pve",
		keyring:    "/usr/share/keyrings/cloudflare-main.gpg",
		sources:    "/etc/apt/sources.list.d/cloudflared.sources",
		unitDirs:   []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"},
		euid:       os.Geteuid,
		hostname:   os.Hostname,
		checkToken: checkPVEToken,
	}
}

// checkPVEToken reads the version of Proxmox VE with a token, through the API
// the daemon reads the guests from.
func checkPVEToken(ctx context.Context, tok store.PVEToken) error {
	c, err := pve.New(pve.Config{BaseURL: pve.DefaultURL, TokenID: tok.TokenID, Secret: tok.Secret.Reveal()})
	if err != nil {
		return err
	}
	_, err = c.Version(ctx)
	return err
}

// New returns a setup that runs commands through r, asks p, keeps its state in
// st, checks Cloudflare tokens through the clients newClient makes and draws
// the install id, the writer nonce and the names of probe objects from rand.
// The manifest, the lock of the node and the connectors are found below the
// roots of st, and uninstall removes those roots.
func New(r Runner, p Prompter, st *store.Store, newClient func(token string) (cfapi.API, error), now func() time.Time, rand io.Reader) *Setup {
	return &Setup{run: r, ask: p, st: st, newClient: newClient, now: now, rand: rand, host: nodeHost()}
}

// run is the state of one Run.
type run struct {
	*Setup
	o           Options
	token       string // the Cloudflare token, once there is one
	version     pveVersion
	install     store.Install
	manifest    Manifest
	running     *bool // whether the daemon runs, once that is known
	newPVEToken bool  // the Proxmox token was made in this run
	// What a recovery did with the daemon: looked at it, and stopped it as
	// it was running.
	looked, stopped bool
}

type step struct {
	name string
	do   func(ctx context.Context) error
}

// Run sets up the node, or with Repair re-asserts what Proxmox holds for pco,
// or with Recover adopts an install whose store was lost. A step that fails
// stops the run with its name in the error; running again goes on from what
// is there.
func (s *Setup) Run(ctx context.Context, o Options) error {
	if err := o.check(); err != nil {
		return err
	}
	r := &run{Setup: s, o: o, token: strings.TrimSpace(o.CloudflareToken)}
	if r.o.Node == "" {
		name, err := s.host.hostname()
		if err != nil {
			return fmt.Errorf("reading the host name: %w", err)
		}
		r.o.Node, _, _ = strings.Cut(name, ".")
	}
	m, _, err := readManifest(s.manifestPath())
	if err != nil {
		return fmt.Errorf("reading the manifest: %w", err)
	}
	r.manifest = m
	for _, st := range r.steps() {
		if err := st.do(ctx); err != nil {
			r.afterFailure(ctx)
			return fmt.Errorf("setup step %s: %w", st.name, err)
		}
	}
	return nil
}

// afterFailure starts the daemon again that a recovery stopped, and says so;
// one that did not run stays stopped.
func (r *run) afterFailure(ctx context.Context) {
	switch {
	case r.stopped:
		if _, err := r.run.Run(ctx, "systemctl", "start", serviceUnit); err != nil {
			r.ask.Warn("%s was running before --recover and could not be started again: %v", serviceUnit, err)
			return
		}
		r.ask.Warn("%s was running before --recover and is started again", serviceUnit)
	case r.looked:
		r.ask.Info("%s was not running before --recover and stays stopped", serviceUnit)
	}
}

func (r *run) steps() []step {
	if r.o.Repair {
		return []step{
			{"preflight", r.preflight},
			{"store", r.requireInstall},
			{"role", r.ensureRole},
			{"user", r.ensureUser},
			{"token", r.ensureToken},
			{"tags", r.ensureTags},
			{"registry", r.register},
			{"service", r.startService},
		}
	}
	return []step{
		{"preflight", r.preflight},
		{"store", r.prepareStore},
		{"role", r.ensureRole},
		{"user", r.ensureUser},
		{"token", r.ensureToken},
		{"tags", r.ensureTags},
		{"cloudflared", r.ensureCloudflared},
		{"credential", r.addCredential},
		{"registry", r.register},
		{"service", r.startService},
	}
}

// choose returns the answer given in advance, the default with Yes, or what
// the operator answers.
func (r *run) choose(given *bool, def bool, question string) (bool, error) {
	switch {
	case given != nil:
		return *given, nil
	case r.o.Yes:
		return def, nil
	}
	return r.ask.Confirm(question, def)
}

// record notes in the manifest what this run is about to create and writes
// it at once, before the command that creates it: the object was not there a
// moment before, so if it is there later it is setup's, also when this run is
// killed right after making it. A create that fails leaves the note, and
// uninstall takes what is gone already for done.
func (r *run) record(change func(*Manifest)) error {
	change(&r.manifest)
	if r.manifest.Node == "" {
		r.manifest.Node = r.o.Node
	}
	if r.manifest.InstalledAt.IsZero() {
		r.manifest.InstalledAt = r.now()
	}
	if err := writeManifest(r.manifestPath(), r.manifest); err != nil {
		return fmt.Errorf("writing the manifest: %w", err)
	}
	return nil
}

// paths are the roots of the store setup works on.
func (s *Setup) paths() store.Paths { return s.st.Paths() }

// takeBack takes back the note of a create that failed, when it left
// nothing: what an admin makes in its place later is the admin's. When that
// cannot be told, the note stays.
func (r *run) takeBack(there func() (bool, error), unnote func(*Manifest)) {
	if present, err := there(); err == nil && !present {
		if err := r.record(unnote); err != nil {
			r.ask.Warn("%v", err)
		}
	}
}

func (s *Setup) manifestPath() string { return filepath.Join(s.paths().Local, manifestName) }

// unitInstalled reports whether the unit file of a unit is where systemd
// looks for it.
func (s *Setup) unitInstalled(unit string) bool {
	for _, dir := range s.host.unitDirs {
		if _, err := os.Stat(filepath.Join(dir, unit)); err == nil {
			return true
		}
	}
	return false
}

// serviceActive asks systemd whether a unit runs, or is starting or stopping.
func (s *Setup) serviceActive(ctx context.Context, unit string) (bool, error) {
	out, err := s.run.Run(ctx, "systemctl", "is-active", unit)
	state := strings.TrimSpace(out)
	switch state {
	case "active", "activating", "deactivating", "reloading", "refreshing":
		return true, nil
	case "inactive", "failed":
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("asking systemd about %s: %w", unit, err)
	}
	return false, fmt.Errorf("systemd says %s is %q", unit, state)
}

// daemonRunning reports whether a daemon runs: pco.service, or one started by
// hand, which holds the lock of the node as well. It looks only once.
func (r *run) daemonRunning(ctx context.Context) (bool, error) {
	if r.running != nil {
		return *r.running, nil
	}
	running, err := r.serviceActive(ctx, serviceUnit)
	if err == nil && !running {
		running, err = r.daemonLocked()
	}
	if err != nil {
		return false, err
	}
	r.running = &running
	return running, nil
}
