// Package applianceinstall installs, repairs and removes the pco appliance
// from the Proxmox VE node it runs on: the unprivileged container that runs
// pco and its connectors, the objects in Proxmox it needs, and the bootstrap
// that pco appliance init turns into its store. It runs as root on the node,
// from a temporary directory, and leaves nothing on the node but what Proxmox
// holds for the appliance: no binary, no unit, nothing under /etc or /usr.
//
// Every object it makes carries a mark (markers.go) and is noted in a journal
// before the command that makes it runs, so that a run that fails or is
// interrupted takes back what it made, and a run that was killed is finished
// or taken back with --resume. Neither the journal nor anything printed holds
// a secret: the secret of the token lives in memory until it is pushed.
package applianceinstall

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

// JournalDir is where the journals of the runs and the lock of the installer
// are kept.
const JournalDir = "/root/.pco-appliance-install"

// The defaults of an appliance.
const (
	DefaultBridge   = "vmbr0"
	DefaultIP       = "dhcp"
	DefaultCores    = 1
	DefaultMemoryMB = 768
	DefaultRootFSGB = 4
	DefaultStateGB  = 1
)

var installID = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ReleaseBase is where the template of a release is downloaded from.
func ReleaseBase(version string) string {
	return "https://github.com/anaryk/proxmox-cloudflared-operator/releases/download/v" + version
}

// Options are the answers to the questions of the installer given in
// advance. The zero values of the numbers and strings with a default take
// that default; KeepTemplate has none, the command line gives it true.
type Options struct {
	Yes     bool   // take the default answers and ask nothing; never adds a NoAccess line
	VMID    int    // 0: the next free id the cluster offers
	Storage string // of the container; required unless exactly one storage holds containers
	// TemplateStorage is where the template is downloaded to: Storage when it
	// holds templates, else the one storage of the node that does.
	TemplateStorage string
	Bridge          string // default vmbr0
	VLAN            int    // tag= on net0; 0 untagged, which a VLAN-aware bridge refuses
	IP              string // "dhcp" (default) or "<cidr>[,gw=<addr>]"
	APIHost         string // default: the node's address on Bridge; required when it has none
	APICA           string // a CA file for a certificate the system roots do not know
	Cores, MemoryMB int
	RootFSGB        int
	StateGB         int
	Template        string // a local file; empty: downloaded through Proxmox
	ReleaseBase     string // default ReleaseBase(version)
	ChecksumsFile   string // the checksums.txt install.sh verified
	KeepTemplate    bool   // a template the run downloaded stays after a rollback and an uninstall
	// CloudflareToken is the first Cloudflare token, from --cf-token-file;
	// empty: none, one is added inside later.
	CloudflareToken string `json:"-"`
	// CloudflareAPI is PCO_CLOUDFLARE_API_URL as cmd/pco validated it, for
	// tests only: forwarded to the pco commands run in the container.
	CloudflareAPI string
	RegisterTags  *bool  // nil: ask, default yes
	GateTag       string // default the gate tag of the default settings
	DenyAccess    bool   // add NoAccess for the principals that can reach in, instead of refusing
	Recover       bool   // with Repair: the volume holds no state
	InstallID     string // with Repair and Recover: the install to adopt
	Resume        string `json:"-"` // a journal to finish
}

func (o *Options) defaults() {
	if o.Bridge == "" {
		o.Bridge = DefaultBridge
	}
	if o.IP == "" {
		o.IP = DefaultIP
	}
	if o.Cores == 0 {
		o.Cores = DefaultCores
	}
	if o.MemoryMB == 0 {
		o.MemoryMB = DefaultMemoryMB
	}
	if o.RootFSGB == 0 {
		o.RootFSGB = DefaultRootFSGB
	}
	if o.StateGB == 0 {
		o.StateGB = DefaultStateGB
	}
	if o.GateTag == "" {
		o.GateTag = store.DefaultSettings().GateTag
	}
	// The journal keeps the CA by its absolute path: a resumed run may run
	// from another directory.
	if o.APICA != "" && !filepath.IsAbs(o.APICA) {
		if abs, err := filepath.Abs(o.APICA); err == nil {
			o.APICA = abs
		}
	}
}

func (o Options) check() error {
	switch {
	case o.VMID != 0 && (o.VMID < 100 || o.VMID > 999999999):
		return fmt.Errorf("--vmid %d: want 100 to 999999999", o.VMID)
	case o.VLAN < 0 || o.VLAN > 4094:
		return fmt.Errorf("--vlan %d: want 1 to 4094, or none for a card without a tag", o.VLAN)
	case o.Cores < 1 || o.Cores > 64:
		return fmt.Errorf("--cores %d: want 1 to 64", o.Cores)
	case o.MemoryMB < 256:
		return fmt.Errorf("--memory %d: want at least 256 MB", o.MemoryMB)
	case o.RootFSGB < 1 || o.StateGB < 1:
		return errors.New("the sizes of the disks are whole GiB, at least 1")
	case o.Template != "" && !filepath.IsAbs(o.Template):
		return fmt.Errorf("--template %s: give the file by its absolute path", o.Template)
	case o.InstallID != "" && !o.Recover:
		return errors.New("--install-id goes with --recover")
	case o.InstallID != "" && !installID.MatchString(o.InstallID):
		return fmt.Errorf("--install-id %q: want 12 lower-case hex characters", o.InstallID)
	case strings.ContainsFunc(o.CloudflareToken, func(r rune) bool { return r <= ' ' }):
		return errors.New("the Cloudflare token holds white space")
	}
	if err := store.ValidateTag(o.GateTag); err != nil {
		return fmt.Errorf("--gate-tag %q: %w", o.GateTag, err)
	}
	if _, _, err := parseIP(o.IP); err != nil {
		return err
	}
	return nil
}

// parseIP reads the address of net0: dhcp, or a CIDR with an optional
// gateway.
func parseIP(s string) (netip.Prefix, netip.Addr, error) {
	if s == DefaultIP {
		return netip.Prefix{}, netip.Addr{}, nil
	}
	addr, rest, _ := strings.Cut(s, ",")
	p, err := netip.ParsePrefix(addr)
	if err != nil || !p.Addr().Is4() || p.Bits() == 32 || p.Addr() == p.Masked().Addr() {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("--ip %q: want dhcp or an IPv4 address with its prefix, as 192.0.2.20/24[,gw=192.0.2.1]", s)
	}
	var gw netip.Addr
	if rest != "" {
		v, ok := strings.CutPrefix(rest, "gw=")
		if gw, err = netip.ParseAddr(v); !ok || err != nil || !gw.Is4() || !p.Contains(gw) || gw == p.Addr() {
			return netip.Prefix{}, netip.Addr{}, fmt.Errorf("--ip %q: the gateway must be another address of %s, as gw=%s", s, p.Masked(), p.Masked().Addr().Next())
		}
	}
	return p, gw, nil
}

// netSpec is the value of net0.
func (o Options) netSpec() string {
	spec := "name=eth0,bridge=" + o.Bridge
	if o.VLAN != 0 {
		spec += ",tag=" + strconv.Itoa(o.VLAN)
	}
	p, gw, _ := parseIP(o.IP)
	if !p.IsValid() {
		return spec + ",ip=dhcp"
	}
	spec += ",ip=" + p.String()
	if gw.IsValid() {
		spec += ",gw=" + gw.String()
	}
	return spec
}

// UninstallOptions are the answers to the questions of the uninstall given in
// advance, as those of pco uninstall: Yes answers whether to remove the
// appliance and nothing else; Cloudflare is answered by PurgeCloudflare or
// KeepCloudflare.
type UninstallOptions struct {
	Yes, PurgeCloudflare, KeepCloudflare bool
	KeepTemplate                         bool // the command line gives it true
	CloudflareAPI                        string
}

// Installer installs, repairs and removes appliances on this node.
type Installer struct {
	r    setup.Runner
	ask  setup.Prompter
	now  func() time.Time
	rand io.Reader
	dir  string
	h    host
}

// host is what the installer takes from the node besides its commands.
type host struct {
	euid        func() int
	hostname    func() (string, error)
	findCommand func(name string) error
	readFile    func(path string) ([]byte, error)
	pveDir      string // the mount point of the cluster filesystem
	clusterCA   string // the CA of the cluster, which signs pveproxy's certificate
	runDir      string // where the bootstrap is written for its push
	probeCert   func(ctx context.Context, address string) ([]*x509.Certificate, error)
	systemRoots func() (*x509.CertPool, error)
	signals     signalSource
	sleep       func(ctx context.Context, d time.Duration) error
	version     string // of this binary, which the template must hold
}

// New returns the installer of this node: it runs commands through r, asks
// p, draws the names of its runs from rand, and keeps its journals and its
// lock in journalDir.
func New(r setup.Runner, p setup.Prompter, now func() time.Time, rand io.Reader, journalDir string) *Installer {
	return &Installer{r: r, ask: p, now: now, rand: rand, dir: journalDir, h: nodeHost()}
}

func nodeHost() host {
	return host{
		euid:        os.Geteuid,
		hostname:    os.Hostname,
		findCommand: findCommand,
		readFile:    os.ReadFile,
		pveDir:      "/etc/pve",
		clusterCA:   "/etc/pve/pve-root-ca.pem",
		runDir:      "/run",
		probeCert:   probeCert,
		systemRoots: x509.SystemCertPool,
		signals:     osSignals{},
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
		version: version.Version,
	}
}

// findCommand looks a command up where the runner of setup looks.
func findCommand(name string) error {
	for _, dir := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		if _, err := exec.LookPath(filepath.Join(dir, name)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s: %w", name, setup.ErrCommandNotFound)
}

// run is the state of one install, repair or uninstall.
type run struct {
	*Installer
	o       Options
	j       *journal
	node    string
	version setup.PVEVersion
	arch    string
	addrs   []nodeAddr
	pvid    int         // of the bridge, once read: the VLAN of the node's own address on it
	data    access.Data // the access control, read in the preflight
	chosen  bool        // the VMID is the cluster's next free one, and another is taken when it is gone
	secret  store.Secret
	tmp     string // the directory of the bootstrap under /run
	unlock  func()
	resumed bool
	web     struct{ url, fingerprint string } // of the web interface, once started
}

func (i *Installer) newRun(o Options, kind string) *run {
	o.defaults()
	return &run{Installer: i, o: o, j: &journal{Kind: kind, Options: o}}
}

// The steps of an install, in order.
const (
	stepPreflight = "preflight"
	stepTemplate  = "template"
	stepPool      = "pool"
	stepContainer = "container"
	stepAccess    = "access"
	stepProxmox   = "proxmox"
	stepTags      = "tags"
	stepStart     = "start"
	stepListen    = "listen"
	stepPush      = "push"
	stepWeb       = "web"
	stepProtect   = "protection"
)

type step struct {
	name string
	do   func(ctx context.Context) error
}

func (r *run) installSteps() []step {
	return []step{
		{stepPreflight, r.preflight},
		{stepTemplate, r.prepareTemplate},
		{stepPool, r.ensurePool},
		{stepContainer, r.createContainer},
		{stepAccess, r.denyAccess},
		{stepProxmox, r.ensureProxmox},
		{stepTags, r.ensureTags},
		{stepStart, r.start},
		{stepListen, r.writeListen},
		{stepPush, r.push},
		{stepWeb, r.startWeb},
		{stepProtect, r.protect},
	}
}

// Install installs an appliance, or with Resume finishes the run of a
// journal. A step that fails, and a signal, take back what the run made; a
// run that cannot take everything back keeps its journal and says so.
func (i *Installer) Install(ctx context.Context, o Options) error {
	if o.Resume != "" {
		return i.resume(ctx, o)
	}
	o.defaults()
	if err := o.check(); err != nil {
		return err
	}
	unlock, err := i.lock()
	if err != nil {
		return err
	}
	defer unlock()
	r := i.newRun(o, kindInstall)
	return r.steps(ctx, r.installSteps())
}

// steps runs the steps not done yet, and takes the run back when one fails
// or a signal comes.
func (r *run) steps(ctx context.Context, steps []step) error {
	ctx, caught, stop := r.catchSignals(ctx)
	defer stop()
	for _, s := range steps {
		if r.skip(s.name) {
			continue
		}
		err := s.do(ctx)
		if sig := caught(); sig != nil {
			return r.fail(ctx, fmt.Errorf("interrupted by %v during step %s", sig, s.name))
		}
		if err != nil {
			return r.fail(ctx, fmt.Errorf("install step %s: %w", s.name, err))
		}
		if err := r.markDone(s.name); err != nil {
			return r.fail(ctx, err)
		}
	}
	if err := r.removeJournal(); err != nil {
		return err
	}
	r.summary()
	return nil
}

// skip says which steps a resumed run leaves out: those it finished, but for
// the token, whose secret no journal holds, and what leads to its push, which
// are done again until the push is done.
func (r *run) skip(name string) bool {
	if !r.j.done(name) {
		return false
	}
	switch name {
	case stepTemplate, stepPool, stepContainer, stepPush, stepProtect:
		return true
	}
	return r.j.done(stepPush)
}

func (r *run) summary() {
	vmid := r.j.VMID
	r.ask.Info("pco appliance lxc/%d is installed on %s, in pool %s", vmid, r.node, poolID)
	r.ask.Info("  token: %s", tokenID(vmid))
	r.ask.Info("  Proxmox API: %s, verified as %s against %s", r.j.Endpoint.Address, r.j.Endpoint.ServerName, r.j.Endpoint.Via)
	if r.web.fingerprint != "" {
		r.ask.Info("  web interface: %s, its certificate's SHA-256 fingerprint, which the browser shows at the first visit:", r.web.url)
		r.ask.Info("    %s", r.web.fingerprint)
	}
	r.ask.Info("next:")
	r.ask.Info("  pct exec %d -- pco status", vmid)
	r.ask.Info("  pct exec %d -- pco credential add --label <label>      (unless a Cloudflare token was given)", vmid)
	r.ask.Info("  tag a guest with %s and write its routes into its notes", r.o.GateTag)
	r.ask.Warn("snapshots, clones and storage replication of lxc/%d carry its secrets, the API token and the Cloudflare "+
		"credentials: whoever can read them can read those", vmid)
}
