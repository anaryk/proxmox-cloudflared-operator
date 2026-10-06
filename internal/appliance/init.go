package appliance

import (
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

const (
	// Unit is the unit of the daemon.
	Unit = "pco.service"

	manifestName    = "manifest.json"
	pendingName     = "init.pending" // names the install an init in mode install began, until the init is through
	credentialLabel = "setup"
	credentialKind  = "scoped"
	// checkTimeout bounds the two checks of a Cloudflare token, as setup's.
	checkTimeout = 3 * time.Minute
)

// RecoverInstallFunc is setup.RecoverInstall, which this package is handed
// rather than imports: setup imports the engine, which imports this package.
type RecoverInstallFunc func(ctx context.Context, api cfapi.API, st *store.Store, installID string,
	now func() time.Time, rand io.Reader, inst store.Install) (store.Install, int, error)

// InitDeps are what Init works with besides the store.
type InitDeps struct {
	// CheckToken reads the version of Proxmox VE with the token through the
	// endpoint, verified under its server name against the CA; CheckToken in
	// this package does.
	CheckToken  func(ctx context.Context, ep store.Endpoint, caFile string, tok store.PVEToken) error
	NewClient   func(token string) (cfapi.API, error)
	Systemd     connector.Systemd // restarts pco.service at the end
	Now         func() time.Time
	Rand        io.Reader
	Incarnation func() (string, error)
	Links       func() ([]NamedLink, error)  // the MACs seen now must contain the bootstrap's
	MountSource func(string) (Source, error) // must name the bootstrap's VMID, or Init refuses; an unknown form passes with a note
	// RecoverInstall adopts the install of a Cloudflare token in mode
	// recover: setup.RecoverInstall.
	RecoverInstall RecoverInstallFunc
	// Info says what a step did; nil says nothing.
	Info func(format string, args ...any)
}

// Init turns a bootstrap into the store. The file is removed as soon as it
// was read into memory. Every object is written through the store.
//
// Before anything is written, the bootstrap must be this container's: its
// MACs among the links, and its VMID the one the state volume names. Then,
// in order: the store, the settings, the writer with the incarnation of this
// start, the install with the appliance block, the Proxmox token, the node,
// the node addresses, the manifest, a read of Proxmox with the token, the
// Cloudflare credential and a restart of pco.service. A step that fails stops
// Init with its name in the error and leaves what was written, which the next
// init of the same mode completes. In mode install that holds past the install
// step too, as the install is marked begun until the last step: an init finds
// the install it began and completes it, and refuses one that is finished.
func Init(ctx context.Context, st *store.Store, b Bootstrap, deps InitDeps) error {
	r := &initRun{st: st, b: b, d: deps, local: st.Paths().Local}
	if r.d.Info == nil {
		r.d.Info = func(string, ...any) {}
	}
	for _, s := range r.steps() {
		if err := s.do(ctx); err != nil {
			return fmt.Errorf("init step %s: %w", s.name, err)
		}
	}
	return nil
}

// initRun is the state of one Init.
type initRun struct {
	st    *store.Store
	b     Bootstrap
	d     InitDeps
	local string

	install store.Install // the stored one, then the one this init stores
	found   bool          // the store held an install before this init
	resumed bool          // it is the install an earlier init in mode install began
	api     cfapi.API     // of the Cloudflare token, once made
}

type initStep struct {
	name string
	do   func(ctx context.Context) error
}

func (r *initRun) steps() []initStep {
	return []initStep{
		{"bootstrap", func(context.Context) error { return r.b.Remove() }},
		{"identity", r.checkIdentity},
		{"store", r.prepareStore},
		{"settings", r.saveSettings},
		{"writer", r.saveWriter},
		{"install", r.saveInstall},
		{"token", r.saveToken},
		{"node", r.registerNode},
		{"node addresses", r.saveNodeAddrs},
		{"manifest", r.saveManifest},
		{"proxmox", r.checkToken},
		{"credential", r.addCredential},
		{"service", r.restart},
		{"finish", r.finish},
	}
}

func (r *initRun) checkIdentity(context.Context) error { return CheckIdentity(r.b, r.local, r.d) }

// CheckIdentity refuses a bootstrap made for another container: its MACs must
// be among the links, and its VMID the one the state volume at local names, so
// that a copy of the bootstrap in the wrong container does nothing. The
// bootstrap that is refused is removed, so that its secrets do not stay in the
// wrong container.
func CheckIdentity(b Bootstrap, local string, deps InitDeps) error {
	err := checkContainer(b, local, deps)
	if err == nil {
		return nil
	}
	if rerr := b.Remove(); rerr != nil {
		return fmt.Errorf("%w (and %w)", err, rerr)
	}
	return err
}

func checkContainer(b Bootstrap, local string, deps InitDeps) error {
	links, err := deps.Links()
	if err != nil {
		return fmt.Errorf("listing the links of this container: %w", err)
	}
	var have []string
	for _, l := range links {
		have = append(have, l.MAC)
	}
	for _, mac := range b.MACs {
		if !slices.Contains(have, mac) {
			return fmt.Errorf("the bootstrap names MAC %s, which no link of this container has (it has %s): "+
				"it was pushed into another container", mac, cmp.Or(strings.Join(have, ", "), "none"))
		}
	}
	src, err := deps.MountSource(local)
	if err != nil {
		return fmt.Errorf("reading the mount of %s: %w", local, err)
	}
	switch src.VMID {
	case b.VMID:
		deps.say("identity: %s is a volume of lxc/%d (%s)", local, src.VMID, src.Volume)
	case 0:
		deps.say("identity: the source of %s (%s) is of no form pco knows; it is taken for lxc/%d, "+
			"which the installer pushed the bootstrap into", local, src.Raw, b.VMID)
	default:
		return fmt.Errorf("%s is a volume of lxc/%d (%s), but the bootstrap is for lxc/%d: it was pushed into another container",
			local, src.VMID, src.Volume, b.VMID)
	}
	return nil
}

// say passes on what a step did, if anyone listens.
func (d InitDeps) say(format string, args ...any) {
	if d.Info != nil {
		d.Info(format, args...)
	}
}

// ReadyForInit makes ready for Init what lies outside the store, touching
// nothing until the bootstrap is known to be this container's: CheckIdentity
// first, then the daemon, which in mode install must not run, as init starts
// it itself, and in the other modes is stopped, which stopped reports it was
// running. A bootstrap that is turned away because of the daemon is kept, so
// that the init the message asks for has it; Init is what removes it.
func (d Daemon) ReadyForInit(ctx context.Context, b Bootstrap, local string, deps InitDeps) (stopped bool, err error) {
	deps.Info = nil
	if err := CheckIdentity(b, local, deps); err != nil {
		return false, err
	}
	if b.Mode != ModeInstall {
		return d.Stop(ctx)
	}
	running, err := d.Running()
	switch {
	case err != nil:
		return false, err
	case running:
		return false, fmt.Errorf("a pco daemon runs (it holds %s), and an init in mode install starts it itself: "+
			"stop %s first and run init again: the bootstrap is kept", d.Lock, Unit)
	}
	return false, nil
}

func (r *initRun) prepareStore(context.Context) error {
	if err := requireDurable(r.st); err != nil {
		return err
	}
	if err := r.st.Init(); err != nil {
		return fmt.Errorf("creating the store: %w", err)
	}
	inst, found, err := r.st.Install()
	if err != nil {
		return fmt.Errorf("reading the install: %w", err)
	}
	switch {
	case r.b.Mode == ModeInstall && found:
		began, err := r.pendingInstall()
		if err != nil {
			return err
		}
		if began != inst.ID {
			return fmt.Errorf("the volume holds install %s already, and mode install never replaces one: "+
				"pco appliance repair --vmid %d on the node keeps it", inst.ID, r.b.VMID)
		}
		r.resumed = true
		r.d.Info("install %s was begun by an init that did not finish, and this one completes it", inst.ID)
	case r.b.Mode == ModeRepair && !found:
		return fmt.Errorf("the volume holds no install to repair: pco appliance repair --vmid %d --recover on the node adopts one", r.b.VMID)
	case r.b.Mode != ModeInstall && found && r.b.InstallID != "" && inst.ID != r.b.InstallID:
		return fmt.Errorf("the volume holds install %s, not %s", inst.ID, r.b.InstallID)
	}
	r.install, r.found = inst, found
	return nil
}

// requireDurable refuses a store whose writes do not survive a power cut: the
// epoch of the writer must.
func requireDurable(st *store.Store) error {
	if !st.Paths().Durable {
		return errors.New("the store of the appliance must write durably, and this one does not")
	}
	return nil
}

// saveSettings writes the settings of an appliance into a store that holds
// no install yet; those of an install are its own.
func (r *initRun) saveSettings(context.Context) error {
	if r.found {
		r.d.Info("settings: those of install %s are kept", r.install.ID)
		kept, err := r.st.Settings()
		switch {
		case err != nil:
			r.d.Info("settings: the gate tag could not be compared with the bootstrap's: %v", err)
		case kept.GateTag != r.b.GateTag:
			r.d.Info("settings: the gate tag stays %s, and the bootstrap names %s: to change it, set gateTag in the file "+
				"of pco settings show --json and apply it with pco settings apply", kept.GateTag, r.b.GateTag)
		}
		return nil
	}
	s := store.DefaultSettings()
	s.IdentityMinimum = string(resolve.LevelObserved)
	s.ObserveOnly = true
	s.GateTag = r.b.GateTag
	if err := r.st.SaveSettings(s); err != nil {
		return fmt.Errorf("storing the settings: %w", err)
	}
	r.d.Info("settings: identity minimum %s, gate tag %s; the install only observes until pco apply", s.IdentityMinimum, s.GateTag)
	return nil
}

// saveWriter writes the writer: generation 1 of a new install, or one above
// the sentinels of the adopted one, with the incarnation of this start. A
// repair, and an install that an earlier init began, keep the writer as it is:
// the daemon draws a new epoch when it belongs to an earlier start.
func (r *initRun) saveWriter(ctx context.Context) error {
	if r.b.Mode == ModeRepair || r.resumed {
		w, found, err := r.st.Writer()
		switch {
		case err != nil:
			return fmt.Errorf("reading leader.json: %w", err)
		case !found || w.InstallID != r.install.ID:
			r.d.Info("writer: leader.json holds no writer of install %s; once a credential is stored, pco appliance recover draws one", r.install.ID)
		default:
			r.d.Info("writer: generation %d kept", w.Generation)
		}
		return nil
	}
	incarnation, err := r.d.Incarnation()
	if err != nil {
		return fmt.Errorf("reading the incarnation of this container: %w", err)
	}
	if r.b.Mode == ModeRecover {
		return r.recoverWriter(ctx, incarnation)
	}
	id, err := randomHex(r.d.Rand, 6)
	if err != nil {
		return err
	}
	nonce, err := newNonce(r.d.Rand)
	if err != nil {
		return err
	}
	if err := r.markPending(id); err != nil {
		return err
	}
	if err := r.st.SaveWriter(planner.Writer{InstallID: id, Generation: 1, Nonce: nonce, Incarnation: incarnation}); err != nil {
		return fmt.Errorf("storing the writer identity: %w", err)
	}
	r.install = store.Install{ID: id, CreatedAt: r.d.Now()}
	r.d.Info("store: created install %s, writer generation 1", id)
	return nil
}

func (r *initRun) recoverWriter(ctx context.Context, incarnation string) error {
	api, err := r.client()
	if err != nil {
		return err
	}
	inst, generation, err := r.d.RecoverInstall(ctx, api, r.st, r.b.InstallID, r.d.Now, r.d.Rand, r.applianceInstall(store.Install{}))
	if err != nil {
		return err
	}
	if err := withIncarnation(r.st, incarnation); err != nil {
		return err
	}
	r.install = inst
	r.d.Info("store: recovered install %s with writer generation %d", inst.ID, generation)
	return nil
}

// markPending records the install this init begins, before any object of it
// is stored, so that an init that stops past that point can be told from one
// that finished: the next init in mode install completes the first and
// refuses the second.
func (r *initRun) markPending(id string) error {
	err := atomicfile.Write(filepath.Join(r.local, pendingName), []byte(id+"\n"), atomicfile.Options{Mode: 0o600})
	if err != nil {
		return fmt.Errorf("marking install %s as begun: %w", id, err)
	}
	return nil
}

// pendingInstall returns the id of the install an init began and did not
// finish, empty when none is marked.
func (r *initRun) pendingInstall() (string, error) {
	data, err := os.ReadFile(filepath.Join(r.local, pendingName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("reading %s: %w", pendingName, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// finish is the last step: the install is complete, in whichever mode it was
// completed, and is no longer marked as begun.
func (r *initRun) finish(context.Context) error {
	if err := os.Remove(filepath.Join(r.local, pendingName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", pendingName, err)
	}
	return nil
}

// withIncarnation binds the stored writer to the incarnation of this start,
// so that the daemon keeps the epoch instead of drawing another one.
func withIncarnation(st *store.Store, incarnation string) error {
	w, found, err := st.Writer()
	switch {
	case err != nil:
		return fmt.Errorf("reading leader.json: %w", err)
	case !found:
		return errors.New("leader.json is gone after it was written")
	}
	w.Incarnation = incarnation
	if err := st.SaveWriter(w); err != nil {
		return fmt.Errorf("storing the writer identity: %w", err)
	}
	return nil
}

// applianceInstall is inst as the install of this appliance.
func (r *initRun) applianceInstall(inst store.Install) store.Install {
	inst.Profile = store.ProfileAppliance
	inst.Appliance = &store.ApplianceInstall{
		VMID:      r.b.VMID,
		Node:      r.b.Node,
		MACs:      slices.Clone(r.b.MACs),
		Endpoints: slices.Clone(r.b.Endpoints),
		CAFile:    filepath.Join(r.local, CAFile),
	}
	return inst
}

func (r *initRun) saveInstall(context.Context) error {
	r.install = r.applianceInstall(r.install)
	if err := r.st.SaveInstall(r.install); err != nil {
		return fmt.Errorf("storing the install: %w", err)
	}
	r.d.Info("install: %s, lxc/%d on %s", r.install.ID, r.b.VMID, r.b.Node)
	return nil
}

func (r *initRun) saveToken(context.Context) error {
	if err := r.st.SavePVEToken(r.b.PVEToken); err != nil {
		return fmt.Errorf("storing the Proxmox token: %w", err)
	}
	r.d.Info("proxmox token: %s stored", r.b.PVEToken.TokenID)
	return nil
}

func (r *initRun) registerNode(context.Context) error {
	nodes, err := r.st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	entry := store.NodeEntry{Name: r.b.Node, Version: version.Version, Since: r.d.Now()}
	if i := slices.IndexFunc(nodes, func(n store.NodeEntry) bool { return n.Name == r.b.Node }); i >= 0 {
		entry.Since = nodes[i].Since
	}
	if err := r.st.SaveNode(entry); err != nil {
		return fmt.Errorf("registering node %s: %w", r.b.Node, err)
	}
	return nil
}

// saveNodeAddrs adds the addresses the installer read on the node to the
// saved ones, which are never forgotten: an address the node holds only at
// run time is in no answer of the API (ruling 26).
func (r *initRun) saveNodeAddrs(context.Context) error {
	saved, err := r.st.NodeAddrs()
	if err != nil {
		return fmt.Errorf("reading the node addresses: %w", err)
	}
	if err := r.st.SaveNodeAddrs(append(saved, r.b.NodeAddrs...)); err != nil {
		return fmt.Errorf("storing the node addresses: %w", err)
	}
	return nil
}

func (r *initRun) saveManifest(context.Context) error {
	var out bytes.Buffer
	if err := json.Indent(&out, r.b.Manifest, "", "  "); err != nil {
		return fmt.Errorf("the manifest: %w", err)
	}
	out.WriteByte('\n')
	if err := atomicfile.Write(filepath.Join(r.local, manifestName), out.Bytes(), atomicfile.Options{Mode: 0o600}); err != nil {
		return fmt.Errorf("writing the manifest: %w", err)
	}
	return nil
}

func (r *initRun) checkToken(ctx context.Context) error {
	ep := r.b.Endpoints[0]
	err := r.d.CheckToken(ctx, ep, filepath.Join(r.local, CAFile), r.b.PVEToken)
	var apiErr *pve.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized:
		return errors.New("the token is refused by Proxmox")
	case err != nil:
		return fmt.Errorf("reading the version of Proxmox VE at %s as %s: %w", ep.Address, ep.ServerName, err)
	}
	r.d.Info("proxmox: the token reads the API at %s, verified as %s", ep.Address, ep.ServerName)
	return nil
}

// CheckToken reads the version of Proxmox VE with a token, through the
// endpoint, with the certificate verified under its server name against the
// CA in caFile and the system roots.
func CheckToken(ctx context.Context, ep store.Endpoint, caFile string, tok store.PVEToken) error {
	c, err := pve.New(pve.Config{
		BaseURL: "https://" + ep.Address, TokenID: tok.TokenID, Secret: tok.Secret.Reveal(),
		CAFile: caFile, ServerName: ep.ServerName,
	})
	if err != nil {
		return err
	}
	_, err = c.Version(ctx)
	return err
}

// client returns the client of the Cloudflare token of the bootstrap.
func (r *initRun) client() (cfapi.API, error) {
	if r.api != nil {
		return r.api, nil
	}
	api, err := r.d.NewClient(r.b.CloudflareToken.Reveal())
	if err != nil {
		return nil, fmt.Errorf("the Cloudflare token cannot be used: %w", err)
	}
	r.api = api
	return api, nil
}

// addCredential checks the Cloudflare token as setup does, shallow and then
// deep, and stores it only when it can do what pco needs. A token that
// cannot is no failure: the daemon idles until one is added.
func (r *initRun) addCredential(ctx context.Context) error {
	token := r.b.CloudflareToken
	if token.Reveal() == "" {
		r.d.Info("credentials: the bootstrap holds no Cloudflare token; add one with pco credential add")
		return nil
	}
	creds, err := r.st.Credentials()
	if err != nil {
		return fmt.Errorf("reading the credentials: %w", err)
	}
	if slices.ContainsFunc(creds, func(c store.Credential) bool { return c.Token.Equal(token) }) {
		r.d.Info("credentials: the token is stored already, nothing needed")
		return nil
	}
	api, err := r.client()
	if err != nil {
		r.d.Info("credentials: %v; it is not stored, add one with pco credential add", err)
		return nil
	}
	report, err := r.check(ctx, api)
	if err != nil {
		return err
	}
	switch {
	case report.Unanswered():
		r.d.Info("credentials: Cloudflare did not answer every check, so the token is not stored; add it with pco credential add")
		return nil
	case !report.Usable:
		r.d.Info("credentials: the token cannot do what pco needs, so it is not stored; grant what is missing and add it with pco credential add")
		return nil
	}
	cred, err := r.newCredential(creds, token)
	if err != nil {
		return err
	}
	if err := r.st.SaveCredential(cred); err != nil {
		return fmt.Errorf("storing the credential: %w", err)
	}
	r.d.Info("credentials: stored the token as credential %s (%s)", cred.ID, cred.Label)
	return nil
}

func (r *initRun) check(ctx context.Context, api cfapi.API) (credentials.Report, error) {
	suffix, err := randomHex(r.d.Rand, 8)
	if err != nil {
		return credentials.Report{}, err
	}
	checker := credentials.NewChecker(r.install.ID, r.d.Now, func() string { return suffix })
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	report := checker.Run(ctx, api, false)
	if report.Usable {
		r.d.Info("credentials: checking write access with a test DNS record and a test tunnel, which are removed again")
		report = checker.Run(ctx, api, true)
	}
	if err := ctx.Err(); err != nil {
		return credentials.Report{}, fmt.Errorf("checking the token: %w", err)
	}
	for _, c := range report.Checks {
		reason := c.Reason()
		if c.OK || reason == "" {
			continue
		}
		what := string(c.Capability)
		if c.Scope != "" {
			what += " on " + c.Scope
		}
		r.d.Info("  %s: %s", what, reason)
	}
	for _, name := range report.Leftovers {
		r.d.Info("a probe record of an earlier check is left in Cloudflare, to be removed by hand: %s", name)
	}
	return report, nil
}

// newCredential is the credential of a token that passed its check, with an
// id of 8 random hex characters no stored one has in any case.
func (r *initRun) newCredential(stored []store.Credential, token store.Secret) (store.Credential, error) {
	for {
		id, err := randomHex(r.d.Rand, 4)
		if err != nil {
			return store.Credential{}, err
		}
		if !slices.ContainsFunc(stored, func(c store.Credential) bool { return strings.EqualFold(c.ID, id) }) {
			return store.Credential{ID: id, Label: credentialLabel, Kind: credentialKind, Token: token, AddedAt: r.d.Now()}, nil
		}
	}
}

func (r *initRun) restart(ctx context.Context) error {
	if err := r.d.Systemd.Restart(ctx, Unit); err != nil {
		return fmt.Errorf("restarting %s: %w", Unit, err)
	}
	r.d.Info("%s: restarted", Unit)
	return nil
}

// randomHex returns 2n random lower-case hex characters.
func randomHex(rand io.Reader, n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand, b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
