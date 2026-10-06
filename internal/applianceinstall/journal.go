package applianceinstall

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// The kinds of run a journal is of.
const (
	kindInstall = "install"
	kindRepair  = "repair"
	kindGrant   = "grant-network"
)

const maxJournal = 1 << 20

// journal is what a run made and decided, written before every create, so
// that a run killed at any point can be finished or taken back. It never
// holds a secret: it names the token, whose secret only the container gets.
type journal struct {
	Run     string  `json:"run"`
	Kind    string  `json:"kind"`
	Node    string  `json:"node"`
	VMID    int     `json:"vmid"`
	Options Options `json:"options"`
	// Endpoint is how the appliance reaches the API, chosen in the
	// preflight.
	Endpoint apiEndpoint `json:"endpoint"`
	// Template is what pct create takes: a volume or a file.
	Template string `json:"template,omitempty"`
	// Container is the description of the container the run made, the mark
	// by which it is told from one of the same VMID that is not its own.
	Container string `json:"container,omitempty"`
	// Denials are the NoAccess lines the admin confirmed; the manifest names
	// those added.
	Denials []denial `json:"denials,omitempty"`
	// AddedMP0 says a repair gave the container its state volume.
	AddedMP0 bool `json:"addedMP0,omitempty"`
	// Grant is the network of a grant-network run.
	Grant *setup.NetworkGrant `json:"grant,omitempty"`
	// Manifest is what the run made of the objects in the manifest of the
	// appliance, which the bootstrap carries into it.
	Manifest setup.Manifest `json:"manifest"`
	// CloudflareToken says the run was given one, which a resumed run must
	// be given again.
	CloudflareToken bool     `json:"cloudflareToken"`
	Done            []string `json:"done"`
	// TakingBack says the run failed and what it made is being taken back:
	// a resumed run finishes that, never the install.
	TakingBack bool `json:"takingBack,omitempty"`

	path string
}

func (j *journal) done(name string) bool { return slices.Contains(j.Done, name) }

// record notes what is about to be made and writes the journal, before the
// command that makes it.
func (r *run) record(change func(j *journal)) error {
	change(r.j)
	return r.save()
}

// recordManifest is record for the objects of the manifest, as the shared
// helpers of setup note them.
func (r *run) recordManifest(change func(*setup.Manifest)) error {
	return r.record(func(j *journal) { change(&j.Manifest) })
}

func (r *run) markDone(name string) error {
	if !r.j.done(name) {
		r.j.Done = append(r.j.Done, name)
	}
	if r.j.path == "" {
		// Nothing was made yet that a journal would have to remember.
		return nil
	}
	return r.save()
}

func (r *run) save() error {
	if r.j.path == "" {
		if r.j.Run == "" {
			id, err := r.runID()
			if err != nil {
				return err
			}
			r.j.Run = id
		}
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			return fmt.Errorf("making %s: %w", r.dir, err)
		}
		r.j.path = filepath.Join(r.dir, r.j.Run+".json")
	}
	r.j.Node = r.node
	r.j.CloudflareToken = r.o.CloudflareToken != ""
	if r.j.Manifest.Node == "" {
		r.j.Manifest.Node = r.node
	}
	if r.j.Manifest.InstalledAt.IsZero() {
		r.j.Manifest.InstalledAt = r.now()
	}
	b, err := json.MarshalIndent(r.j, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicfile.Write(r.j.path, append(b, '\n'), atomicfile.Options{Mode: 0o600}); err != nil {
		return fmt.Errorf("writing the journal: %w", err)
	}
	return nil
}

// runID names a run by its start and two random bytes.
func (r *run) runID() (string, error) {
	b := make([]byte, 2)
	if _, err := io.ReadFull(r.rand, b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return r.now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b), nil
}

func (r *run) removeJournal() error {
	if r.j.path == "" {
		return nil
	}
	if err := os.Remove(r.j.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the journal: %w", err)
	}
	r.j.path = ""
	return nil
}

func readJournal(path string) (*journal, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxJournal+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("reading the journal %s: %w", path, err)
	case len(b) > maxJournal:
		return nil, fmt.Errorf("the journal %s is larger than a journal is", path)
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("the journal %s: %w", path, err)
	}
	switch j.Kind {
	case kindInstall, kindRepair, kindGrant:
	default:
		return nil, fmt.Errorf("%s is not a journal of pco appliance", path)
	}
	j.path = path
	return &j, nil
}

// lock takes the lock of the installer, which a second installer finds taken
// and is refused by, and sweeps what runs killed outright left under /run.
// The lock file goes with the lock, so that the journal directory is empty
// after a run that succeeded.
func (i *Installer) lock() (unlock func(), err error) {
	unlock, err = i.takeLock()
	if err != nil {
		return nil, err
	}
	i.sweep()
	return unlock, nil
}

// sweep removes the directories of runs under /run, the bootstrap with its
// secrets in one maybe: with the lock taken, none is a live run's.
func (i *Installer) sweep() {
	dirs, err := filepath.Glob(filepath.Join(i.h.runDir, "pco-appliance-install-*"))
	if err != nil {
		return
	}
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			i.ask.Warn("removing %s, which a run that was cut short left: %v", dir, err)
			continue
		}
		i.ask.Warn("removed %s, which a run that was cut short left", dir)
	}
}

func (i *Installer) takeLock() (unlock func(), err error) {
	if err := os.MkdirAll(i.dir, 0o700); err != nil {
		return nil, fmt.Errorf("making %s: %w", i.dir, err)
	}
	path := filepath.Join(i.dir, "lock")
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return nil, fmt.Errorf("opening the lock of the installer: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("another pco appliance installer runs on this node (it holds %s); wait for it to finish", path)
			}
			return nil, fmt.Errorf("taking the lock of the installer: %w", err)
		}
		// A lock whose file was removed by the run that held it is no lock:
		// take the one that is there now.
		held, err1 := f.Stat()
		there, err2 := os.Stat(path)
		if err1 == nil && err2 == nil && os.SameFile(held, there) {
			return func() {
				_ = os.Remove(path)
				_ = f.Close()
			}, nil
		}
		_ = f.Close()
	}
}

// signalSource delivers the signals of the operating system.
type signalSource interface {
	Notify(c chan<- os.Signal, sig ...os.Signal)
	Stop(c chan<- os.Signal)
}

type osSignals struct{}

func (osSignals) Notify(c chan<- os.Signal, sig ...os.Signal) { signal.Notify(c, sig...) }
func (osSignals) Stop(c chan<- os.Signal)                     { signal.Stop(c) }

// catchSignals ends ctx on SIGINT, SIGTERM or SIGHUP and says which came
// first. The signals stay caught until stop, so that a second one does not cut
// the taking back short.
func (r *run) catchSignals(ctx context.Context) (context.Context, func() os.Signal, func()) {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan os.Signal, 1)
	r.h.signals.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	var (
		mu   sync.Mutex
		got  os.Signal
		done = make(chan struct{})
	)
	go func() {
		for {
			select {
			case sig := <-ch:
				mu.Lock()
				if got == nil {
					got = sig
				}
				mu.Unlock()
				cancel()
			case <-done:
				return
			}
		}
	}()
	caught := func() os.Signal {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
	return ctx, caught, func() {
		r.h.signals.Stop(ch)
		close(done)
		cancel()
	}
}

// rollback takes back what the journal says the run made, the container
// first, as it may hold the secrets by then. What is gone already is
// skipped, and what does not carry the run's mark is left as it is.
func (r *run) rollback(ctx context.Context) error {
	if r.j.Kind == kindGrant {
		return r.rollbackGrant(ctx)
	}
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
			r.ask.Warn("%v", err)
		}
	}
	vmid, m := r.j.VMID, r.j.Manifest
	if r.j.Container != "" {
		note(r.takeBackContainer(ctx, vmid))
	}
	us, err := users(ctx, r.r)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	toks, err := tokens(ctx, r.r, us)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if m.CreatedToken && slices.ContainsFunc(toks, func(t tokenEntry) bool { return t.Name == tokenName(vmid) && t.Comment == marker(vmid) }) {
		// A user that goes leaves the secrets of its tokens behind: one whose
		// token stays, stays too.
		if err := r.removeToken(ctx, vmid); err != nil {
			note(err)
		} else {
			toks = slices.DeleteFunc(toks, func(t tokenEntry) bool { return t.Name == tokenName(vmid) })
		}
	}
	acl, err := aclLines(ctx, r.r)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if a := m.Appliance; a != nil {
		for _, n := range a.NoAccess {
			if line := noAccessLine(n); slices.Contains(acl, line) {
				note(r.deleteLine(ctx, line))
			}
		}
	}
	if len(m.RegisteredTags) > 0 {
		note(r.removeTags(ctx, m.RegisteredTags))
	}
	note(r.takeBackUser(ctx, m, toks, acl))
	if m.CreatedRole {
		if acl, err := aclLines(ctx, r.r); err != nil {
			note(err)
		} else if !slices.ContainsFunc(acl, func(l aclLine) bool { return l.Role == setup.RoleID }) {
			note(r.removeObject(ctx, "role "+setup.RoleID, "pveum", "role", "delete", setup.RoleID))
		}
	}
	if a := m.Appliance; a != nil && a.CreatedPool {
		if members, err := poolMembers(ctx, r.r, poolID); err != nil && !notThere(err) {
			note(err)
		} else if err == nil && len(members) == 0 {
			note(r.removeObject(ctx, "pool "+poolID, "pveum", "pool", "delete", poolID))
		}
	}
	if a := m.Appliance; a != nil && a.Template != "" && !r.o.KeepTemplate {
		note(r.removeObject(ctx, "template "+a.Template, "pvesm", "free", a.Template))
	}
	return errors.Join(errs...)
}

func (r *run) takeBackContainer(ctx context.Context, vmid int) error {
	cfg, err := readCTConfig(ctx, r.r, r.node, vmid)
	switch {
	case err != nil && notThere(err):
		return nil
	case err != nil:
		return fmt.Errorf("reading the configuration of lxc/%d: %w", vmid, err)
	case strings.TrimSpace(cfg["description"]) != r.j.Container:
		r.ask.Warn("lxc/%d is not the container this run made: it is left as it is", vmid)
		return nil
	}
	if err := r.unlockCreate(ctx, vmid, cfg); err != nil {
		return err
	}
	running, err := ctRunning(ctx, r.r, vmid)
	if err != nil {
		return err
	}
	return r.destroyContainer(ctx, vmid, running)
}

// unlockCreate clears the lock a pct create cut short left on the container
// the run made, as its description proves: Proxmox changes, starts and
// destroys no locked container.
func (r *run) unlockCreate(ctx context.Context, vmid int, cfg ctConfig) error {
	if cfg["lock"] != "create" {
		return nil
	}
	if _, err := r.r.Run(ctx, "pct", "unlock", strconv.Itoa(vmid)); err != nil {
		return fmt.Errorf("unlocking lxc/%d, which a pct create that was cut short left locked: %w", vmid, err)
	}
	r.ask.Warn("lxc/%d was left locked by a pct create that was cut short: unlocked", vmid)
	return nil
}

// takeBackUser removes the user the run made, or the grant it made to a user
// that was there, unless another token of the user needs it.
func (r *run) takeBackUser(ctx context.Context, m setup.Manifest, toks []tokenEntry, acl []aclLine) error {
	grant := aclLine{Path: "/", Type: "user", UGID: setup.UserID, Role: setup.RoleID}
	other := slices.ContainsFunc(acl, func(l aclLine) bool { return l.Type == "user" && l.UGID == setup.UserID && l != grant })
	switch {
	case len(toks) > 0:
		return nil
	case m.CreatedUser && !other:
		return r.removeObject(ctx, "user "+setup.UserID, "pveum", "user", "delete", setup.UserID)
	case m.GrantedACL && slices.Contains(acl, grant):
		return r.deleteLine(ctx, grant)
	}
	return nil
}

// resume finishes the run of a journal: what it finished stays, the token is
// made anew (no journal holds its secret), and the rest is done. A step that
// fails takes the whole run back.
func (i *Installer) resume(ctx context.Context, o Options) error {
	unlock, err := i.lock()
	if err != nil {
		return err
	}
	defer unlock()
	j, err := readJournal(o.Resume)
	if err != nil {
		return err
	}
	switch j.Kind {
	case kindRepair:
		if err := os.Remove(j.path); err != nil {
			return err
		}
		return fmt.Errorf("the journal %s is of a repair, which takes nothing back: run pco appliance repair --vmid %d again", o.Resume, j.VMID)
	case kindGrant:
		j.TakingBack = true
	}
	if j.TakingBack {
		r := i.newRun(j.Options, j.Kind)
		r.j, r.node = j, j.Node
		if err := r.whoami(); err != nil {
			return err
		}
		if err := r.rollback(ctx); err != nil {
			return fmt.Errorf("taking back the run of %s: %w", o.Resume, err)
		}
		r.ask.Info("what the run of %s made is taken back", o.Resume)
		return r.removeJournal()
	}
	if j.CloudflareToken && o.CloudflareToken == "" {
		return fmt.Errorf("the run of %s was given a Cloudflare token, which no journal holds: pass it again with --cf-token-file", o.Resume)
	}
	opts := j.Options
	opts.Yes, opts.CloudflareToken, opts.CloudflareAPI = o.Yes, o.CloudflareToken, o.CloudflareAPI
	r := i.newRun(opts, kindInstall)
	r.j, r.node, r.resumed = j, j.Node, true
	r.j.Options = opts
	r.ask.Info("resuming the install of lxc/%d from %s (done: %s)", j.VMID, o.Resume, orNone(j.Done))
	steps := r.installSteps()
	steps[0] = step{stepPreflight, r.node0}
	return r.steps(ctx, steps)
}

// fail takes back what the run made and returns why it failed. A run that
// cannot take everything back keeps its journal for --resume.
func (r *run) fail(ctx context.Context, cause error) error {
	if r.j.path == "" {
		return fmt.Errorf("%w; nothing was changed", cause)
	}
	ctx = context.WithoutCancel(ctx)
	r.ask.Warn("%v; taking back what this run made", cause)
	if err := r.record(func(j *journal) { j.TakingBack = true }); err != nil {
		r.ask.Warn("%v", err)
	}
	if err := r.rollback(ctx); err != nil {
		return fmt.Errorf("%w; taking back what the run made failed (%w): the journal %s names what is left, "+
			"and pco appliance install --resume %s takes back the rest", cause, err, r.j.path, r.j.path)
	}
	if err := r.removeJournal(); err != nil {
		r.ask.Warn("%v", err)
	}
	return fmt.Errorf("%w; what the run made is taken back", cause)
}
