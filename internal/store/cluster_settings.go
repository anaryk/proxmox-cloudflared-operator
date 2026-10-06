package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
)

const (
	// MaxReplicas is the most nodes the shared tunnel runs on.
	MaxReplicas = 25

	defaultAdmissionMaxMoves = 10
	maxAdmissionMaxMoves     = 1000
)

// ClusterSettings are the settings of a cluster of nodes.
//
// The previous release decodes the settings strictly and refuses a file with
// a key it does not know. The block is therefore left out of the file while
// every field has its default (IsZero), so that settings saved by this
// release without a cluster setting are the file the previous release reads.
type ClusterSettings struct {
	// AgentNetwork is the network whose address of a node its agent listens
	// on and dials from; unset is the address the node's name resolves to.
	AgentNetwork netip.Prefix `json:"agentNetwork,omitzero"`
	// Replicas are the nodes that run the shared tunnel; none is every
	// Active node.
	Replicas []string `json:"replicas,omitempty"`
	// FollowMaintenance drains a node while Proxmox has it in maintenance;
	// unset is true.
	FollowMaintenance *bool `json:"followMaintenance,omitempty"`
	// RotateOnForget rotates the shared tunnel's secret when a node is
	// forgotten; unset is true.
	RotateOnForget *bool `json:"rotateOnForget,omitempty"`
	// AdmissionMaxMoves is how many routes may move for a node to be
	// admitted for routing: one that would move more stays joining. 0 is the
	// default, 10.
	AdmissionMaxMoves int `json:"admissionMaxMoves,omitempty"`
}

// FollowsMaintenance reports whether a node in maintenance is drained.
func (c ClusterSettings) FollowsMaintenance() bool {
	return c.FollowMaintenance == nil || *c.FollowMaintenance
}

// RotatesOnForget reports whether forgetting a node rotates the secret of the
// shared tunnel.
func (c ClusterSettings) RotatesOnForget() bool { return c.RotateOnForget == nil || *c.RotateOnForget }

// MaxMoves is how many moves the admission of a node may make.
func (c ClusterSettings) MaxMoves() int {
	if c.AdmissionMaxMoves == 0 {
		return defaultAdmissionMaxMoves
	}
	return c.AdmissionMaxMoves
}

// IsZero reports whether every field has its default, which leaves the block
// out of the settings file (the json tag's omitzero asks it).
func (c ClusterSettings) IsZero() bool {
	return c.AgentNetwork == (netip.Prefix{}) && len(c.Replicas) == 0 &&
		c.FollowsMaintenance() && c.RotatesOnForget() && c.MaxMoves() == defaultAdmissionMaxMoves
}

// defaultClusterSettings are the cluster settings of a new install, with
// every default spelt out.
func defaultClusterSettings() ClusterSettings {
	yes, alsoYes := true, true
	return ClusterSettings{FollowMaintenance: &yes, RotateOnForget: &alsoYes, AdmissionMaxMoves: defaultAdmissionMaxMoves}
}

// UnmarshalJSON reads the cluster settings strictly, whatever the decoder
// that calls it, as ManagedSettings does: a key that is not there takes its
// default, one that is there is taken as written and checked here, so that a
// zero or empty value is refused and never read as the default. A null is
// the block left out.
func (c *ClusterSettings) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*c = defaultClusterSettings()
		return nil
	}
	var in struct {
		AgentNetwork      *string   `json:"agentNetwork"`
		Replicas          *[]string `json:"replicas"`
		FollowMaintenance *bool     `json:"followMaintenance"`
		RotateOnForget    *bool     `json:"rotateOnForget"`
		AdmissionMaxMoves *int      `json:"admissionMaxMoves"`
	}
	if err := decode(data, &in, true); err != nil {
		var typ *json.UnmarshalTypeError
		if errors.As(err, &typ) && typ.Field != "" {
			named := *typ
			named.Field = "cluster." + typ.Field
			return &named
		}
		return err
	}
	out := defaultClusterSettings()
	if in.AgentNetwork != nil {
		if *in.AgentNetwork == "" {
			return invalid("cluster.agentNetwork", "cluster.agentNetwork is empty: leave it out for the address each node's name resolves to")
		}
		p, err := netip.ParsePrefix(*in.AgentNetwork)
		if err != nil {
			return invalid("cluster.agentNetwork", "cluster.agentNetwork %q: want a prefix such as 10.99.0.0/24", *in.AgentNetwork)
		}
		out.AgentNetwork = p
	}
	if in.Replicas != nil {
		out.Replicas = *in.Replicas
	}
	if in.FollowMaintenance != nil {
		out.FollowMaintenance = in.FollowMaintenance
	}
	if in.RotateOnForget != nil {
		out.RotateOnForget = in.RotateOnForget
	}
	if in.AdmissionMaxMoves != nil {
		out.AdmissionMaxMoves = *in.AdmissionMaxMoves
		if out.AdmissionMaxMoves == 0 {
			return invalid("cluster.admissionMaxMoves", "cluster.admissionMaxMoves 0: from 1 to %d", maxAdmissionMaxMoves)
		}
	}
	n, err := out.normalized()
	if err != nil {
		return err
	}
	*c = n
	return nil
}

// normalized returns c with every field it leaves unset at its default, or a
// *FieldError naming the first field that is invalid.
func (c ClusterSettings) normalized() (ClusterSettings, error) {
	p := c.AgentNetwork
	switch {
	case p == (netip.Prefix{}):
	case !p.IsValid():
		return ClusterSettings{}, invalid("cluster.agentNetwork", "cluster.agentNetwork %s: want an IPv4 or IPv6 prefix", p)
	case p != p.Masked():
		return ClusterSettings{}, invalid("cluster.agentNetwork", "cluster.agentNetwork %s: want the address of the network, %s", p, p.Masked())
	}
	if len(c.Replicas) > MaxReplicas {
		return ClusterSettings{}, invalid("cluster.replicas", "cluster.replicas: %d nodes, at most %d", len(c.Replicas), MaxReplicas)
	}
	for i, node := range c.Replicas {
		field := fmt.Sprintf("cluster.replicas[%d]", i)
		if err := checkNodeName(field, node); err != nil {
			return ClusterSettings{}, &FieldError{Field: field, Err: err}
		}
		if slices.Contains(c.Replicas[:i], node) {
			return ClusterSettings{}, invalid(field, "%s: node %s is named twice", field, node)
		}
	}
	if m := c.AdmissionMaxMoves; m < 0 || m > maxAdmissionMaxMoves {
		return ClusterSettings{}, invalid("cluster.admissionMaxMoves", "cluster.admissionMaxMoves %d: from 1 to %d", m, maxAdmissionMaxMoves)
	}
	d := defaultClusterSettings()
	if len(c.Replicas) == 0 {
		c.Replicas = nil
	} else {
		c.Replicas = slices.Clone(c.Replicas)
	}
	if c.FollowMaintenance == nil {
		c.FollowMaintenance = d.FollowMaintenance
	}
	if c.RotateOnForget == nil {
		c.RotateOnForget = d.RotateOnForget
	}
	if c.AdmissionMaxMoves == 0 {
		c.AdmissionMaxMoves = d.AdmissionMaxMoves
	}
	return c, nil
}
