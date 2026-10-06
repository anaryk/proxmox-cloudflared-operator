package applianceinstall

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
)

// Repair puts the appliance vmid right after a restore, a changed certificate
// of the API or a lost token. Its volume decides how: with the state of
// an install, the token is made anew, the tags and the certificate checked
// again, and a bootstrap of mode repair carries the token, the MACs, the
// endpoint and the node's addresses as they are now. Without state (a restore
// to a new VMID, over the appliance, or a lost volume, and only with
// Recover), the volume is given back, marked, and a bootstrap of mode recover
// adopts the install the Cloudflare token sees, with a manifest rebuilt from
// the marks in Proxmox.
//
// A repair takes nothing back when it fails: every step looks at what is
// there, and running it again finishes it.
func (i *Installer) Repair(ctx context.Context, vmid int, o Options) error {
	if vmid < 100 || vmid > 999999999 {
		return fmt.Errorf("--vmid %d: want 100 to 999999999", vmid)
	}
	o.VMID = vmid
	o.defaults()
	if err := o.check(); err != nil {
		return err
	}
	unlock, err := i.lock()
	if err != nil {
		return err
	}
	defer unlock()
	r := i.newRun(o, kindRepair)
	r.j.VMID = vmid
	err = r.repair(ctx)
	if rerr := r.removeJournal(); rerr != nil {
		r.ask.Warn("%v", rerr)
	}
	switch {
	case err == nil:
		return nil
	case errors.As(err, new(refusal)):
		return err
	}
	return fmt.Errorf("%w; run pco appliance repair --vmid %d again once that is put right", err, vmid)
}

// refusal is an error that running the command again does not put right.
type refusal struct{ error }

func (e refusal) Unwrap() error { return e.error }

// whereElse says where the container vmid is when this node has no
// configuration of it: on another node of the cluster, where the command
// must run, which it refuses with the outcome; or on none, which is nil, as
// the container is gone.
func (r *run) whereElse(ctx context.Context, vmid int, command, outcome string) error {
	node, ok, err := containerNode(ctx, r.r, vmid)
	switch {
	case err != nil:
		return err
	case !ok:
		return nil
	case node == r.node:
		return refusal{fmt.Errorf("lxc/%d is listed on %s, but its configuration cannot be read here; %s", vmid, node, outcome)}
	}
	return refusal{fmt.Errorf("lxc/%d is on node %s, not on %s: run the %s there; %s", vmid, node, r.node, command, outcome)}
}

func (r *run) repair(ctx context.Context) error {
	vmid := r.j.VMID
	if err := r.node0(ctx); err != nil {
		return err
	}
	if err := r.cluster(ctx); err != nil {
		return err
	}
	cfg, err := readCTConfig(ctx, r.r, r.node, vmid)
	switch {
	case err != nil && !notThere(err):
		return fmt.Errorf("reading the configuration of lxc/%d: %w", vmid, err)
	case err != nil:
		if err := r.whereElse(ctx, vmid, "repair", "nothing was changed"); err != nil {
			return err
		}
		return refusal{fmt.Errorf("there is no container lxc/%d in the cluster", vmid)}
	}
	if err := r.markContainer(ctx, cfg); err != nil {
		return err
	}
	r.fromConfig(cfg)
	running, err := ctRunning(ctx, r.r, vmid)
	if err != nil {
		return err
	}
	// pct push and pull need the container running; it is put back as it was.
	if !running {
		if _, err := r.r.Run(ctx, "pct", "start", strconv.Itoa(vmid)); err != nil {
			return fmt.Errorf("starting lxc/%d: %w", vmid, err)
		}
		defer r.stopAgain(ctx, vmid)
	}
	state, err := r.hasState(ctx, vmid)
	if err != nil {
		return err
	}
	switch {
	case state && r.o.Recover:
		return fmt.Errorf("the volume of lxc/%d holds the state of an install: repair it without --recover", vmid)
	case !state && !r.o.Recover:
		return fmt.Errorf("the volume of lxc/%d holds no state (a restore, or a lost volume): pco appliance repair --vmid %d "+
			"--recover --cf-token-file <file> adopts the install the Cloudflare token sees", vmid, vmid)
	}
	if err := r.repairEndpoint(ctx); err != nil {
		return err
	}
	if state {
		return r.repairState(ctx)
	}
	return r.recoverState(ctx, cfg)
}

// markContainer refuses a container without the installer's mark and a copy
// beside its original, and gives a restored one the mark of its own VMID, by
// which uninstall finds it.
func (r *run) markContainer(ctx context.Context, cfg ctConfig) error {
	vmid := r.j.VMID
	from, ok := describedVMID(cfg["description"])
	switch {
	case !ok:
		return refusal{fmt.Errorf("lxc/%d is not a pco appliance (its description lacks the mark of the installer): nothing was changed", vmid)}
	case from == vmid:
		return nil
	}
	if err := r.notACopy(ctx, from); err != nil {
		return err
	}
	if _, err := r.r.Run(ctx, "pct", "set", strconv.Itoa(vmid), "--description", description(vmid, r.now())); err != nil {
		return fmt.Errorf("marking lxc/%d: %w", vmid, err)
	}
	r.ask.Info("container lxc/%d: made from lxc/%d, and marked as itself now", vmid, from)
	return nil
}

// notACopy refuses to repair the container while lxc/from, which it is a copy
// of, is in the cluster: both would write the one install, the copy with the
// credentials of the original.
func (r *run) notACopy(ctx context.Context, from int) error {
	vmid := r.j.VMID
	if from == 0 || from == vmid {
		return nil
	}
	node, ok, err := containerNode(ctx, r.r, from)
	if err != nil || !ok {
		return err
	}
	return refusal{fmt.Errorf("lxc/%d is a copy of lxc/%d, which is still there, on node %s: repaired, the copy would write "+
		"the install of lxc/%d beside it, with its credentials. Remove the copy with pco appliance uninstall --vmid %d "+
		"--keep-cloudflare, or, for an appliance of its own, install one anew with pco appliance install; nothing was changed",
		vmid, from, node, from, vmid)}
}

// fromConfig takes the bridge and the VLAN of the appliance's card, and its
// storage, from its configuration unless the options name them.
func (r *run) fromConfig(cfg ctConfig) {
	net0 := cfg["net0"]
	if b := option(net0, "bridge"); b != "" && r.o.Bridge == DefaultBridge {
		r.o.Bridge = b
	}
	if v, err := strconv.Atoi(option(net0, "tag")); err == nil && r.o.VLAN == 0 {
		r.o.VLAN = v
	}
	if r.o.Storage == "" {
		r.o.Storage = cfg.volumeStorage("rootfs")
	}
}

func (r *run) stopAgain(ctx context.Context, vmid int) {
	if _, err := r.r.Run(context.WithoutCancel(ctx), "pct", "stop", strconv.Itoa(vmid)); err != nil {
		r.ask.Warn("lxc/%d was stopped before the repair and could not be stopped again: %v", vmid, err)
		return
	}
	r.ask.Info("container lxc/%d: stopped again, as it was", vmid)
}

// hasState reports whether the volume of the container holds a store: its
// marker and the store's meta directory.
func (r *run) hasState(ctx context.Context, vmid int) (bool, error) {
	for _, test := range [][]string{{"test", "-f", markerFile}, {"test", "-d", stateMeta}} {
		if _, err := r.exec(ctx, vmid, test...); err != nil {
			var code interface{ ExitCode() int }
			if errors.As(err, &code) && code.ExitCode() != 1 {
				return false, fmt.Errorf("looking at the volume of lxc/%d: %w", vmid, err)
			}
			return false, nil
		}
	}
	return true, nil
}

// repairEndpoint probes the certificate of the API again: a regenerated
// cluster CA, ACME turned on or a custom certificate changes the name it
// verifies under and the CA the appliance needs.
func (r *run) repairEndpoint(ctx context.Context) error {
	host := r.o.APIHost
	if host == "" {
		addr, ok := r.bridgeAddr()
		if !ok {
			return fmt.Errorf("the node has no address on %s, which the appliance reaches the API at: name one with --api-host", r.netDevice())
		}
		host = addr
	}
	ep, err := r.chooseEndpoint(ctx, host)
	if err != nil {
		return err
	}
	r.j.Endpoint = ep
	return nil
}

func (r *run) repairState(ctx context.Context) error {
	vmid := r.j.VMID
	pulled, named := r.readManifest(ctx, vmid, true)
	if err := r.notACopy(ctx, named); err != nil {
		return err
	}
	if pulled != nil {
		r.j.Manifest = *pulled
	} else {
		m, err := r.manifestFromMarks(ctx, vmid)
		if err != nil {
			return err
		}
		r.j.Manifest = m
	}
	r.appliance().Node = r.node
	if err := r.ensureProxmox(ctx); err != nil {
		return err
	}
	if err := r.ensureTags(ctx); err != nil {
		return err
	}
	return r.pushBootstrap(ctx, appliance.ModeRepair, false)
}

func (r *run) recoverState(ctx context.Context, cfg ctConfig) error {
	vmid := r.j.VMID
	if err := r.recoveryToken(); err != nil {
		return err
	}
	if cfg["mp0"] == "" {
		if err := r.addVolume(ctx); err != nil {
			return err
		}
	}
	if err := r.pushMarker(ctx); err != nil {
		return err
	}
	r.removeTemp()
	m, err := r.manifestFromMarks(ctx, vmid)
	if err != nil {
		return err
	}
	r.j.Manifest = m
	if err := r.ensureProxmox(ctx); err != nil {
		return err
	}
	return r.pushBootstrap(ctx, appliance.ModeRecover, false)
}

// recoveryToken makes sure there is a Cloudflare token to find the install
// with: from --cf-token-file, or asked for.
func (r *run) recoveryToken() error {
	const missing = "--recover needs a Cloudflare token to find the install with: pass --cf-token-file"
	if r.o.CloudflareToken != "" {
		return nil
	}
	if r.o.Yes {
		return errors.New(missing)
	}
	token, err := r.ask.Secret("Cloudflare API token, to find the install with: ")
	if err != nil {
		return fmt.Errorf("reading the token: %w", err)
	}
	if r.o.CloudflareToken = strings.TrimSpace(token); r.o.CloudflareToken == "" {
		return errors.New(missing)
	}
	return nil
}

// addVolume gives a container restored without its state volume a new one;
// a mount point is added to a stopped container.
func (r *run) addVolume(ctx context.Context) error {
	vmid := r.j.VMID
	id := strconv.Itoa(vmid)
	if err := r.record(func(j *journal) { j.AddedMP0 = true }); err != nil {
		return err
	}
	if _, err := r.r.Run(ctx, "pct", "stop", id); err != nil {
		return fmt.Errorf("stopping lxc/%d to give it a state volume: %w", vmid, err)
	}
	if _, err := r.r.Run(ctx, "pct", "set", id, "--mp0", r.mp0()); err != nil {
		return fmt.Errorf("giving lxc/%d a state volume: %w", vmid, err)
	}
	if _, err := r.r.Run(ctx, "pct", "start", id); err != nil {
		return fmt.Errorf("starting lxc/%d: %w", vmid, err)
	}
	if err := r.booted(ctx, vmid); err != nil {
		return err
	}
	r.ask.Info("container lxc/%d: given a state volume on %s", vmid, r.o.Storage)
	return nil
}
