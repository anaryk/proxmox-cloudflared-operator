package applianceinstall

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// ManagedOptions say what pco appliance managed enable makes for an
// appliance.
type ManagedOptions struct {
	VMID int
	// Pool grants the appliance the cards of every guest in that pool as
	// well, so that the managed network attaches them without a grant per
	// guest.
	Pool            string
	ConfirmSDNApply bool // apply the changes of others that are pending in the SDN as well
	Yes             bool
}

// ManagedEnableCommand is pco appliance managed enable: it makes the managed
// network of an appliance on the node, as root. The node's firewall must be
// able to isolate it, as in the appliance the guest firewall does: the
// datacenter firewall on, the iptables backend, pve-firewall running; each is
// read before anything is made. Then what it makes and grants is shown and
// asked about, once: the SDN zone pco and vnet pconet, without isolated
// ports, as the appliance's own card is a port of the vnet, each noted in the
// container's description and the appliance's manifest before it is made,
// under the lock of the SDN configuration; then the grant of the vnet to the
// appliance, as grant-network makes it, and with Pool the grant of the cards
// of that pool.
func (i *Installer) ManagedEnableCommand(ctx context.Context, o ManagedOptions) error {
	if o.VMID < 100 || o.VMID > 999999999 {
		return fmt.Errorf("--vmid %d: want 100 to 999999999", o.VMID)
	}
	if o.Pool != "" {
		if err := (setup.NetworkGrant{Pool: o.Pool}).Check(); err != nil {
			return fmt.Errorf("--pool %s: %w", o.Pool, err)
		}
	}
	r := i.newRun(Options{Yes: o.Yes, VMID: o.VMID}, kindGrant)
	r.j.VMID = o.VMID
	if err := r.whoami(); err != nil {
		return err
	}
	unlock, err := i.lock()
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := r.applianceRuns(ctx, o.VMID)
	if err != nil {
		return err
	}
	if err := r.checkFirewall(ctx); err != nil {
		return err
	}
	r.showManaged(o)
	if !o.Yes {
		ok, err := r.ask.Confirm(fmt.Sprintf("Make the managed network for lxc/%d and grant these? The SDN apply reloads the network on every node.", o.VMID), false)
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
	}
	var ours setup.Manifest
	if m, _ := r.readManifest(ctx, o.VMID, false); m != nil {
		ours = *m
	}
	sdn := setup.SDN{
		Run: r.r, Ask: r.ask, Ours: ours, Record: r.noteSDN(ctx, o.VMID, ours, cfg),
		IsolatePorts: false, Confirm: o.ConfirmSDNApply, Yes: true,
	}
	if err := sdn.Ensure(ctx); err != nil {
		return err
	}
	if err := r.grantManaged(ctx, o); err != nil {
		return err
	}
	r.ask.Info("the managed network is made for lxc/%d: vnet %s in zone %s, granted to %s", o.VMID, setup.ManagedVnet, setup.ManagedZone, tokenID(o.VMID))
	if o.Pool != "" {
		r.ask.Warn("%s may now change the network cards and the firewall of every guest in pool %s; "+
			"pveum acl delete /pool/%s --users %s --tokens '%s' --roles %s takes that back", tokenID(o.VMID), o.Pool,
			o.Pool, setup.UserID, tokenID(o.VMID), setup.RoleManaged)
	}
	r.ask.Info("next: pct exec %d -- pco network managed enable", o.VMID)
	return nil
}

// applianceRuns refuses a container that is not an appliance of this node,
// and one that does not run: its manifest notes what is made for it. It
// returns the container's configuration.
func (r *run) applianceRuns(ctx context.Context, vmid int) (ctConfig, error) {
	cfg, err := readCTConfig(ctx, r.r, r.node, vmid)
	switch {
	case err != nil && !notThere(err):
		return nil, fmt.Errorf("reading the configuration of lxc/%d: %w", vmid, err)
	case err != nil:
		if err := r.whereElse(ctx, vmid, "managed enable", "nothing was made"); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("there is no container lxc/%d in the cluster", vmid)
	}
	if _, ok := parseDescription(cfg["description"]); !ok {
		return nil, fmt.Errorf("lxc/%d is not a pco appliance (its description lacks the mark of the installer): nothing was made", vmid)
	}
	running, err := ctRunning(ctx, r.r, vmid)
	switch {
	case err != nil:
		return nil, err
	case !running:
		return nil, fmt.Errorf("lxc/%d is stopped: start it with pct start %d, as its manifest notes what this makes", vmid, vmid)
	}
	return cfg, nil
}

// checkFirewall refuses a node whose guest firewall cannot isolate the
// managed network of an appliance: the nftables backend installed no guest
// rules where it was tried, and without the datacenter firewall or
// pve-firewall running no guest rule is in force.
func (r *run) checkFirewall(ctx context.Context) error {
	var node struct {
		NFTables flexBool `json:"nftables"`
	}
	if err := pvesh(ctx, r.r, &node, "/nodes/"+r.node+"/firewall/options"); err != nil {
		return fmt.Errorf("reading the firewall options of node %s: %w", r.node, err)
	}
	if node.NFTables {
		//nolint:staticcheck // the line the state and the docs show, which names the feature
		return errors.New("Managed in the appliance is not supported with the nftables firewall backend (nftables: 1): " +
			"it installed no guest rules in the lab; set nftables: 0 in the node's firewall options")
	}
	var cluster struct {
		Enable flexBool `json:"enable"`
	}
	if err := pvesh(ctx, r.r, &cluster, "/cluster/firewall/options"); err != nil {
		return fmt.Errorf("reading the firewall options of the datacenter: %w", err)
	}
	if !cluster.Enable {
		//nolint:staticcheck // the line the state and the docs show, which names the feature
		return errors.New("Managed in the appliance needs the datacenter firewall; it is off: turn it on under Datacenter, " +
			"Firewall, Options, with rules that keep your own access, and run this again")
	}
	out, err := r.r.Run(ctx, "systemctl", "is-active", "pve-firewall")
	if state := strings.TrimSpace(out); state != "active" {
		if state == "" && err != nil {
			state = err.Error()
		}
		return fmt.Errorf("the node's firewall service pve-firewall is not running (%s): start it with systemctl start "+
			"pve-firewall, and run this again", state)
	}
	return nil
}

// noteSDN notes the zone and the vnet of the managed network before each is
// made: in the description of the container, the node's own record, which
// the appliance cannot change, under the note the node keeps (sdnNotePath),
// and in the manifest of the appliance, through
// pco in it, from what the manifest held when the run read it. Uninstall
// removes an object only when both name it as pco's.
func (r *run) noteSDN(ctx context.Context, vmid int, m setup.Manifest, cfg ctConfig) func(change func(*setup.Manifest)) error {
	return func(change func(*setup.Manifest)) error {
		before := m
		change(&m)
		args := []string{"pco", "appliance", "manifest", "sdn"}
		if m.CreatedZone != before.CreatedZone {
			args = append(args, "--created-zone="+strconv.FormatBool(m.CreatedZone))
		}
		if m.CreatedVnet != before.CreatedVnet {
			args = append(args, "--created-vnet="+strconv.FormatBool(m.CreatedVnet))
		}
		if len(args) == 4 {
			return nil
		}
		note, err := r.ensureSDNNote(vmid)
		if err != nil {
			m = before
			return err
		}
		desc, ok := withSDNNotes(cfg["description"], m.CreatedZone, m.CreatedVnet, note)
		if !ok {
			m = before
			return fmt.Errorf("the description of lxc/%d is not the installer's any more; nothing was made", vmid)
		}
		if _, err := r.r.Run(ctx, "pct", "set", strconv.Itoa(vmid), "--description", desc); err != nil {
			m = before
			return fmt.Errorf("noting it in the description of lxc/%d: %w", vmid, err)
		}
		if _, err := r.exec(ctx, vmid, args...); err != nil {
			if _, uerr := r.r.Run(ctx, "pct", "set", strconv.Itoa(vmid), "--description", cfg["description"]); uerr != nil {
				r.ask.Warn("the description of lxc/%d keeps a note of what was not made: %v", vmid, uerr)
			}
			m = before
			return fmt.Errorf("noting it in the manifest of lxc/%d: %w", vmid, err)
		}
		cfg["description"] = desc
		return nil
	}
}

// showManaged says what managed enable makes and grants, before it asks.
func (r *run) showManaged(o ManagedOptions) {
	vm, network := "/vms/"+strconv.Itoa(o.VMID), setup.NetworkGrant{Zone: setup.ManagedZone, VNet: setup.ManagedVnet}
	id := tokenID(o.VMID)
	r.ask.Info("pco appliance managed enable makes the SDN zone %s and the vnet %s where they are missing, applies the SDN "+
		"configuration, and grants pco@pve and %s:", setup.ManagedZone, setup.ManagedVnet, id)
	r.ask.Info("  role %s (VM.Config.Network) on %s", setup.RoleManaged, vm)
	r.ask.Info("  role %s (SDN.Use, SDN.Audit) on %s", setup.RoleSDN, network.Path())
	r.ask.Info("  pveum acl modify %s --users %s --tokens '%s' --roles %s", vm, setup.UserID, id, setup.RoleManaged)
	r.ask.Info("  pveum acl modify %s --users %s --tokens '%s' --roles %s", network.Path(), setup.UserID, id, setup.RoleSDN)
	if o.Pool != "" {
		pool := setup.NetworkGrant{Pool: o.Pool}
		r.ask.Info("  role %s (VM.Config.Network) on %s: the network cards and the firewall of every guest in pool %s",
			setup.RoleManaged, pool.Path(), o.Pool)
		r.ask.Info("  pveum acl modify %s --users %s --tokens '%s' --roles %s", pool.Path(), setup.UserID, id, setup.RoleManaged)
	}
}

// grantManaged grants the appliance the managed vnet, and with a pool the
// cards of the pool, each noted in a journal first and in the manifest after,
// as grant-network does.
func (r *run) grantManaged(ctx context.Context, o ManagedOptions) error {
	network := setup.NetworkGrant{Zone: setup.ManagedZone, VNet: setup.ManagedVnet}
	if err := r.grant(ctx, network, func() (setup.NetworkGrant, error) {
		return GrantNetwork(ctx, r.r, o.VMID, network.Zone, network.VNet, 0)
	}); err != nil {
		return err
	}
	if o.Pool == "" {
		return nil
	}
	pool := setup.NetworkGrant{Pool: o.Pool}
	return r.grant(ctx, pool, func() (setup.NetworkGrant, error) { return GrantPool(ctx, r.r, o.VMID, o.Pool) })
}

// grant notes a grant in the journal, makes it and records it in the
// manifest of the appliance. A grant that fails takes itself back.
func (r *run) grant(ctx context.Context, g setup.NetworkGrant, make func() (setup.NetworkGrant, error)) error {
	if err := r.record(func(j *journal) { j.Grant = &g }); err != nil {
		return err
	}
	made, err := make()
	if err != nil {
		_ = r.removeJournal()
		return err
	}
	r.ask.Info("granted: %s", made.Path())
	r.recordGrant(ctx, "add", made)
	return r.removeJournal()
}
