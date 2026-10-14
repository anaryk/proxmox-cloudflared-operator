package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The bounds the checks of a cluster go by.
const (
	// maxAnswerFindings and maxAnswerText bound what the answer of an
	// agent adds to the controller's findings, which is that agent's word.
	maxAnswerFindings = 64
	maxAnswerText     = 512

	// AnswerWait is how long pco doctor --cluster waits for the agents of
	// the other nodes to answer with their checks.
	AnswerWait = 30 * time.Second

	leaseRefreshLate = 15 * time.Second
	statusStale      = 30 * time.Second
	nodeSkewWarn     = 2 * time.Second
	nodeSkewFail     = 10 * time.Second
	leafWarn         = 30 * 24 * time.Hour
	caWarn           = 365 * 24 * time.Hour
	cloudflareSilent = 10 * time.Minute
	pinnedEmptyWarn  = 10 * time.Minute
)

// ErrNotChecked is the answer of a ClusterSource that has no such file to
// read, as an appliance has neither the members file nor corosync.conf: the
// checks that go by them say they were not made, rather than pass.
var ErrNotChecked = errors.New("a container reads neither the members file nor corosync.conf of the cluster")

// ClusterSource is what the checks of a cluster read beyond the state of
// the engine: the agent of this node, what Proxmox says of the cluster, the
// install's CA, what the controller's cycles know, and the agents of the
// other nodes.
type ClusterSource interface {
	// Agent is the agent of this node, as its client knows it.
	Agent() (agentproto.AgentView, error)
	Members() (members.Members, error)
	Corosync() (members.Corosync, error)
	// CAExpires is when the install's CA ends; the zero time when the
	// install has none yet.
	CAExpires() (time.Time, error)
	Facts() engine.ClusterFacts
	// Ask has the agents of nodes make their checks, and waits for their
	// answers for at most wait.
	Ask(ctx context.Context, nodes []string, wait time.Duration) map[string]AgentAnswer
}

// AgentAnswer is what the agent of a node answered when asked for its
// checks: its findings, or why it could not make them.
type AgentAnswer struct {
	Answered bool
	Findings []Finding
	Err      string
	// Why says why the agent was not asked, as while another ask ran;
	// empty when it was asked and did not answer in time.
	Why string
}

// clustered is an Env of a node that may be one of a cluster.
type clustered interface {
	ClusterSource() ClusterSource
}

func clusterOf(env Env) ClusterSource {
	if c, ok := env.(clustered); ok {
		return c.ClusterSource()
	}
	return nil
}

// Options says what a run of the doctor checks beyond this node.
type Options struct {
	// Cluster has the agents of the other Active nodes make their checks
	// too, on the controller.
	Cluster bool
}

// RunWith is Run with options. A node of a cluster that is not the
// controller makes the checks of itself and its agent, and says where the
// checks of the cluster run; the controller adds them to its own.
func RunWith(ctx context.Context, st engine.State, env Env, opts Options) []Finding {
	src := clusterOf(env)
	if src == nil {
		return sorted(runNode(ctx, st, env))
	}
	view, err := src.Agent()
	switch {
	case err == nil && view.Remote:
		out := append(agentLocal(ctx, st, env, view), checkFollower(st, view, opts))
		return sorted(out)
	case len(st.Nodes) <= 1:
		return sorted(runNode(ctx, st, env))
	}
	// The other agents make their checks while the controller makes its
	// own.
	asked := make(chan []Finding, 1)
	if opts.Cluster {
		go func() { asked <- askAgents(ctx, st, src) }()
	} else {
		asked <- nil
	}
	out := runNode(ctx, st, env)
	out = append(out, clusterChecks(st, src, env.Now())...)
	out = append(out, applianceClusterChecks(ctx, src)...)
	if err == nil {
		out = append(out, agentChecks(ctx, st, env, view)...)
	}
	return sorted(append(out, <-asked...))
}

func sorted(out []Finding) []Finding {
	slices.SortStableFunc(out, compareChecks)
	return out
}

// checkFollower says, on a node that is not the controller, where the
// checks of the cluster run, or that no controller answers to run them.
func checkFollower(st engine.State, view agentproto.AgentView, opts Options) Finding {
	const check = "cluster"
	ctrl := controllerName(st, view)
	if !view.Heard || view.SilentFor >= controllerSilent {
		return warn(check, "no controller answered this node's agent; the checks of the cluster were not made",
			"run pco doctor on the other nodes; the controller check says what this node last heard")
	}
	if opts.Cluster {
		return warn(check, "this node is not the controller; the checks of the cluster run on "+ctrl, "pco doctor --cluster on "+ctrl)
	}
	return ok(check, "the checks of the cluster run on the controller, "+ctrl+": pco doctor --cluster there")
}

// controllerName is the controller as this node knows it: the holder of the
// lease and its address on the host, what the agent follows otherwise.
func controllerName(st engine.State, view agentproto.AgentView) string {
	if l := st.Lease; l != nil && l.Holder != "" {
		if l.Address.IsValid() {
			return fmt.Sprintf("%s (%s)", l.Holder, l.Address)
		}
		return l.Holder
	}
	return cmp.Or(view.Controller, "unknown")
}

// clusterChecks are the checks the controller makes of the cluster.
func clusterChecks(st engine.State, src ClusterSource, now time.Time) []Finding {
	var out []Finding
	if st.Lease != nil {
		out = append(out, checkLease(*st.Lease))
	}
	facts := src.Facts()
	if c := st.Cluster; c != nil {
		out = append(out, checkQuorumDevice(*c), checkQuorumOverride(*c), checkMembers(st.Nodes, *c), checkCoverage(st, *c),
			checkUpgrade(*c), checkRotation(*c))
	}
	mem, memErr := src.Members()
	cs, csErr := src.Corosync()
	out = append(out,
		checkChannel(st, mem, memErr), checkAgentFirewall(facts.AgentRule), checkAgentAddress(st.Nodes, cs, csErr), checkSkew(st.Nodes), checkVersions(st.Nodes),
		checkCertificates(st.Nodes, src, now), checkCloudflareReach(facts, now), checkPinned(facts, now))
	out = append(out, checkReplicas(st, facts)...)
	return out
}

func checkLease(l engine.LeaseView) Finding {
	const check = "lease"
	age := l.RefreshAge.Round(time.Second)
	switch {
	case l.Behind != "":
		return fail(check, "the controller is behind the agents: "+l.Behind+"; nothing is written",
			"read the differences with pco promote, then confirm them or put the store right")
	case l.Fenced != "":
		return fail(check, "the controller holds the lease but writes nothing: "+l.Fenced, "pco status says why; journalctl -u pco")
	case l.RefreshAge > leaseRefreshLate:
		return warn(check, fmt.Sprintf("%s holds the lease, refreshed %s ago, more than %s", l.Holder, age, leaseRefreshLate),
			"pmxcfs answers slowly: pvecm status and journalctl -u pve-cluster on "+l.Holder)
	}
	return ok(check, fmt.Sprintf("%s holds the lease at generation %d, refreshed %s ago", l.Holder, l.Generation, age))
}

func checkQuorumDevice(c engine.ClusterView) Finding {
	const check = "quorum device"
	switch {
	case c.QuorumDevice:
		return ok(check, "the cluster has a quorum device")
	case len(c.Members) == 2:
		return warn(check, "two nodes without a quorum device: when one is lost, the other has no quorum and pco on it writes nothing",
			"add a quorum device: pvecm qdevice setup <address>")
	}
	return ok(check, fmt.Sprintf("%d nodes; a quorum device is not needed", len(c.Members)))
}

// checkQuorumOverride fails on an override of the quorum of corosync, under
// which two parts of the cluster may both hold quorum and both write.
func checkQuorumOverride(c engine.ClusterView) Finding {
	const check = "quorum override"
	var risky []string
	for _, o := range c.Overrides {
		if o == "two_node" && c.QuorumDevice {
			continue
		}
		risky = append(risky, o)
	}
	if len(risky) == 0 {
		return ok(check, "corosync.conf overrides no quorum")
	}
	return fail(check, "corosync.conf overrides the quorum: "+strings.Join(risky, ", ")+
		"; two parts of the cluster may both have quorum, and pco relies on the sentinel of its writer alone",
		"remove "+strings.Join(risky, ", ")+" from the quorum section of /etc/pve/corosync.conf")
}

// checkMembers fails for a node of the registry that Proxmox does not list:
// a node removed from the cluster that pco still counts.
func checkMembers(nodes []engine.NodeView, c engine.ClusterView) Finding {
	const check = "cluster members"
	var missing []string
	for _, n := range nodes {
		if n.State != store.NodeRemoved && !slices.Contains(c.Members, n.Name) {
			missing = append(missing, n.Name)
		}
	}
	if len(missing) == 0 {
		return ok(check, "every node of the install is a member of the cluster")
	}
	fixes := make([]string, len(missing))
	for i, n := range missing {
		fixes[i] = "pco node forget " + n
	}
	return fail(check, "the cluster does not list "+strings.Join(missing, ", ")+", which the install still counts",
		strings.Join(fixes, "; "))
}

// checkChannel fails for the nodes Proxmox lists online whose agents sent
// this controller no status: the agent channel does not reach it from them.
func checkChannel(st engine.State, mem members.Members, err error) Finding {
	const check = "agent channel"
	if errors.Is(err, ErrNotChecked) {
		return ok(check, "not checked in the appliance: "+err.Error())
	}
	if err != nil {
		return warn(check, "the members of the cluster could not be read: "+err.Error(), "pvecm status")
	}
	var silent []string
	for _, m := range mem.Nodes {
		if !m.Online || m.Name == st.Node {
			continue
		}
		i := slices.IndexFunc(st.Nodes, func(n engine.NodeView) bool { return n.Name == m.Name })
		if i >= 0 && st.Nodes[i].Build != "" && st.Nodes[i].StatusAge <= statusStale || i >= 0 && st.Nodes[i].State == store.NodeRemoved {
			continue
		}
		silent = append(silent, m.Name)
	}
	if len(silent) == 0 {
		return ok(check, "every node online sends its status")
	}
	return fail(check, fmt.Sprintf(".members lists %s online that %s sent no status: %s",
		count(len(silent), "node", "nodes"), verb(len(silent), "has", "have"), strings.Join(silent, ", ")),
		"check cluster.agentNetwork and that the datacenter firewall admits tcp/8644 from the ipset pco-nodes; "+
			"pco doctor on each of them says whether its agent reaches the controller")
}

// checkAgentFirewall fails while the controller found that the datacenter
// firewall cuts off the agents that send it no status: a rule of the agent
// channel deleted or disabled, or an address missing from its ipset, which
// leaves the node only unknown.
func checkAgentFirewall(f engine.AgentRuleFacts) Finding {
	const check = "agent firewall"
	if len(f.Silent) == 0 {
		return ok(check, "every agent sends its status; the firewall was not read for them")
	}
	agents := "the agent of " + f.Silent[0] + " sends"
	if len(f.Silent) > 1 {
		agents = "the agents of " + strings.Join(f.Silent, ", ") + " send"
	}
	switch {
	case f.Err != "":
		return warn(check, agents+" no status, and the datacenter firewall could not be read: "+printable(f.Err),
			"pvesh get /cluster/firewall/rules shows it; the token of pco reads it with Sys.Audit on /, which pco setup --repair gives it")
	case f.Gap != "":
		return fail(check, agents+" no status, and "+f.Gap, f.Fix)
	}
	why := "; pco doctor on it says why its agent sends no status"
	if len(f.Silent) > 1 {
		why = "; pco doctor on each says why its agent sends no status"
	}
	return ok(check, "the datacenter firewall admits the agent channel of "+strings.Join(f.Silent, ", ")+why)
}

// printable is s with each run of control characters, which a terminal
// would act on, made one space; text Proxmox answered with goes through it.
func printable(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsControl(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// checkAgentAddress warns for an agent that listens on a link of corosync:
// the agent channel then competes with corosync for that network.
func checkAgentAddress(nodes []engine.NodeView, cs members.Corosync, err error) Finding {
	const check = "agent address"
	if errors.Is(err, ErrNotChecked) {
		return ok(check, "not checked in the appliance: "+err.Error())
	}
	if err != nil {
		return warn(check, "corosync.conf could not be read: "+err.Error(), "pvecm status")
	}
	var links []netip.Addr
	for _, addrs := range cs.Links {
		links = append(links, addrs...)
	}
	var on []string
	for _, n := range nodes {
		if n.AgentAddress.IsValid() && slices.Contains(links, n.AgentAddress.Unmap()) {
			on = append(on, fmt.Sprintf("%s (%s)", n.Name, n.AgentAddress))
		}
	}
	if len(on) == 0 {
		return ok(check, "no agent listens on a link of corosync")
	}
	return warn(check, "the agent of "+strings.Join(on, ", ")+" listens on a link of corosync, whose traffic it then competes with",
		"set cluster.agentNetwork to another network in the settings, then restart pco on every node")
}

// checkSkew compares the clocks of the nodes with the controller's.
func checkSkew(nodes []engine.NodeView) Finding {
	const check = "clock skew"
	var worst engine.NodeView
	for _, n := range nodes {
		if n.ClockSkew.Abs() > worst.ClockSkew.Abs() {
			worst = n
		}
	}
	skew := worst.ClockSkew.Abs().Round(100 * time.Millisecond)
	fix := "check the time service on " + worst.Name + ": timedatectl; chronyc tracking"
	switch {
	case skew > nodeSkewFail:
		return fail(check, fmt.Sprintf("the clock of %s is %s off the controller's, more than %s", worst.Name, skew, nodeSkewFail), fix)
	case skew > nodeSkewWarn:
		return warn(check, fmt.Sprintf("the clock of %s is %s off the controller's, more than %s", worst.Name, skew, nodeSkewWarn), fix)
	}
	return ok(check, "the clocks of the nodes agree within "+nodeSkewWarn.String())
}

// checkVersions warns while the nodes run builds or protocols that differ,
// as during an upgrade.
func checkVersions(nodes []engine.NodeView) Finding {
	const check = "node versions"
	builds := map[string][]string{}
	protocols := map[string]bool{}
	for _, n := range nodes {
		if n.Build == "" {
			continue
		}
		builds[n.Build] = append(builds[n.Build], n.Name)
		protocols[n.Protocol] = true
	}
	if len(builds) <= 1 && len(protocols) <= 1 {
		return ok(check, "every node runs the same build")
	}
	var parts []string
	for _, b := range slices.Sorted(maps.Keys(builds)) {
		parts = append(parts, b+" on "+strings.Join(builds[b], ", "))
	}
	return warn(check, "the nodes run different builds: "+strings.Join(parts, "; "), "upgrade the nodes that run an older build")
}

// checkCertificates warns for a certificate of a node that ends within 30
// days, which its renewal should have replaced, and for the CA within a
// year.
func checkCertificates(nodes []engine.NodeView, src ClusterSource, now time.Time) Finding {
	const check = "certificates"
	var ending []string
	for _, n := range nodes {
		if !n.CertExpires.IsZero() && n.State != store.NodeRemoved && n.CertExpires.Sub(now) < leafWarn {
			ending = append(ending, fmt.Sprintf("%s's on %s", n.Name, n.CertExpires.UTC().Format(time.DateOnly)))
		}
	}
	ca, err := src.CAExpires()
	switch {
	case err != nil:
		return warn(check, "the CA of the install could not be read: "+err.Error(), "pco status; journalctl -u pco")
	case len(ending) > 0:
		return warn(check, "certificates of nodes end within 30 days: "+strings.Join(ending, ", "),
			"a node renews its own after 60 days: journalctl -u pco on it says why it did not; an appliance agent whose certificate "+
				"ended is enrolled again with a new join token (pco node forget <node>, then pco appliance uninstall and install on its node)")
	case !ca.IsZero() && ca.Sub(now) < caWarn:
		return warn(check, "the CA of the install ends on "+ca.UTC().Format(time.DateOnly)+", in less than a year",
			"a new CA comes only with the rotation of pco node forget --compromised (docs/security.md, between the nodes of a cluster); plan one before then")
	}
	return ok(check, "no certificate of a node ends within 30 days, nor the CA within a year")
}

// checkRotation says how the last rotation of the CA and the secrets of a
// node forgotten as compromised went.
func checkRotation(c engine.ClusterView) Finding {
	const check = "ca rotation"
	r := c.CARotation
	switch {
	case r == nil:
		return ok(check, "no rotation under way")
	case len(r.Refused) > 0:
		return fail(check, "after the rotation for "+r.Node+", the controller refuses the certificates of "+strings.Join(r.Refused, ", "),
			"systemctl restart pco on each of them: a node issues its certificate under the new CA when it starts")
	case r.Next != "":
		detail := fmt.Sprintf("the rotation for %s is under way since %s, at the step %s", r.Node, r.StartedAt.UTC().Format(time.RFC3339), r.Next)
		if r.Held != "" {
			detail += ", which waits: " + r.Held
		}
		return warn(check, detail, "pco cluster status shows its steps")
	}
	return ok(check, "the rotation for "+r.Node+" is finished")
}

func checkUpgrade(c engine.ClusterView) Finding {
	const check = "upgrade"
	u := c.Upgrade
	if u == nil {
		return ok(check, "no upgrade is unfinished")
	}
	detail := "the upgrade from " + u.From + " is not finished"
	if !u.Since.IsZero() {
		detail += " since " + u.Since.UTC().Format(time.DateOnly)
	}
	fix := "pco cluster finish-upgrade once every node runs this release"
	if len(u.Older) > 0 {
		detail += "; " + strings.Join(u.Older, ", ") + " still " + verb(len(u.Older), "runs", "run") + " an earlier build"
		fix = "upgrade " + strings.Join(u.Older, ", ") + ", then pco cluster finish-upgrade"
	}
	return warn(check, detail, fix)
}

// checkCoverage warns for the nodes whose forwarding-table watch flapped,
// and those without an agent that watches, with the routes that stay
// pinned to them meanwhile.
func checkCoverage(st engine.State, c engine.ClusterView) Finding {
	const check = "coverage"
	if len(c.Coverage.Flapping) == 0 && len(c.Coverage.Uncovered) == 0 {
		return ok(check, "every node has an agent whose watch runs")
	}
	var parts []string
	if len(c.Coverage.Flapping) > 0 {
		parts = append(parts, "the watch of "+strings.Join(c.Coverage.Flapping, ", ")+" was lost twice within ten minutes"+
			pinnedTo(st, c.Coverage.Flapping))
	}
	if len(c.Coverage.Uncovered) > 0 {
		parts = append(parts, strings.Join(c.Coverage.Uncovered, ", ")+" "+verb(len(c.Coverage.Uncovered), "has", "have")+
			" no agent whose watch runs, so no port another node proves counts"+pinnedTo(st, c.Coverage.Uncovered))
	}
	return warn(check, strings.Join(parts, "; "), "pco node list; journalctl -u pco on those nodes says why the watch stops")
}

// pinnedTo names the routes pinned to nodes, as ", which keeps a.example.com
// pinned".
func pinnedTo(st engine.State, nodes []string) string {
	var hosts []string
	for _, r := range st.Routes {
		if p := r.Placement; p != nil && p.Kind == engine.TunnelPinned && slices.Contains(nodes, p.Node) {
			hosts = append(hosts, r.Hostname)
		}
	}
	if len(hosts) == 0 {
		return ""
	}
	return ", which keeps " + strings.Join(hosts, ", ") + " pinned"
}

// checkReplicas warns for a shared tunnel with one ready replica, which
// the loss of that node takes down.
func checkReplicas(st engine.State, facts engine.ClusterFacts) []Finding {
	var out []Finding
	for _, t := range st.Tunnels {
		if t.Kind != "" || t.ID == "" || !t.Exists {
			continue
		}
		check := "replicas " + t.Name + " in account " + t.AccountID
		switch n := facts.ReadyReplicas[t.ID]; n {
		case 0:
			out = append(out, warn(check, "no node reports a ready connector", "pco node list; pco doctor --cluster says why on each node"))
		case 1:
			out = append(out, warn(check, "one node reports a ready connector: the loss of that node takes the tunnel down",
				"pco node list says which nodes are replicas; cluster.replicas in the settings"))
		default:
			out = append(out, ok(check, fmt.Sprintf("%d nodes report a ready connector", n)))
		}
	}
	return out
}

// checkPinned warns for a tunnel pinned to a node that served no route for
// longer than pco waits before it deletes one.
func checkPinned(facts engine.ClusterFacts, now time.Time) Finding {
	const check = "pinned tunnels"
	var idle []string
	for _, p := range facts.PinnedEmpty {
		if now.Sub(p.Since) > pinnedEmptyWarn {
			idle = append(idle, fmt.Sprintf("%s in account %s since %s", p.Name, p.AccountID, p.Since.UTC().Format(time.RFC3339)))
		}
	}
	if len(idle) == 0 {
		return ok(check, "no tunnel pinned to a node is left without routes")
	}
	return warn(check, "tunnels pinned to a node serve no route and are not deleted: "+strings.Join(idle, "; "),
		"pco status lists the problems that say why the delete does not happen")
}

// checkCloudflareReach warns when the controller's cycles have not got
// through to Cloudflare for ten minutes: another node may.
func checkCloudflareReach(facts engine.ClusterFacts, now time.Time) Finding {
	const check = "cloudflare reach"
	last := facts.CloudflareAt
	switch {
	case !last.IsZero() && now.Sub(last) <= cloudflareSilent:
		return ok(check, "the controller reached Cloudflare "+now.Sub(last).Round(time.Second).String()+" ago")
	case last.IsZero() && (facts.Started.IsZero() || now.Sub(facts.Started) <= cloudflareSilent):
		return ok(check, "the controller has not reached Cloudflare yet since it started")
	case last.IsZero():
		last = facts.Started
	}
	return warn(check, "the controller has not reached Cloudflare since "+last.UTC().Format(time.RFC3339),
		"pco status says why; if another node reaches Cloudflare, pco cluster handoff moves the controller")
}

// askAgents has the agents of the other Active nodes make their checks,
// each finding named with its node, as "cloudflared on pve2".
func askAgents(ctx context.Context, st engine.State, src ClusterSource) []Finding {
	var nodes []string
	for _, n := range st.Nodes {
		if n.State == store.NodeActive && !n.Controller && n.Name != st.Node {
			nodes = append(nodes, n.Name)
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	answers := src.Ask(ctx, nodes, AnswerWait)
	var out []Finding
	for _, node := range nodes {
		const check = "answer"
		a := answers[node]
		var f Finding
		switch {
		case !a.Answered && a.Why != "":
			f = warn(check, "the agent was not asked: "+a.Why, "run pco doctor --cluster again once the other run ended")
		case !a.Answered:
			f = warn(check, fmt.Sprintf("the agent did not answer within %s", AnswerWait), "pco doctor on "+node)
		case a.Err != "":
			f = fail(check, "the agent could not make its checks: "+a.Err, "pco doctor on "+node)
		default:
			kept, unknown, over := boundAnswer(a.Findings)
			for _, got := range kept {
				got.Check += " on " + node
				out = append(out, got)
			}
			detail := fmt.Sprintf("the agent answered with %s", count(len(a.Findings), "finding", "findings"))
			if unknown+over == 0 {
				f = ok(check, detail)
				break
			}
			var why []string
			if unknown > 0 {
				why = append(why, fmt.Sprintf("%d of no check an agent makes", unknown))
			}
			if over > 0 {
				why = append(why, fmt.Sprintf("%d over the %d an answer may carry", over, maxAnswerFindings))
			}
			f = warn(check, fmt.Sprintf("%s; %d were left out: %s", detail, unknown+over, strings.Join(why, ", ")), "pco doctor on "+node)
		}
		f.Check += " on " + node
		out = append(out, f)
	}
	return out
}

// agentCheckNames are the names of the checks an agent makes of its node
// (AgentChecks): those of the node, of its agent, and of the appliance in a
// container. An answer names no other.
func agentCheckNames() map[string]bool {
	names := map[string]bool{"doctor": true}
	for _, n := range []string{
		"cloudflared", "outbound", "proxmox", "store", "node lock", "egress", "nftables",
		"agent", "agent certificate", "controller", "agent dial",
	} {
		names[n] = true
	}
	for _, r := range applianceRuns {
		names[r.name] = true
	}
	return names
}

// boundAnswer is what the controller takes of the findings an agent
// answered with: only those of a check an agent makes, by its exact name,
// which the caller names with the node itself, so that no answer can pass a
// finding off as one of another node or of the controller; at most
// maxAnswerFindings, each text clipped to maxAnswerText bytes and each level
// one of the three. unknown and over count those left out.
func boundAnswer(findings []Finding) (kept []Finding, unknown, over int) {
	names := agentCheckNames()
	for _, f := range findings {
		if !names[f.Check] {
			unknown++
			continue
		}
		if len(kept) == maxAnswerFindings {
			over++
			continue
		}
		switch f.Level {
		case LevelOK, LevelWarn, LevelFail:
		default:
			f.Level = LevelWarn
		}
		f.Detail, f.Fix = clipText(f.Detail), clipText(f.Fix)
		kept = append(kept, f)
	}
	return kept, unknown, over
}

// clipText cuts s to maxAnswerText bytes, at a whole character.
func clipText(s string) string {
	if len(s) <= maxAnswerText {
		return s
	}
	return strings.ToValidUTF8(s[:maxAnswerText], "")
}
