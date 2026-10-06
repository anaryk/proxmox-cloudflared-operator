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
// name. A field the daemon adds has none until it is given
// one here, and the test of this table fails meanwhile: nothing new reaches
// a reader unseen. The networking milestone adds "networks" and "managed",
// filtered by cases of their own.
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
	// free text is not reliable, and the documentation says so.
	"problems":      kept,
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
}

// routeFields are the fields of a route of the state: a reader gets a route
// whole, or not at all. reason is free text that may name the holder of a
// hostname, as the routes table shows it to an admin.
var routeFields = map[string]decision{
	"hostname": kept, "owner": kept, "state": kept, "level": kept, "reason": kept, "service": kept,
	"zone": kept, "warnings": kept, "guest": kept, "candidates": kept, "accountId": kept, "rule": kept, "path": kept,
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
