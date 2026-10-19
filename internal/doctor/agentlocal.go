package doctor

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	// controllerSilent is how long an agent goes without a heartbeat
	// before the controller counts as silent; controllerGone, before no
	// node holds the lease long enough to doubt one will.
	controllerSilent = 60 * time.Second
	controllerGone   = 3 * time.Minute

	// agentPort is the port of the agent endpoint of the controller.
	agentPort = 8644

	fixDialHost = "add this node's agent address to the ipset pco-nodes of the datacenter firewall and keep the rule " +
		"IN ACCEPT -p tcp -dport 8644 -source +pco-nodes there; pco setup on this node adds both"
	fixDialAppliance = "add this container's address to the ipset pco-agents and accept tcp/8644 from +pco-agents " +
		"in the firewall of the controller's container"
)

// AgentChecks are the checks a node of a cluster makes of itself and of its
// agent, which the controller asks every agent for: those of the node that
// need no state of the controller, of the appliance in a container, and of
// the agent. Every check has a finding, sorted by check.
func AgentChecks(ctx context.Context, st engine.State, env Env) []Finding {
	src := clusterOf(env)
	if src == nil {
		return sorted(agentLocal(ctx, st, env, agentproto.AgentView{}))
	}
	view, err := src.Agent()
	if err != nil {
		return sorted(append(agentLocal(ctx, st, env, agentproto.AgentView{}),
			fail("agent", "the agent of this node does not answer: "+err.Error(), "journalctl -u pco")))
	}
	return sorted(agentLocal(ctx, st, env, view))
}

// agentLocal are the checks of a node that is not the controller: the node
// as the daemon finds it, and its agent.
func agentLocal(ctx context.Context, st engine.State, env Env, view agentproto.AgentView) []Finding {
	app := applianceOf(st, env)
	repair := ""
	if app != nil {
		repair = repairFix(app.Self().VMID)
	}
	out := []Finding{
		checkCloudflared(ctx, env), checkOutbound(ctx, st, env), checkProxmox(ctx, env), checkStore(ctx, env, repair),
		checkLock(ctx, env), checkEgress(st.Egress, fixTable), checkNftables(ctx, env, app),
	}
	if app != nil {
		out = append(out, newApplianceRun(ctx, st, env, app, true).findings()...)
	}
	if view.Node != "" {
		out = append(out, agentChecks(ctx, st, env, view)...)
	}
	return out
}

// agentChecks are the checks of the agent of this node: whether it serves,
// whether it hears the controller and can reach it, and its certificate.
func agentChecks(ctx context.Context, st engine.State, env Env, view agentproto.AgentView) []Finding {
	out := []Finding{checkAgent(view), checkAgentCert(view, env.Now())}
	if view.Remote {
		out = append(out, checkController(st, view), checkAgentDial(ctx, st, env, view))
	}
	return out
}

func checkAgent(view agentproto.AgentView) Finding {
	const check = "agent"
	switch {
	case view.StoodDown != "":
		return fail(check, "the agent stood down: "+view.StoodDown, "pco agent status; journalctl -u pco")
	case len(view.Problems) > 0:
		return warn(check, fmt.Sprintf("%s: %s", count(len(view.Problems), "problem", "problems"), strings.Join(view.Problems, "; ")),
			"pco agent status")
	case view.Epoch.Generation == 0:
		return warn(check, "the agent follows no controller yet", "pco agent status")
	}
	return ok(check, fmt.Sprintf("the agent serves snapshot %d of generation %d, applied %s ago", view.Seq, view.Epoch.Generation,
		view.SeqAge.Round(time.Second)))
}

func checkAgentCert(view agentproto.AgentView, now time.Time) Finding {
	const check = "agent certificate"
	end := view.CertExpires
	switch {
	case end.IsZero():
		return ok(check, "the agent of this node runs in the controller's process")
	case !end.After(now):
		return fail(check, "the certificate of the agent ended on "+end.UTC().Format(time.DateOnly), renewFix(view, true))
	case end.Sub(now) < leafWarn:
		return warn(check, "the certificate of the agent ends on "+end.UTC().Format(time.DateOnly)+", in "+days(end.Sub(now)), renewFix(view, false))
	}
	return ok(check, "the certificate of the agent ends on "+end.UTC().Format(time.DateOnly))
}

// renewFix says how the certificate of the agent is had again. The
// controller takes no ended certificate of an appliance agent, not even to
// renew it: enrolling it again is the only way back.
func renewFix(view agentproto.AgentView, ended bool) string {
	switch {
	case view.Kind == pki.KindAppliance && ended:
		return "enrol it again, the only way back: pco node forget " + view.Node + " on the controller, then on its node " +
			"pco appliance uninstall --vmid <its vmid> and pco appliance install --kind agent with a new join token"
	case view.Kind == pki.KindAppliance:
		return "the agent renews its own over the channel while it is valid: journalctl -u pco in its container says why it did not; " +
			"once it ended, the agent is enrolled again with a new join token"
	}
	return "systemctl restart pco: a host node issues its own at the start of the daemon"
}

// checkController says when the agent last heard the controller it follows.
func checkController(st engine.State, view agentproto.AgentView) Finding {
	const check = "controller"
	ctrl := controllerName(st, view)
	silent := view.SilentFor.Round(time.Second)
	noHolder := st.Lease != nil && st.Lease.Holder == ""
	switch {
	case !view.Heard && noHolder || view.SilentFor >= controllerGone:
		return fail(check, fmt.Sprintf("no controller answered for %s", silent),
			"pvecm status says whether the cluster has quorum; a node takes the lease within 150 seconds of losing the controller; "+
				"pco events --kind security says what happened to the lease")
	case !view.Heard || view.SilentFor >= controllerSilent:
		return warn(check, fmt.Sprintf("the controller, %s, has been silent for %s", ctrl, silent), "pco agent status; agent dial says whether it is reached")
	}
	return ok(check, fmt.Sprintf("the controller, %s, answered %s ago", ctrl, view.HeartbeatAge.Round(time.Second)))
}

// checkAgentDial tries the agent endpoint of the controller over TCP, from
// this node, as its agent connects to it.
func checkAgentDial(ctx context.Context, st engine.State, env Env, view agentproto.AgentView) Finding {
	const check = "agent dial"
	addr, known := controllerEndpoint(st, view)
	fix := fixDialHost
	if view.Kind == pki.KindAppliance {
		fix = fixDialAppliance
	}
	if !known {
		return warn(check, "the address of the controller is not known on this node", "pco agent status")
	}
	if err := env.CanDial(ctx, "tcp", addr); err != nil {
		return fail(check, "the agent endpoint of the controller, "+addr+", cannot be reached: "+err.Error(), fix)
	}
	return ok(check, "the agent endpoint of the controller, "+addr+", answers")
}

// controllerEndpoint is where the controller's agent endpoint listens: the
// lease holder's agent address on the host, the address the agent follows
// in the appliance.
func controllerEndpoint(st engine.State, view agentproto.AgentView) (string, bool) {
	if l := st.Lease; l != nil && l.Address.IsValid() {
		return netip.AddrPortFrom(l.Address, agentPort).String(), true
	}
	if ap, err := netip.ParseAddrPort(view.Controller); err == nil {
		return ap.String(), true
	}
	if a, err := netip.ParseAddr(view.Controller); err == nil {
		return netip.AddrPortFrom(a, agentPort).String(), true
	}
	if host, port, err := net.SplitHostPort(view.Controller); err == nil && host != "" {
		if _, err := strconv.Atoi(port); err == nil {
			return view.Controller, true
		}
	}
	return "", false
}
