package applianceinstall

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// ConvertOptions are the answers of pco appliance convert given in advance.
type ConvertOptions struct {
	VMID    int
	Yes     bool
	HA      bool
	HANodes []string
}

// Convert turns the combined appliance vmid on this node into the controller
// of a cluster, in place: the store it holds, its credentials and its
// connectors stay, and it goes on serving the node it runs on. Inside, pco
// appliance convert records the kind and restarts the daemon, which makes
// the install's CA and serves the agent endpoint; on the node, the container
// is tagged a controller, its firewall opens tcp/8644 to ipset pco-agents
// alone, its token may read its own storage, and with HA it becomes an HA
// resource. Each step is safe to run again: a convert that failed is run
// again to finish.
func (i *Installer) Convert(ctx context.Context, o ConvertOptions) error {
	if o.VMID < 100 || o.VMID > 999999999 {
		return fmt.Errorf("--vmid %d: want 100 to 999999999", o.VMID)
	}
	co := Options{Yes: o.Yes, Kind: KindController, HA: o.HA, HANodes: o.HANodes}
	if err := co.checkKind(); err != nil {
		return err
	}
	r := i.newRun(co, "convert")
	r.j.VMID = o.VMID
	if err := r.whoami(); err != nil {
		return err
	}
	unlock, err := i.lock()
	if err != nil {
		return err
	}
	defer unlock()
	out, err := r.r.Run(ctx, "pveversion")
	if err != nil {
		return fmt.Errorf("reading the version of Proxmox VE: %w", err)
	}
	if r.version, err = setup.ParsePVEVersion(out); err != nil {
		return err
	}
	cfg, err := r.convertible(ctx, o.VMID)
	if err != nil {
		return err
	}
	r.o.Storage = cfg.volumeStorage("mp0")
	if r.o.Storage == "" {
		return fmt.Errorf("lxc/%d has no state volume on mp0: pco appliance repair --vmid %d first", o.VMID, o.VMID)
	}
	if o.HA {
		r.explainHA()
	}
	if !o.Yes {
		ok, err := r.ask.Confirm(fmt.Sprintf("Make lxc/%d the controller that the agents of the other nodes join?", o.VMID), false)
		switch {
		case err != nil:
			return err
		case !ok:
			return setup.ErrAborted
		}
	}
	return r.convert(ctx, cfg)
}

// convertible reads the container and refuses one that is not a combined
// appliance of the installer on this node, or a controller a convert
// began, and one beside another controller.
func (r *run) convertible(ctx context.Context, vmid int) (ctConfig, error) {
	cfg, err := readCTConfig(ctx, r.r, r.node, vmid)
	if err != nil {
		if notThere(err) {
			if node, found, nerr := containerNode(ctx, r.r, vmid); nerr == nil && found {
				return nil, fmt.Errorf("lxc/%d is on node %s: run the convert there", vmid, node)
			}
		}
		return nil, fmt.Errorf("reading the configuration of lxc/%d: %w", vmid, err)
	}
	d, ok := parseDescription(cfg["description"])
	switch kind := kindOf(cfg["tags"]); {
	case !ok || d.vmid != vmid:
		return nil, fmt.Errorf("lxc/%d is not an appliance pco appliance install made", vmid)
	case kind != KindCombined && kind != KindController:
		return nil, fmt.Errorf("lxc/%d is no combined appliance (its tags say %s): only a combined appliance becomes a controller", vmid, cmpKind(kind))
	}
	running, err := ctRunning(ctx, r.r, vmid)
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, fmt.Errorf("lxc/%d is stopped: start it, the convert runs inside it as well", vmid)
	}
	by, err := appliancesByKind(ctx, r.r)
	if err != nil {
		return nil, err
	}
	for _, c := range by[KindController] {
		if int(c.VMID) != vmid {
			return nil, fmt.Errorf("the controller lxc/%d serves this cluster already: one controller serves an install", int(c.VMID))
		}
	}
	return cfg, nil
}

func (r *run) convert(ctx context.Context, cfg ctConfig) error {
	vmid := r.j.VMID
	out, err := r.pco(ctx, vmid, "appliance", "convert")
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); line != "" {
			r.ask.Info("  lxc/%d: %s", vmid, line)
		}
	}
	if err != nil {
		return fmt.Errorf("pco appliance convert in lxc/%d: %w", vmid, err)
	}
	if kindOf(cfg["tags"]) != KindController {
		tags := strings.ReplaceAll(cfg["tags"], "pco-"+KindCombined, "pco-"+KindController)
		if _, err := r.r.Run(ctx, "pct", "set", strconv.Itoa(vmid), "--tags", tags); err != nil {
			return fmt.Errorf("tagging lxc/%d a controller: %w", vmid, err)
		}
	}
	for _, step := range []func(context.Context) error{r.endpointRule, r.auditStorage} {
		if err := step(ctx); err != nil {
			return err
		}
	}
	if r.o.HA {
		if err := r.makeHA(ctx); err != nil {
			return err
		}
	}
	r.ask.Info("lxc/%d is the controller of the install; its connectors ran throughout", vmid)
	r.ask.Info("next, for each other node:")
	r.ask.Info("  pct exec %d -- pco cluster join-token --node <node> --file /root/<node>.join", vmid)
	r.ask.Info("  copy the file to the node, then there: pco appliance install --kind agent --join-file /root/<node>.join --storage <storage>")
	return nil
}

func cmpKind(kind string) string {
	if kind == "" {
		return "none"
	}
	return kind
}
