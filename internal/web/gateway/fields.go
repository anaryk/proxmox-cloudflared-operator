package gateway

// action is what a reader gets of a field.
type action int

const (
	keep   action = iota // as it is
	filter               // what the case of the same name leaves of it
	remove               // nothing: the case of the same name empties it
)

// decision is what readers get of one field, and the case of stateCases
// that sees to it.
type decision struct {
	action action
	by     string
}

var kept = decision{action: keep}

func filteredBy(name string) decision { return decision{action: filter, by: name} }
func removedBy(name string) decision  { return decision{action: remove, by: name} }

// stateFields is the decision for every field of engine.State, by its JSON
// name: a reader sees what belongs to the guests it may audit and the
// configuration of the cluster, which it holds Sys.Audit on, and nothing an
// admin alone acts on. A field the daemon adds has none until it is given
// one here, and the test of this table fails meanwhile: nothing new reaches
// a reader unseen.
var stateFields = map[string]decision{
	"at":          kept,
	"finishedAt":  kept,
	"node":        kept,
	"digest":      kept, // the page compares it with the digest of the state notices
	"mode":        kept,
	"complete":    kept,
	"routes":      filteredBy("routes"), // by guest; manual routes stay
	"issues":      filteredBy("issues"), // by guest; those of the settings stay
	"tunnels":     kept,                 // cluster configuration
	"connectors":  kept,
	"credentials": kept, // no token, and no guest
	"zones":       kept,
	"actions":     filteredBy("actions"),   // by hostname; tunnel actions stay
	"conflicts":   filteredBy("conflicts"), // by hostname
	"lost":        filteredBy("lost"),      // by hostname
	// problems and hold are operational text, shown as they are: filtering
	// free text is not reliable, and the documentation says so. The lines
	// of the managed network lose the MACs they name.
	"problems":      filteredBy("problems"),
	"writerVerdict": kept,
	"hold":          kept,
	"profile":       kept,
	// Readers cannot confirm, and what waits names guests.
	"waiting":    removedBy("waiting"),
	"offer":      removedBy("offer"),
	"unapproved": filteredBy("unapproved"), // by guest
	// A segment is a bridge and a VLAN with a count of routes: no guest.
	"segments":        kept,
	"egress":          kept,
	"admission":       kept,
	"gateTagged":      kept,
	"rogueConnectors": kept, // a tunnel's, not a guest's; the problem lines name them already
	// The copies and tenants of the appliance are guests; its own VMID and
	// the principals that reach into it are the appliance's.
	"identity":     filteredBy("identity"),
	"epochDrawnAt": kept,
	// A network is a bridge, a prefix and the leg of pco in it, with counts
	// of what it serves: cluster configuration, no guest. The MACs the legs
	// show on their LANs are for an admin.
	"networks": filteredBy("networks"),
	// The managed network names guests: their cards, those that wait and
	// the foreign cards, each by guest; the MACs, identities and pveum lines
	// are an admin's.
	"managed": filteredBy("managed"),
	// The lease names nodes, their addresses and the epoch: the cluster's.
	"lease": kept,
	// The nodes are the cluster's, but the uses of a join token name the
	// sources and containers of the joins, and an appliance agent's address
	// is that of a guest.
	"nodes": filteredBy("nodes"),
	// The cluster is its own, but a block names a guest and its MAC.
	"cluster": filteredBy("cluster"),
}

// leaseFields are the fields of the lease, which a reader gets whole.
var leaseFields = map[string]decision{
	"holder": kept, "address": kept, "previous": kept, "generation": kept, "nonce": kept, "acquiredAt": kept,
	"refreshAge": kept, "valid": kept, "fenced": kept, "behind": kept, "handoff": kept,
}

// nodeFields are the fields of a node: a reader gets every node, without
// what the enrolment of an appliance agent says of its container.
var nodeFields = map[string]decision{
	"name": kept, "state": kept, "stateSince": kept, "drained": kept, "kind": kept, "vmid": kept, "index": kept,
	"controller": kept, "build": kept, "protocol": kept, "statusAge": kept, "watchRunning": kept, "covered": kept,
	"replicaReady": kept, "verified": kept, "failing": kept, "belowMinimum": kept, "pinned": kept,
	"certExpires": kept, "clockSkew": kept,
	"agentAddress": filteredBy("nodes"), // a host's stays; an appliance agent's is a container's
	"joinUses":     removedBy("nodes"),
	"contested":    removedBy("nodes"),
}

// clusterFields are the fields of the cluster: the blocks are those of the
// guests a reader sees.
var clusterFields = map[string]decision{
	"controller": kept, "generation": kept, "members": kept, "quorate": kept, "quorumDevice": kept,
	"overrides": kept, "coverage": kept, "upgrade": kept, "caRotation": kept,
	"blocks": filteredBy("cluster"),
}

// blockFields are the fields of a block: a reader gets one of a guest it
// sees, without the MAC and without the port it was seen on, which is
// another guest's.
var blockFields = map[string]decision{
	"address": kept, "guest": kept, "node": kept, "since": kept, "lastReport": kept,
	"mac":  removedBy("cluster"),
	"port": removedBy("cluster"),
}

// routeFields are the fields of a route of the state: a reader gets a route
// whole, or not at all. reason is free text that may name the holder of a
// hostname, as the routes table shows it to an admin.
var routeFields = map[string]decision{
	"hostname": kept, "owner": kept, "state": kept, "level": kept, "reason": kept, "service": kept,
	"zone": kept, "warnings": kept, "guest": kept, "candidates": kept, "accountId": kept, "rule": kept, "path": kept,
	"replicas": kept, "placement": kept,
}

// unapprovedFields are the fields of a guest that waits for approval: a
// reader gets it whole, when it sees the guest.
var unapprovedFields = map[string]decision{
	"kind": kept, "vmid": kept, "name": kept, "identity": kept, "hostnames": kept, "why": kept, "macs": kept, "addresses": kept,
}

// waitingFields are the fields of what waits for a confirmation, which no
// reader gets.
var waitingFields = map[string]decision{
	"kind": {action: remove}, "subject": {action: remove}, "detail": {action: remove}, "items": {action: remove},
}

// identityFields are the fields of the appliance's identity.
var identityFields = map[string]decision{
	"vmid": kept, "node": kept, "ok": kept, "copy": kept, "exposed": kept, "checkedAt": kept,
	"why":     kept, // free text, as the problems are
	"copies":  filteredBy("identity"),
	"tenants": filteredBy("identity"),
}

// claimFields are the fields of a claim (GET /claims): a reader gets the
// claims of the holders it sees, with the claimants it sees.
var claimFields = map[string]decision{
	"hostname": kept, "holder": kept, "guest": kept, "since": kept, "missingSince": kept, "state": kept,
	"waiting": filteredBy("claims"),
}

// claimantFields are the fields of an owner that waits for a hostname: a
// reader gets one whole, when it sees the owner.
var claimantFields = map[string]decision{"owner": kept, "guest": kept, "since": kept}

// approvalFields are the fields of an approval (GET /approvals): a reader
// gets one whole, when it sees the guest.
var approvalFields = map[string]decision{
	"owner": kept, "guest": kept, "identity": kept, "current": kept, "matches": kept, "macs": kept, "addresses": kept,
}

// guestFields are the fields of a guest of GET /guests: a reader gets one
// whole, when it sees the guest.
var guestFields = map[string]decision{
	"ref": kept, "name": kept, "node": kept, "running": kept, "tagged": kept, "identity": kept,
	"approval": kept, "routes": kept, "issues": kept,
}

// routeTrafficFields are the fields of the figure of a route: a reader gets
// those of the routes it sees, with shared counting only those.
var routeTrafficFields = map[string]decision{
	"hostname": kept, "owner": kept, "target": kept, "flowsPerSec": kept, "stale": kept,
	"shared": filteredBy("traffic"),
}
