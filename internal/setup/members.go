package setup

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ensureCluster records what identifies the cluster the node is in, finds
// the address the node's agent listens on, gives the install its CA when it
// is the first node and has none, and issues or keeps the node's own
// certificates. It runs before anything in Proxmox is touched, so a node
// that cannot join changes nothing there.
func (r *run) ensureCluster(ctx context.Context) error {
	m, err := members.ReadFiles(members.Files{
		Members: r.host.members, Corosync: r.host.corosync, LocalCorosync: r.host.localCorosync,
	}, r.now())
	if err != nil {
		return fmt.Errorf("reading who is in the cluster: %w", err)
	}
	if err := r.ensureIdentity(m); err != nil {
		return err
	}
	if err := r.chooseAgentAddress(ctx, m); err != nil {
		return err
	}
	ca, err := r.ensureCA()
	if err != nil {
		return err
	}
	return r.ensureCertificates(ca)
}

// readOthers reads which other nodes run the install: the registered ones
// that were not removed. A node joins them.
func (r *run) readOthers() error {
	nodes, err := r.st.Nodes()
	if err != nil {
		return fmt.Errorf("reading the node registry: %w", err)
	}
	r.others = nil
	for _, n := range nodes {
		if n.Name != r.o.Node && n.State != string(agentproto.Removed) {
			r.others = append(r.others, n.Name)
		}
	}
	slices.Sort(r.others)
	return nil
}

// ensureIdentity records the cluster the store was made in, by the
// fingerprint of the cluster CA of Proxmox VE, which is made once and never
// rotated. A store that names another cluster was copied here, and is left
// as it is: the daemon writes nothing of it until an admin promotes this
// cluster. A record made on a node that stood alone gets the name of the
// cluster the node is now in.
func (r *run) ensureIdentity(m members.Members) error {
	fingerprint, err := members.CAFingerprint(r.host.pveRootCA)
	if err != nil {
		return fmt.Errorf("reading the cluster CA of Proxmox VE, which identifies the cluster: %w", err)
	}
	inst := r.install
	switch {
	case inst.Cluster == nil:
		inst.Cluster = &store.ClusterIdentity{CASHA256: fingerprint, Name: m.Cluster}
		if err := r.st.SaveInstall(inst); err != nil {
			return fmt.Errorf("recording the cluster identity: %w", err)
		}
		if m.Cluster == "" {
			r.ask.Info("cluster: recorded the identity of this node, which stands alone (%s)", fingerprint)
		} else {
			r.ask.Info("cluster: recorded the identity of cluster %s (%s)", m.Cluster, fingerprint)
		}
	case inst.Cluster.CASHA256 != fingerprint && !r.o.Recover && !r.o.NewInstall:
		// A node set up into a store of another cluster would register,
		// enrol and open the firewall for an install whose writer is
		// elsewhere: the admin decides first.
		name := inst.Cluster.Name
		if name == "" {
			name = "without a name"
		}
		return fmt.Errorf("the store was made in cluster %s (%s), not in this one (%s): run pco promote on a node "+
			"that runs pco to make this cluster its writer, or pco setup --recover to adopt the install from Cloudflare; "+
			"nothing was changed", name, inst.Cluster.CASHA256, fingerprint)
	case inst.Cluster.Name == "" && m.Cluster != "":
		inst.Cluster.Name = m.Cluster
		if err := r.st.SaveInstall(inst); err != nil {
			return fmt.Errorf("recording the cluster name: %w", err)
		}
		r.ask.Info("cluster: the install, made on a node that stood alone, is now in cluster %s", m.Cluster)
	}
	r.install = inst
	return nil
}

// chooseAgentAddress finds the one address the node's agent listens on and
// is dialled at. With cluster.agentNetwork set it is the node's address in
// that network: the one pmxcfs lists when it is in it, else the one address
// of the node's interfaces in it; two or more are refused rather than one
// guessed. Without the setting it is the address pmxcfs lists for the node,
// which is the one its name resolves to, else, on a node that corosync never
// listed, the address of the cluster status.
func (r *run) chooseAgentAddress(ctx context.Context, m members.Members) error {
	settings, err := r.st.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	var listed netip.Addr
	for _, n := range m.Nodes {
		if n.Name == r.o.Node && n.IP.IsValid() {
			listed = n.IP
		}
	}
	if listed.IsValid() {
		if why := unusableAgentAddress(listed); why != "" {
			return fmt.Errorf("%s lists %s for node %s, which is %s: the agent cannot listen there", r.host.members, listed, r.o.Node, why)
		}
	}
	if p := settings.Cluster.AgentNetwork; p.IsValid() {
		return r.agentAddressIn(p, listed)
	}
	if listed.IsValid() {
		r.agentAddress = listed
		return nil
	}
	addrs, err := r.nodeAddrs(ctx)
	if err != nil {
		return err
	}
	chosen, err := pickAgentAddress(addrs)
	if err != nil {
		return fmt.Errorf("neither %s nor the cluster status lists an address of node %s the agent can listen on: %w", r.host.members, r.o.Node, err)
	}
	r.agentAddress = chosen
	return nil
}

// pickAgentAddress chooses, among the addresses of the node, one the agent
// can listen on: an IPv4 one where there is one, an IPv6 one otherwise.
func pickAgentAddress(addrs []netip.Addr) (netip.Addr, error) {
	var usable []netip.Addr
	var why []string
	for _, a := range addrs {
		if w := unusableAgentAddress(a); w != "" {
			why = append(why, a.String()+" is "+w)
			continue
		}
		usable = append(usable, a)
	}
	if len(usable) == 0 {
		if len(why) == 0 {
			return netip.Addr{}, errors.New("none listed")
		}
		return netip.Addr{}, errors.New(strings.Join(why, ", "))
	}
	slices.SortStableFunc(usable, func(a, b netip.Addr) int { return cmp.Compare(family(a), family(b)) })
	return usable[0], nil
}

// family orders IPv4 before IPv6.
func family(a netip.Addr) int {
	if a.Is4() {
		return 4
	}
	return 6
}

// unusableAgentAddress says why a is no address the agent can listen on and
// be dialled at from the other nodes, or nothing when it is one.
func unusableAgentAddress(a netip.Addr) string {
	switch {
	case a.IsLoopback():
		return "a loopback address"
	case a.IsUnspecified():
		return "the unspecified address"
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast():
		return "a link-local address"
	case a.IsMulticast():
		return "a multicast address"
	}
	return ""
}

// agentAddressIn chooses the node's address in the agent network p, the one
// pmxcfs lists for it, listed, first.
func (r *run) agentAddressIn(p netip.Prefix, listed netip.Addr) error {
	if listed.IsValid() && p.Contains(listed) {
		r.agentAddress = listed
		return nil
	}
	addrs, err := r.host.addrs()
	if err != nil {
		return fmt.Errorf("reading the addresses of the node: %w", err)
	}
	var in []netip.Addr
	for _, a := range addrs {
		if p.Contains(a) && unusableAgentAddress(a) == "" && !slices.Contains(in, a) {
			in = append(in, a)
		}
	}
	switch len(in) {
	case 0:
		return fmt.Errorf("this node has no address in cluster.agentNetwork %s (it has %s): give it one, or change the setting",
			p, joinAddrs(addrs))
	case 1:
		r.agentAddress = in[0]
		return nil
	}
	return fmt.Errorf("this node has %d addresses in cluster.agentNetwork %s (%s), and the agent listens on one: "+
		"leave one of them in it, or narrow the setting", len(in), p, joinAddrs(in))
}

func joinAddrs(addrs []netip.Addr) string {
	if len(addrs) == 0 {
		return "none"
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return strings.Join(out, ", ")
}
