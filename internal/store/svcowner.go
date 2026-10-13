package store

// ServiceOwner is whom a service address was made for: the owner of the
// route, the guest it reaches and that guest's identity, which tells it apart
// from a later guest that reuses its VMID. A rule at Cloudflare that names a
// service address may outlive the route, as when the write that removes it
// fails, so the address must never forward to anyone else that takes the
// real address later. A route with no guest has neither Guest nor Identity.
// A mapping stored before owners were kept has none of the three.
type ServiceOwner struct {
	Owner    string `json:"owner,omitempty"`
	Guest    string `json:"guest,omitempty"`
	Identity string `json:"identity,omitempty"`
}

// Known reports whether o names an owner: a mapping stored before owners
// were kept does not.
func (o ServiceOwner) Known() bool { return o.Owner != "" }

// Name names the owner and, when it is another, the guest. It is no String
// method, which every mapping that embeds o would take for its own.
func (o ServiceOwner) Name() string {
	if o.Guest == "" || o.Guest == o.Owner {
		return o.Owner
	}
	return o.Owner + " (guest " + o.Guest + ")"
}
