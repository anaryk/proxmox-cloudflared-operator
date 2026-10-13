package managednet

import (
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// The firewall backends of a node.
const (
	BackendIPTables = "pve-firewall"
	BackendNFTables = "proxmox-firewall"
)

// FilteredInput is what the predicate of the filtered level reads, all of it
// from one cycle: the node's firewall, every card on the managed network, and
// what the cycle found of the vnet and of frames from unknown MACs.
type FilteredInput struct {
	Node string
	// The firewall options of the node and of the datacenter, and the state
	// of pve-firewall on the node, each with the error of its read.
	NodeFirewall       pve.NodeFirewallOptions
	NodeFirewallErr    error
	ClusterFirewall    pve.ClusterFirewallOptions
	ClusterFirewallErr error
	Service            string
	ServiceErr         error
	// Foreign are the cards on the vnet that no attachment explains, as
	// Classify lists them: a copy, a card with a lease's MAC on another
	// guest, a card with the MAC of the leg.
	Foreign []ForeignNIC
	// Cards are the attachments' cards that are on the vnet now.
	Cards []FilteredCard
	// Vnet are the reasons the vnet is not as pco made it; none when it is.
	Vnet []string
	// Unknown are the MACs the source-MAC watch saw on the vnet since the
	// last judgment and no lease knows.
	Unknown []string
	// Unread says what the cycle could not read, without which nothing is
	// proven: the attachments, the leases.
	Unread []string
}

// FilteredCard is an attachment's card on the vnet with what the cycle read
// and found of it.
type FilteredCard struct {
	Owner string
	Card  FirewallCard
	// Firewall says that the card has firewall=1.
	Firewall bool
	Running  bool
	// Read is the guest's firewall as read for the card, nil when it could
	// not be read, and ReadErr why.
	Read    *GuestFirewall
	ReadErr error
	// Canary is the probe of a running card, nil when none was made.
	Canary *CanaryResult
}

// FilteredResult is the verdict: whether every guest on the vnet is isolated
// by its firewall, whether the level is offered on the node's backend at
// all, the backend, and why it does not hold, in the order found.
type FilteredResult struct {
	Holds   bool
	Offered bool
	Backend string
	Why     []string
}

// Filtered is the predicate of the filtered level in the appliance, pure: the
// node runs the iptables backend, on which alone the guest rules are
// installed; the datacenter firewall is on; pve-firewall runs on the node;
// no card on the vnet is foreign; each attachment's card has firewall=1, its
// guest firewall is on with macfilter on, its ipset holds exactly the lease,
// pco's rules are first on it as written, and a running one stays silent to
// the canary; the vnet is as pco made it; and no unknown MAC was seen.
func Filtered(in FilteredInput, comment string) FilteredResult {
	res := FilteredResult{Offered: true, Backend: BackendIPTables}
	why := func(format string, args ...any) { res.Why = append(res.Why, fmt.Sprintf(format, args...)) }
	switch {
	case in.NodeFirewallErr != nil:
		res.Backend = ""
		why("the firewall options of node %s could not be read: %v", in.Node, in.NodeFirewallErr)
	case in.NodeFirewall.NFTables:
		return FilteredResult{Backend: BackendNFTables, Why: []string{fmt.Sprintf(
			"filtered is not supported with the nftables firewall backend (nftables: 1) of node %s, which installs no guest rules; "+
				"set nftables: 0 in its firewall options", in.Node)}}
	}
	switch {
	case in.ClusterFirewallErr != nil:
		why("the firewall options of the datacenter could not be read: %v", in.ClusterFirewallErr)
	case !in.ClusterFirewall.Enabled:
		why("the datacenter firewall is off")
	}
	switch {
	case in.ServiceErr != nil:
		why("the state of %s on node %s could not be read: %v", BackendIPTables, in.Node, in.ServiceErr)
	case in.Service != "running":
		why("%s is %s on node %s", BackendIPTables, in.Service, in.Node)
	}
	res.Why = append(res.Why, in.Unread...)
	for _, f := range in.Foreign {
		why("guest %s: net%d (%s) on the vnet is no card pco attached", f.Owner, f.NIC, f.MAC)
	}
	for _, c := range in.Cards {
		for _, w := range cardWhy(c, comment) {
			why("guest %s: %s", c.Owner, w)
		}
	}
	for _, w := range in.Vnet {
		why("the vnet %s: %s", gateway.ManagedBridge, w)
	}
	for _, mac := range in.Unknown {
		why("a frame from %s, a MAC no lease knows, was seen on the vnet", mac)
	}
	res.Holds = len(res.Why) == 0
	return res
}

// cardWhy says why one attachment's card is not isolated.
func cardWhy(c FilteredCard, comment string) []string {
	var why []string
	iface := c.Card.Iface()
	if !c.Firewall {
		why = append(why, iface+" has firewall=0")
	}
	switch {
	case c.Read == nil && c.ReadErr != nil:
		why = append(why, "its firewall could not be read: "+c.ReadErr.Error())
	case c.Read == nil:
		why = append(why, "its firewall was not read in this cycle")
	default:
		why = append(why, c.Read.Drift(c.Card, comment)...)
	}
	switch {
	case !c.Running:
	case c.Canary == nil:
		why = append(why, "no canary was sent to "+iface)
	case c.Canary.Verdict == CanaryAnswered:
		why = append(why, fmt.Sprintf("the firewall is not enforcing on %s: %s on port %d", iface, c.Canary.Detail, c.Canary.Port))
	case c.Canary.Verdict != CanarySilent:
		why = append(why, fmt.Sprintf("the canary on %s could not be sent: %s", iface, c.Canary.Detail))
	}
	return why
}
