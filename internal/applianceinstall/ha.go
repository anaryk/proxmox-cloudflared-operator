package applianceinstall

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// haRule is the name of the rule, or on Proxmox VE 8 the group, that keeps a
// controller on the nodes the admin chose.
func haRule(vmid int) string { return "pco-" + strconv.Itoa(vmid) }

// explainHA says what an HA resource does to the node that runs it, before
// the controller becomes one.
func (r *run) explainHA() {
	r.ask.Warn("--ha makes lxc/%d a Proxmox HA resource: the node that runs it arms its watchdog, and fences itself when it "+
		"loses quorum, which resets every guest on that node", r.j.VMID)
	r.ask.Warn("on shared storage the relocated controller writes on; on replicated storage its volume may be one replication " +
		"interval old, and it only observes until pco appliance recover")
}

// makeHA keeps the controller on the nodes the admin chose, when any, and
// makes it an HA resource that is started.
func (r *run) makeHA(ctx context.Context) error {
	sid := "ct:" + strconv.Itoa(r.j.VMID)
	var group []string
	if len(r.o.HANodes) > 0 {
		nodes := strings.Join(r.o.HANodes, ",")
		name := haRule(r.j.VMID)
		if r.version.Major() >= 9 {
			if _, err := r.r.Run(ctx, "ha-manager", "add", sid, "--state", "started"); err != nil {
				return fmt.Errorf("making lxc/%d an HA resource: %w", r.j.VMID, err)
			}
			if _, err := r.r.Run(ctx, "ha-manager", "rules", "add", "node-affinity", name, "--resources", sid, "--nodes", nodes,
				"--comment", marker(r.j.VMID)); err != nil {
				return fmt.Errorf("adding the rule %s that keeps lxc/%d on %s: %w", name, r.j.VMID, nodes, err)
			}
			r.ask.Info("ha: lxc/%d is an HA resource, kept on %s by rule %s", r.j.VMID, nodes, name)
			return nil
		}
		if _, err := r.r.Run(ctx, "ha-manager", "groupadd", name, "--nodes", nodes, "--restricted", "1", "--comment", marker(r.j.VMID)); err != nil {
			return fmt.Errorf("adding the group %s that keeps lxc/%d on %s: %w", name, r.j.VMID, nodes, err)
		}
		group = []string{"--group", name}
	}
	if _, err := r.r.Run(ctx, "ha-manager", append([]string{"add", sid, "--state", "started"}, group...)...); err != nil {
		return fmt.Errorf("making lxc/%d an HA resource: %w", r.j.VMID, err)
	}
	r.ask.Info("ha: lxc/%d is an HA resource", r.j.VMID)
	return nil
}

// removeHA removes the rule, or on Proxmox VE 8 the group, that an install
// or a convert made to keep the controller vmid on its nodes: the one of its
// name that carries its mark. pct destroy --purge removed the HA resource
// itself. A listing Proxmox does not have, rules on 8, is none.
func (r *run) removeHA(ctx context.Context, vmid int) error {
	name := haRule(vmid)
	for _, kind := range []struct{ path, key, remove string }{
		{"/cluster/ha/rules", "rule", "rules remove"},
		{"/cluster/ha/groups", "group", "groupremove"},
	} {
		var entries []map[string]any
		if err := pvesh(ctx, r.r, &entries, kind.path); err != nil {
			continue
		}
		for _, e := range entries {
			if e[kind.key] != name || e["comment"] != marker(vmid) {
				continue
			}
			args := append(strings.Fields(kind.remove), name)
			if _, err := r.r.Run(ctx, "ha-manager", args...); err != nil {
				return fmt.Errorf("removing the HA %s %s of lxc/%d: %w", kind.key, name, vmid, err)
			}
			r.ask.Info("ha: %s %s of lxc/%d removed", kind.key, name, vmid)
		}
	}
	return nil
}
