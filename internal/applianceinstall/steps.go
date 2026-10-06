package applianceinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// maxVMIDTries is how often a VMID the cluster offered is given up for the
// next one, when another took it before pct create did.
const maxVMIDTries = 3

func (r *run) appliance() *setup.ApplianceManifest {
	if r.j.Manifest.Appliance == nil {
		r.j.Manifest.Appliance = &setup.ApplianceManifest{Pool: poolID}
	}
	return r.j.Manifest.Appliance
}

func (r *run) ensurePool(ctx context.Context) error {
	all, err := pools(ctx, r.r)
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(all, func(p poolEntry) bool { return p.ID == poolID }); i >= 0 {
		if all[i].Comment != poolComment {
			r.ask.Warn("pool %s is there without the comment %q: the appliance goes into it, and uninstall leaves it", poolID, poolComment)
		}
		r.ask.Info("pool %s: there, nothing needed", poolID)
		return nil
	}
	if err := r.record(func(j *journal) { r.appliance().CreatedPool = true }); err != nil {
		return err
	}
	if _, err := r.r.Run(ctx, "pveum", "pool", "add", poolID, "--comment", poolComment); err != nil {
		if all, lerr := pools(ctx, r.r); lerr == nil && !slices.ContainsFunc(all, func(p poolEntry) bool { return p.ID == poolID }) {
			_ = r.record(func(j *journal) { r.appliance().CreatedPool = false })
		}
		return fmt.Errorf("creating pool %s: %w", poolID, err)
	}
	r.ask.Info("pool %s: created", poolID)
	return nil
}

func (r *run) createArgs(vmid int, desc string) []string {
	return []string{"create", strconv.Itoa(vmid), r.j.Template,
		"--unprivileged", "1",
		"--features", "nesting=1",
		"--ostype", "debian",
		"--hostname", ctHostname,
		"--cores", strconv.Itoa(r.o.Cores),
		"--memory", strconv.Itoa(r.o.MemoryMB),
		"--swap", "0",
		"--rootfs", r.o.Storage + ":" + strconv.Itoa(r.o.RootFSGB),
		"--mp0", r.mp0(),
		"--net0", r.o.netSpec(),
		"--onboot", "1",
		"--startup", "order=1,up=20",
		"--pool", poolID,
		"--tags", ctTags,
		"--description", desc,
	}
}

// mp0 is the state volume, kept out of backups: what is on it is restored by
// pco appliance repair, never from a backup that would carry its secrets.
func (r *run) mp0() string {
	return r.o.Storage + ":" + strconv.Itoa(r.o.StateGB) + ",mp=" + stateDir + ",backup=0"
}

// createContainer makes the container. When the cluster offered the VMID and
// another took it in the meantime, the next one is taken.
func (r *run) createContainer(ctx context.Context) error {
	for try := 1; ; try++ {
		vmid, desc := r.j.VMID, description(r.j.VMID, r.now())
		if err := r.record(func(j *journal) {
			j.Container = desc
			a := r.appliance()
			a.VMID, a.Node, a.Token = vmid, r.node, tokenID(vmid)
		}); err != nil {
			return err
		}
		_, err := r.r.Run(ctx, "pct", r.createArgs(vmid, desc)...)
		if err == nil {
			r.ask.Info("container lxc/%d: created on %s, %s", vmid, r.o.Storage, r.o.netSpec())
			return nil
		}
		if r.ours(ctx, vmid) {
			return fmt.Errorf("creating lxc/%d: %w", vmid, err)
		}
		if uerr := r.record(func(j *journal) { j.Container = "" }); uerr != nil {
			return uerr
		}
		if !r.chosen || try == maxVMIDTries || !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("creating lxc/%d: %w", vmid, err)
		}
		r.ask.Warn("VMID %d was taken before the container was made; taking the next free one", vmid)
		if err := r.nextVMID(ctx); err != nil {
			return err
		}
	}
}

// nextVMID takes the next free VMID instead of one that was taken, and asks
// again about the principals that could reach into it.
func (r *run) nextVMID(ctx context.Context) error {
	vmid, err := nextID(ctx, r.r)
	if err != nil {
		return err
	}
	if guestExists(ctx, r.r, vmid) {
		return fmt.Errorf("the cluster offered VMID %d, which is taken", vmid)
	}
	r.j.VMID = vmid
	r.j.Denials = denials(r.data, vmid)
	return r.confirmDenials()
}

// ours reports whether the container vmid is the one this run made, by the
// description it was made with.
func (r *run) ours(ctx context.Context, vmid int) bool {
	cfg, err := readCTConfig(ctx, r.r, r.node, vmid)
	return err == nil && r.j.Container != "" && strings.TrimSpace(cfg["description"]) == r.j.Container
}

// denyAccess adds the NoAccess lines the admin confirmed and asks Proxmox
// whether they hold: its answer is what counts, not pco's computation. Each
// line is in the manifest before it is added, as uninstall takes it back.
func (r *run) denyAccess(ctx context.Context) error {
	for _, d := range r.j.Denials {
		line := setup.NoAccessLine{Principal: d.Who, Path: d.Path, Role: roleNoAccess}
		if err := r.record(func(*journal) {
			if a := r.appliance(); !slices.Contains(a.NoAccess, line) {
				a.NoAccess = append(a.NoAccess, line)
			}
		}); err != nil {
			return err
		}
		if _, err := r.r.Run(ctx, "pveum", "acl", "modify", d.Path, d.Flag, d.Who, "--roles", roleNoAccess); err != nil {
			return fmt.Errorf("adding NoAccess for %s on %s: %w", d.Who, d.Path, err)
		}
		r.ask.Info("acl %s: NoAccess for %s", d.Path, d.Who)
	}
	for _, d := range r.j.Denials {
		held, err := permissions(ctx, r.r, d.Who, d.Path)
		if err != nil {
			return err
		}
		if left := slices.DeleteFunc(slices.Clone(held), func(p string) bool { return !slices.Contains(d.Privs, p) }); len(left) > 0 {
			return fmt.Errorf("after NoAccess on %s, %s still holds %s there", d.Path, d.Who, strings.Join(left, ", "))
		}
	}
	return nil
}

// ensureProxmox makes role PCO, user pco@pve with its grant on /, and the
// token of the appliance anew: Proxmox shows a secret only once, and no
// journal holds it.
func (r *run) ensureProxmox(ctx context.Context) error {
	o := setup.Objects{Run: r.r, Ask: r.ask, Version: r.version, Record: r.recordManifest}
	if err := o.EnsureRole(ctx); err != nil {
		return err
	}
	if err := o.EnsureUser(ctx); err != nil {
		return err
	}
	return r.makeToken(ctx)
}

func (r *run) makeToken(ctx context.Context) error {
	vmid := r.j.VMID
	name, id := tokenName(vmid), tokenID(vmid)
	us, err := users(ctx, r.r)
	if err != nil {
		return err
	}
	toks, err := tokens(ctx, r.r, us)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(toks, func(t tokenEntry) bool { return t.Name == name }) {
		if _, err := r.r.Run(ctx, "pveum", "user", "token", "remove", setup.UserID, name); err != nil {
			return fmt.Errorf("removing token %s to make it anew: %w", id, err)
		}
		r.ask.Info("token %s: removed, to be made anew", id)
	}
	if err := r.recordManifest(func(m *setup.Manifest) { m.CreatedToken = true }); err != nil {
		return err
	}
	out, err := r.r.Run(ctx, "pveum", "user", "token", "add", setup.UserID, name,
		"--privsep", "1", "--comment", marker(vmid), "--output-format", "json")
	if err != nil {
		return fmt.Errorf("creating token %s: %w", id, err)
	}
	secret, err := setup.TokenSecret(out, id)
	if err != nil {
		return err
	}
	r.secret = store.NewSecret(secret)
	if _, err := r.r.Run(ctx, "pveum", "acl", "modify", "/", "--tokens", id, "--roles", setup.RoleID); err != nil {
		return fmt.Errorf("granting role %s on / to %s: %w", setup.RoleID, id, err)
	}
	r.ask.Info("token %s: created, privilege-separated, with role %s on /", id, setup.RoleID)
	return nil
}

// ensureTags registers the gate tags, as pco setup does.
func (r *run) ensureTags(ctx context.Context) error {
	register := true
	switch {
	case r.o.RegisterTags != nil:
		register = *r.o.RegisterTags
	case !r.o.Yes:
		var err error
		if register, err = r.ask.Confirm(setup.TagsQuestion(r.o.GateTag), true); err != nil {
			return err
		}
	}
	o := setup.Objects{Run: r.r, Ask: r.ask, Version: r.version, Record: r.recordManifest}
	return o.EnsureTags(ctx, r.o.GateTag, register)
}

// bootTimeout bounds the wait for the container's systemd to finish its boot.
const bootTimeout = "90"

// allowedFailures are the units that fail in a container that boots well:
// the daemon without its volume, and the first upgrade on a network without
// a way to Debian's mirrors.
var allowedFailures = []string{appliance.Unit, "pco-first-boot.service"}

// start starts the container, waits for its boot, and refuses a template
// that holds another version of pco than this installer: nothing is pushed
// into it.
func (r *run) start(ctx context.Context) error {
	vmid := r.j.VMID
	running, err := ctRunning(ctx, r.r, vmid)
	if err != nil {
		return err
	}
	if !running {
		if _, err := r.r.Run(ctx, "pct", "start", strconv.Itoa(vmid)); err != nil {
			return fmt.Errorf("starting lxc/%d: %w", vmid, err)
		}
	}
	if err := r.booted(ctx, vmid); err != nil {
		return err
	}
	out, err := r.exec(ctx, vmid, "pco", "version")
	if err != nil {
		return fmt.Errorf("asking the pco in lxc/%d for its version: %w", vmid, err)
	}
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "pco" || f[1] != r.h.version {
		return fmt.Errorf("lxc/%d holds %s, not pco %s as this installer: use the template of version %s",
			vmid, strings.TrimSpace(out), r.h.version, r.h.version)
	}
	r.ask.Info("container lxc/%d: running pco %s", vmid, r.h.version)
	return nil
}

func (r *run) booted(ctx context.Context, vmid int) error {
	out, err := r.exec(ctx, vmid, "timeout", bootTimeout, "systemctl", "is-system-running", "--wait")
	switch state := strings.TrimSpace(out); state {
	case "running":
		return nil
	case "degraded":
		failed, ferr := r.failedUnits(ctx, vmid)
		if ferr != nil {
			return ferr
		}
		if others := slices.DeleteFunc(failed, func(u string) bool { return slices.Contains(allowedFailures, u) }); len(others) > 0 {
			return fmt.Errorf("lxc/%d booted with failed units: %s", vmid, strings.Join(others, ", "))
		}
		return nil
	default:
		if err == nil {
			err = errors.New("no answer")
		}
		return fmt.Errorf("lxc/%d did not finish its boot within %s s (systemd says %q): %w", vmid, bootTimeout, state, err)
	}
}

func (r *run) failedUnits(ctx context.Context, vmid int) ([]string, error) {
	out, err := r.exec(ctx, vmid, "systemctl", "list-units", "--state=failed", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return nil, fmt.Errorf("listing the failed units of lxc/%d: %w", vmid, err)
	}
	var units []string
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) > 0 {
			units = append(units, f[0])
		}
	}
	return units, nil
}

// exec runs a command in the container with none of the installer's
// environment: pct exec passes all of it by default, a token in a variable
// of the admin's shell included.
func (r *run) exec(ctx context.Context, vmid int, args ...string) (string, error) {
	return r.r.Run(ctx, "pct", append([]string{"exec", strconv.Itoa(vmid), "--keep-env", "0", "--"}, args...)...)
}

// pco runs pco in the container, with PCO_CLOUDFLARE_API_URL when the
// installer has it: the end-to-end suite points the container at its relay
// so, and only for the command, never in the container's configuration.
func (r *run) pco(ctx context.Context, vmid int, args ...string) (string, error) {
	cmd := append([]string{"pco"}, args...)
	if r.o.CloudflareAPI != "" {
		cmd = append([]string{"env", "PCO_CLOUDFLARE_API_URL=" + r.o.CloudflareAPI}, cmd...)
	}
	return r.exec(ctx, vmid, cmd...)
}

// push puts the CA, the volume marker and the bootstrap into the container,
// and has pco appliance init make the store of it.
func (r *run) push(ctx context.Context) error {
	err := r.pushBootstrap(ctx, appliance.ModeInstall, true)
	if err == nil || !r.resumed {
		return err
	}
	// A run killed after init made the store finds an install that mode
	// install does not replace; mode repair takes it as it is.
	if state, serr := r.hasState(ctx, r.j.VMID); serr != nil || !state {
		return err
	}
	r.ask.Warn("%v; the volume holds the install an init of the run made before it was cut short: repairing it", err)
	return r.pushBootstrap(ctx, appliance.ModeRepair, false)
}

// pushBootstrap pushes the CA, the marker when asked, and a bootstrap of the
// mode, and runs pco appliance init on it. The bootstrap is written in a
// directory of its own under /run, readable by root alone, and removed right
// after its push, whatever came of it.
func (r *run) pushBootstrap(ctx context.Context, mode string, marker bool) error {
	vmid := r.j.VMID
	if err := r.makeTemp(); err != nil {
		return err
	}
	defer r.removeTemp()
	id := strconv.Itoa(vmid)
	if _, err := r.r.Run(ctx, "pct", "push", id, r.j.Endpoint.CA, caFile, "--perms", "0644"); err != nil {
		return fmt.Errorf("pushing the CA into lxc/%d: %w", vmid, err)
	}
	if marker {
		if err := r.pushMarker(ctx); err != nil {
			return err
		}
	}
	cfg, err := readCTConfig(ctx, r.r, r.node, vmid)
	if err != nil {
		return fmt.Errorf("reading the configuration of lxc/%d: %w", vmid, err)
	}
	macs, err := cfg.macs()
	if err != nil {
		return err
	}
	data, err := r.bootstrap(mode, macs)
	if err != nil {
		return err
	}
	path := filepath.Join(r.tmp, "bootstrap.json")
	if err := writeNew(path, data); err != nil {
		return err
	}
	_, err = r.r.Run(ctx, "pct", "push", id, path, bootstrapFile, "--perms", "0600")
	if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		r.ask.Warn("removing %s: %v", path, rerr)
	}
	if err != nil {
		return fmt.Errorf("pushing the bootstrap into lxc/%d: %w", vmid, err)
	}
	out, err := r.pco(ctx, vmid, "appliance", "init", "--bootstrap", bootstrapFile)
	for line := range strings.Lines(out) {
		if line = strings.TrimRight(line, "\n"); line != "" {
			r.ask.Info("  lxc/%d: %s", vmid, line)
		}
	}
	if err != nil {
		return fmt.Errorf("pco appliance init in lxc/%d: %w", vmid, err)
	}
	return nil
}

func (r *run) pushMarker(ctx context.Context) error {
	if err := r.makeTemp(); err != nil {
		return err
	}
	path := filepath.Join(r.tmp, "volume")
	if err := writeNew(path, nil); err != nil {
		return err
	}
	if _, err := r.r.Run(ctx, "pct", "push", strconv.Itoa(r.j.VMID), path, markerFile, "--perms", "0600"); err != nil {
		return fmt.Errorf("pushing the volume marker into lxc/%d: %w", r.j.VMID, err)
	}
	return nil
}

// bootstrap is the file of the bootstrap of the mode, secrets in the clear.
func (r *run) bootstrap(mode string, macs []string) ([]byte, error) {
	manifest, err := json.Marshal(r.j.Manifest)
	if err != nil {
		return nil, err
	}
	b := appliance.Bootstrap{
		Mode:            mode,
		VMID:            r.j.VMID,
		Node:            r.node,
		MACs:            macs,
		Endpoints:       []store.Endpoint{r.j.Endpoint.Endpoint},
		PVEToken:        store.PVEToken{TokenID: tokenID(r.j.VMID), Secret: r.secret},
		CloudflareToken: store.NewSecret(r.o.CloudflareToken),
		GateTag:         r.o.GateTag,
		Manifest:        manifest,
	}
	if mode == appliance.ModeRecover {
		b.InstallID = r.o.InstallID
	}
	for _, a := range r.addrs {
		b.NodeAddrs = append(b.NodeAddrs, a.Prefix.Addr())
	}
	return b.Encode()
}

// makeTemp makes the directory of this run under /run, readable by root
// alone. One left by a run that was killed is made anew.
func (r *run) makeTemp() error {
	if r.tmp != "" {
		return nil
	}
	dir := filepath.Join(r.h.runDir, "pco-appliance-install-"+r.runName())
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	r.tmp = dir
	return nil
}

func (r *run) removeTemp() {
	if r.tmp == "" {
		return
	}
	if err := os.RemoveAll(r.tmp); err != nil {
		r.ask.Warn("removing %s: %v", r.tmp, err)
	}
	r.tmp = ""
}

// runName names the run for its directory under /run, also before its
// journal does.
func (r *run) runName() string {
	if r.j.Run != "" {
		return r.j.Run
	}
	if id, err := r.runID(); err == nil {
		r.j.Run = id
	}
	return r.j.Run
}

// writeNew writes a file that must not be there yet, readable by root alone.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (r *run) protect(ctx context.Context) error {
	if _, err := r.r.Run(ctx, "pct", "set", strconv.Itoa(r.j.VMID), "--protection", "1"); err != nil {
		return fmt.Errorf("protecting lxc/%d: %w", r.j.VMID, err)
	}
	r.ask.Info("container lxc/%d: protected", r.j.VMID)
	return nil
}
