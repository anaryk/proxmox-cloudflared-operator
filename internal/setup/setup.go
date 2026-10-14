// Package setup prepares a Proxmox VE node for pco and takes it away again:
// the role, user and API token the daemon reads Proxmox with, the gate tags,
// the cloudflared package, the first Cloudflare token, the web interface with
// its certificate, the identity of the install and the unit of the daemon.
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
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// The objects setup makes in Proxmox and on the node. The role, the user and
// its comment are those of the appliance's installer as well.
const (
	RoleID      = "PCO"
	UserID      = "pco@pve"
	UserComment = "pco operator"
	tokenName   = "pco"
	tokenID     = UserID + "!" + tokenName

	serviceUnit = "pco.service"
	egressUnit  = "pco-egress.service"
	webUnit     = "pco-web.service"
	// netnsUnit brings the gateway namespace up; the daemon enables it when
	// the first network is attached.
	netnsUnit = "pco-netns.service"

	// credentialLabel is the label of the credential setup stores.
	credentialLabel = "setup"
)

// defaultTags are the tags setup registers whatever the settings say, so that
// only an admin may set them.
func defaultTags() []string { return []string{"cf-tunnel", "cf-tunnel-managed"} }

// wantedTags are the tags to register: the default ones and the gate tag of
// the settings, when it is another.
func wantedTags(gate string) []string {
	tags := defaultTags()
	if !slices.Contains(tags, gate) {
		tags = append(tags, gate)
	}
	return tags
}

// ErrAborted is the error of an uninstall the operator did not confirm.
var ErrAborted = errors.New("aborted: nothing was changed")

// errAppliance is the answer of setup and uninstall inside an appliance, whose
// store the installer on the node makes and takes away.
var errAppliance = errors.New("this is an appliance: the installer on the node sets it up and removes it (pco appliance install|uninstall)")

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
	NewInstall         bool   // start an install beside the connectors of another
	Node               string // default: hostname
	Verbose            bool   // name every zone the token leaves out, not only those the install served

	// The web interface. WebCert is ca, own or pveproxy; empty keeps the mode
	// setup chose before, or is ca. WebCertFile and WebKeyFile go with own.
	// WebListen is the address pco-web listens on; empty keeps the one in
	// /etc/default/pco-web, or is the node's address in the cluster status.
	WebCert     string
	WebCertFile string
	WebKeyFile  string
	WebListen   string
	NoWeb       bool

	// Managed makes the managed network: the SDN zone pco and vnet pconet,
	// applied under the lock of the SDN configuration, and the network in the
	// store. ConfirmSDNApply applies the changes of others that are pending
	// along with pco's. Daemon stores the network through the daemon when
	// one runs, as ManagedOptions.Daemon does.
	Managed         bool
	ConfirmSDNApply bool
	Daemon          func(ctx context.Context) (asked bool, err error)

	// FirewallRule answers whether to add the datacenter firewall rule that
	// admits the agent channel from the nodes; nil asks (default yes).
	FirewallRule *bool

	// firewallAlone offers the firewall rule to a node that stands alone,
	// which is asked nothing of it otherwise: for the tests of the rule's
	// steps on a first node.
	firewallAlone bool
}

func (o Options) check() error {
	switch {
	case o.ConfirmSDNApply && !o.Managed:
		return errors.New("--confirm-sdn-apply goes with --managed")
	case o.Repair && o.Recover:
		return errors.New("--repair and --recover do not go together")
	case o.Repair && o.CloudflareToken != "":
		return errors.New("--repair does not store a Cloudflare token: add it with pco credential add")
	case o.CloudflareToken != "" && !credentials.ValidToken(strings.TrimSpace(o.CloudflareToken)):
		return credentials.ErrBadToken
	case o.NewInstall && (o.Repair || o.Recover):
		return errors.New("--new-install goes with neither --repair nor --recover")
	case o.InstallID != "" && !o.Recover:
		return errors.New("--install-id goes with --recover")
	case o.InstallID != "" && !validInstallID(o.InstallID):
		return fmt.Errorf("install id %q: want 12 lower-case hex characters", o.InstallID)
	}
	return o.checkWeb()
}

func (o Options) checkWeb() error {
	switch {
	case o.NoWeb && (o.WebCert != "" || o.WebCertFile != "" || o.WebKeyFile != "" || o.WebListen != ""):
		return errors.New("--no-web goes with none of --web-cert, --web-cert-file, --web-key-file and --web-listen")
	case o.WebCert != "" && o.WebCert != webcert.ModeCA && o.WebCert != webcert.ModeOwn && o.WebCert != webcert.ModePVEProxy:
		return fmt.Errorf("--web-cert %q: want ca, own or pveproxy", o.WebCert)
	case (o.WebCertFile == "") != (o.WebKeyFile == ""):
		return errors.New("--web-cert-file and --web-key-file go together")
	case o.WebCertFile != "" && o.WebCert != webcert.ModeOwn:
		return errors.New("--web-cert-file and --web-key-file go with --web-cert own")
	case o.WebCert == webcert.ModeOwn && o.WebCertFile == "":
		return errors.New("--web-cert own takes the certificate and its key from --web-cert-file and --web-key-file")
	}
	return nil
}

// Daemon is the daemon of this node, as uninstall asks it to hand the lease
// off before the node leaves the install: to any eligible node when to is
// empty. An error says why that is not possible.
type Daemon interface {
	Handoff(ctx context.Context, to string) error
}

// Setup sets up and removes pco on the node it runs on.
type Setup struct {
	run       Runner
	ask       Prompter
	st        *store.Store
	newClient func(token string) (cfapi.API, error)
	daemon    Daemon
	now       func() time.Time
	rand      io.Reader
	host      host
}

// host is where setup finds the parts of the node it works on.
type host struct {
	pveDir string // the mount point of the cluster filesystem
	// datacenterCfg is the file the options of the cluster are kept in,
	// the registered tags among them.
	datacenterCfg string
	// members lists the nodes of the cluster as pmxcfs sees them, corosync
	// and localCorosync are the cluster's corosync.conf and the node's own
	// copy, which a node that stands alone has neither of, and pveRootCA is
	// the certificate whose fingerprint identifies the cluster.
	members       string
	corosync      string
	localCorosync string
	pveRootCA     string
	// leaseLock is the lock of the lease in the cluster filesystem, which
	// uninstall removes on a node that stands alone.
	leaseLock string
	// addrs are the addresses of the node, for cluster.agentNetwork.
	addrs    func() ([]netip.Addr, error)
	keyring  string   // the key of Cloudflare's apt repository
	sources  string   // the apt source of cloudflared
	unitDirs []string // where the units of the package may be installed
	netnsPin string   // where the gateway namespace is pinned
	euid     func() int
	hostname func() (string, error)
	// checkToken makes one read of the Proxmox API with a token.
	checkToken func(ctx context.Context, tok store.PVEToken) error
	// snapshot reads the nodes and the guests of the cluster with a token,
	// as the daemon does, for the checks of the managed subnet.
	snapshot func(ctx context.Context, tok store.PVEToken) (inventory.Snapshot, error)
	// profile reads the profile of the machine.
	profile func() (string, error)
	// sleep waits, as between two looks at the guests whose managed cards
	// were removed.
	sleep func(ctx context.Context, d time.Duration) error

	// The web interface: the directory of its certificate, the environment
	// file of its unit, the cluster CA and the directory of the certificates
	// pveproxy serves. fqdn finds the fully qualified name of a host name in
	// /etc/hosts.
	webDir       string
	webEnv       string
	clusterCA    string
	clusterCAKey string
	nodeCertDir  string
	fqdn         func(hostname string) string
}

func nodeHost() host {
	return host{
		pveDir:        "/etc/pve",
		datacenterCfg: "/etc/pve/datacenter.cfg",
		members:       members.MembersFile,
		corosync:      members.CorosyncFile,
		localCorosync: members.CorosyncLocalFile,
		pveRootCA:     members.RootCAFile,
		leaseLock:     lease.DefaultLock,
		addrs:         interfaceAddrs,
		keyring:       "/usr/share/keyrings/cloudflare-main.gpg",
		sources:       "/etc/apt/sources.list.d/cloudflared.sources",
		unitDirs:      []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"},
		netnsPin:      filepath.Join("/run/netns", gateway.Namespace),
		euid:          os.Geteuid,
		hostname:      os.Hostname,
		checkToken:    checkPVEToken,
		snapshot:      readSnapshot,
		profile:       func() (string, error) { return store.DetectProfile(store.ProfileFile) },
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},

		webDir:       webcert.Dir,
		webEnv:       webcert.EnvFile,
		clusterCA:    webcert.ClusterCA,
		clusterCAKey: webcert.ClusterCAKey,
		nodeCertDir:  pve.DefaultNodeCertDir,
		fqdn: func(hostname string) string {
			etcHosts, _ := os.ReadFile("/etc/hosts")
			return webcert.FQDN(hostname, etcHosts)
		},
	}
}

// interfaceAddrs lists the unicast addresses of the node's interfaces.
func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
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

// readSnapshot reads the cluster once through the API, as the daemon reads
// it, with a token.
func readSnapshot(ctx context.Context, tok store.PVEToken) (inventory.Snapshot, error) {
	c, err := pve.New(pve.Config{BaseURL: pve.DefaultURL, TokenID: tok.TokenID, Secret: tok.Secret.Reveal()})
	if err != nil {
		return inventory.Snapshot{}, err
	}
	return inventory.New(c, inventory.Options{}, time.Now, zerolog.Nop()).Refresh(ctx), nil
}

// New returns a setup that runs commands through r, asks p, keeps its state in
// st, checks Cloudflare tokens through the clients newClient makes, asks d
// for the lease to be handed off and draws the install id, the writer nonce,
// the keys and the names of probe objects from rand. The manifest, the lock
// of the node and the connectors are found below the roots of st, and
// uninstall removes those roots.
func New(r Runner, p Prompter, st *store.Store, newClient func(token string) (cfapi.API, error), d Daemon, now func() time.Time, rand io.Reader) *Setup {
	return &Setup{run: r, ask: p, st: st, newClient: newClient, daemon: d, now: now, rand: rand, host: nodeHost()}
}

// run is the state of one Run.
type run struct {
	*Setup
	o           Options
	token       string // the Cloudflare token, once there is one
	version     PVEVersion
	install     store.Install
	manifest    Manifest
	running     *bool  // whether the daemon runs, once that is known
	newPVEToken bool   // the Proxmox token was made in this run
	gate        string // the gate tag of the settings, once they are read
	// others are the registered nodes besides this one that were not
	// removed, by name: with any, the node joins their install. agentAddress
	// is where the node's agent listens, and serial and fingerprint name its
	// client certificate, once the cluster step issued or kept it.
	others       []string
	agentAddress netip.Addr
	serial       string
	fingerprint  string
	// What a recovery did with the daemon: looked at it, and stopped it as
	// it was running.
	looked, stopped bool
	// claimRev is the revision of the install this node claimed a
	// restored store at.
	claimRev int64
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
	if err := s.refuseAppliance(); err != nil {
		return err
	}
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

// refuseAppliance refuses to work on an appliance. A profile that cannot be
// read is no host either.
func (s *Setup) refuseAppliance() error {
	profile, err := s.host.profile()
	switch {
	case err != nil:
		return err
	case profile == store.ProfileAppliance:
		return errAppliance
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
	var managed []step
	if r.o.Managed {
		managed = []step{{"managed", r.ensureManaged}}
	}
	if r.o.Repair {
		return slices.Concat([]step{
			{"preflight", r.preflight},
			{"store", r.requireInstall},
			{"cluster", r.ensureCluster},
			{"role", r.ensureRole},
			{"user", r.ensureUser},
			{"token", r.ensureToken},
			{"tags", r.ensureTags},
			{"firewall", r.ensureFirewall},
		}, managed, []step{
			{"web", r.ensureWeb},
			{"registry", r.register},
			{"service", r.startService},
		})
	}
	return slices.Concat([]step{
		{"preflight", r.preflight},
		{"store", r.prepareStore},
		{"cluster", r.ensureCluster},
		{"role", r.ensureRole},
		{"user", r.ensureUser},
		{"token", r.ensureToken},
		{"tags", r.ensureTags},
		{"firewall", r.ensureFirewall},
	}, managed, []step{
		{"cloudflared", r.ensureCloudflared},
		{"credential", r.addCredential},
		{"web", r.ensureWeb},
		{"registry", r.register},
		{"service", r.startService},
	})
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

// serviceActive asks systemd whether the daemon runs, or is starting or
// stopping.
func (s *Setup) serviceActive(ctx context.Context) (bool, error) {
	unit := serviceUnit
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
	running, err := r.serviceActive(ctx)
	if err == nil && !running {
		running, err = r.daemonLocked()
	}
	if err != nil {
		return false, err
	}
	r.running = &running
	return running, nil
}
