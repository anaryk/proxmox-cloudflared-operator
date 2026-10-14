package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ApplianceCluster is the ClusterSource of a controller container, which
// also tells where its own container and its agent containers are, as the
// Proxmox API says.
type ApplianceCluster interface {
	ApplianceFacts(ctx context.Context) (ApplianceFacts, error)
}

// ApplianceFacts are the controller container and the agent containers of
// an appliance install.
type ApplianceFacts struct {
	// VMID is the controller's container; HA says it is an HA resource, and
	// Storage is what its install recorded of its state volume, with the
	// replication jobs of its container now.
	VMID int
	// Node is the node the controller was installed on, which it serves,
	// and RunsOn the node it runs on now.
	Node        string
	RunsOn      string
	HA          bool
	Storage     *store.ApplianceStorage
	Replication []string
	Agents      []AgentContainer
}

// AgentContainer is the container of the agent of Node, enrolled as VMID.
type AgentContainer struct {
	Node string
	VMID int
	// Found says the cluster lists the container; RunsOn is the node it
	// runs on, Lock its lock, HA whether it is an HA resource and Cmode its
	// console mode, "tty" when it has none.
	Found  bool
	RunsOn string
	Lock   string
	HA     bool
	Cmode  string
	Err    string
}

// applianceClusterChecks are the checks of the controller of an appliance
// install: where its own volume is, and its agent containers.
func applianceClusterChecks(ctx context.Context, src ClusterSource) []Finding {
	ac, ok := src.(ApplianceCluster)
	if !ok {
		return nil
	}
	f, err := ac.ApplianceFacts(ctx)
	if err != nil {
		return []Finding{warn("controller ha", "the containers of the install could not be read: "+err.Error(), "pco doctor again")}
	}
	out := []Finding{checkControllerHA(f)}
	for _, a := range f.Agents {
		out = append(out, checkAgentContainer(a))
	}
	return out
}

func checkControllerHA(f ApplianceFacts) Finding {
	const check = "controller ha"
	where := "storage of its own"
	if s := f.Storage; s != nil {
		where = "storage " + s.ID
		if s.Shared {
			where = "shared storage " + s.ID
		}
	}
	switch {
	case f.RunsOn != "" && f.Node != "" && f.RunsOn != f.Node:
		return warn(check, fmt.Sprintf("lxc/%d runs on %s and serves node %s, which it was installed on: the legs and the DHCP "+
			"of the managed network of %s are on the bridges of %s now, and the guests of %s that only %s's bridges reach go unserved",
			f.VMID, f.RunsOn, f.Node, f.Node, f.RunsOn, f.Node, f.Node), fmt.Sprintf("ha-manager migrate ct:%d %s, or pct migrate it back", f.VMID, f.Node))
	case !f.HA:
		return ok(check, fmt.Sprintf("lxc/%d is no HA resource, on %s: a lost controller is restored from its export and promoted", f.VMID, where))
	case len(f.Replication) > 0:
		return warn(check, fmt.Sprintf("lxc/%d is an HA resource on %s, copied by replication %s: a relocation brings back a volume up to "+
			"one interval old, and the controller only observes until pco appliance recover", f.VMID, where, strings.Join(f.Replication, ", ")),
			"put the controller on shared storage, or keep the interval short")
	case f.Storage != nil && !f.Storage.Shared:
		return warn(check, fmt.Sprintf("lxc/%d is an HA resource on %s, which is not shared: it cannot be relocated with its volume", f.VMID, where),
			"put the controller on shared storage, or ha-manager remove ct:"+fmt.Sprint(f.VMID))
	}
	return ok(check, fmt.Sprintf("lxc/%d is an HA resource on %s", f.VMID, where))
}

func checkAgentContainer(a AgentContainer) Finding {
	check := "agent container " + a.Node
	switch {
	case a.Err != "":
		return warn(check, fmt.Sprintf("agent %d could not be read: %s", a.VMID, a.Err), "pco doctor again")
	case !a.Found:
		return fail(check, fmt.Sprintf("agent %d of node %s is not among the cluster's resources", a.VMID, a.Node),
			"pco node forget "+a.Node+" when it is gone for good")
	case a.HA:
		return fail(check, fmt.Sprintf("agent %d is a Proxmox HA resource; agents are bound to their node", a.VMID),
			fmt.Sprintf("ha-manager remove ct:%d", a.VMID))
	case a.RunsOn != a.Node && a.Lock != "migrate":
		return fail(check, fmt.Sprintf("agent %d runs on %s, enrolled for %s; move it back or enrol it again", a.VMID, a.RunsOn, a.Node),
			fmt.Sprintf("pct migrate %d %s", a.VMID, a.Node))
	case a.Cmode != "" && a.Cmode != "tty" && a.Cmode != "console":
		return fail(check, fmt.Sprintf("the console of agent %d is %s, which hands whoever opens it a shell as root", a.VMID, a.Cmode),
			fmt.Sprintf("on node %s: pct set %d --cmode tty, then restart the container", a.RunsOn, a.VMID))
	}
	return ok(check, fmt.Sprintf("agent %d runs on %s, no HA resource, its console a login", a.VMID, a.Node))
}
