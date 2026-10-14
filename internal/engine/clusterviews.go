package engine

import (
	"cmp"
	"net/netip"
	"slices"
	"time"
)

// LeaseView is the lease of the host profile as the controller holds it, or
// as another node last read it. The appliance has none.
type LeaseView struct {
	// Holder is the node that holds the lease, and Address its agent's
	// address. Previous is the node that held it before, while a banner says
	// the controller moved.
	Holder     string     `json:"holder"`
	Address    netip.Addr `json:"address,omitzero"`
	Previous   string     `json:"previous,omitempty"`
	Generation int        `json:"generation"`
	Nonce      string     `json:"nonce,omitempty"`
	AcquiredAt time.Time  `json:"acquiredAt,omitzero"`
	// RefreshAge is how long ago the last refresh that succeeded started.
	RefreshAge time.Duration `json:"refreshAge"`
	Valid      bool          `json:"valid"`
	// Fenced says why the holder writes nothing although it holds the
	// lease; Behind why the controller is behind the agents, until pco
	// promote; Handoff the node a hand-off goes to.
	Fenced  string `json:"fenced,omitempty"`
	Behind  string `json:"behind,omitempty"`
	Handoff string `json:"handoff,omitempty"`
}

// NodeView is a node of the install's registry, with what its agent last
// reported.
type NodeView struct {
	Name string `json:"name"`
	// State is joining, active, draining, unknown, down, contested or
	// removed; Drained the reason of an intent to drain, admin or
	// maintenance, which outlives the node's absence.
	State      string    `json:"state"`
	StateSince time.Time `json:"stateSince,omitzero"`
	Drained    string    `json:"drained,omitempty"`
	// Kind is the kind of the node's agent, host or appliance, and VMID
	// the container of an appliance agent.
	Kind string `json:"kind"`
	VMID int    `json:"vmid,omitempty"`
	// Index numbers the node's leg MACs; nil before routing admission.
	Index        *int       `json:"index,omitempty"`
	AgentAddress netip.Addr `json:"agentAddress,omitzero"`
	Controller   bool       `json:"controller"`
	Build        string     `json:"build,omitempty"`
	Protocol     string     `json:"protocol,omitempty"`
	// StatusAge is how long ago the controller received the node's last
	// status.
	StatusAge    time.Duration `json:"statusAge"`
	WatchRunning bool          `json:"watchRunning"`
	Covered      bool          `json:"covered"`
	ReplicaReady bool          `json:"replicaReady"`
	// Verified, Failing and BelowMinimum count the targets of the node's
	// snapshot by how they fare there; Pinned the routes pinned to it.
	Verified     int           `json:"verified"`
	Failing      int           `json:"failing"`
	BelowMinimum int           `json:"belowMinimum"`
	Pinned       int           `json:"pinned"`
	CertExpires  time.Time     `json:"certExpires,omitzero"`
	ClockSkew    time.Duration `json:"clockSkew"`
	// JoinUses are the uses of the join token an appliance agent enrolled
	// with, which an admin reads before admitting it; Contested those that
	// made the node contested.
	JoinUses  []JoinUseView `json:"joinUses,omitempty"`
	Contested []JoinUseView `json:"contested,omitempty"`
}

// JoinUseView is one use of a join token: when, from where, and the
// container and MACs it said it was.
type JoinUseView struct {
	At     time.Time  `json:"at"`
	Source netip.Addr `json:"source,omitzero"`
	VMID   int        `json:"vmid,omitempty"`
	MACs   []string   `json:"macs,omitempty"`
}

// ReplicaView is how a route's target fares on one replica of its tunnel.
type ReplicaView struct {
	Node string `json:"node"`
	// Level is the level proven on that node; Attested says the port was
	// proven by the hosting node's forwarding table, port (attested).
	Level    string `json:"level,omitempty"`
	Attested bool   `json:"attested,omitempty"`
	OK       bool   `json:"ok"`
	Reason   string `json:"reason,omitempty"`
	InSet    bool   `json:"inSet"`
}

// The kinds of PlacementView and of a tunnel.
const (
	TunnelShared = "shared"
	TunnelPinned = "pinned"
)

// PlacementView is the tunnel a route is placed on: the shared one, or the
// one pinned to a node, with why it is pinned and a move under way.
type PlacementView struct {
	Tunnel string    `json:"tunnel"`
	Kind   string    `json:"kind"`
	Node   string    `json:"node,omitempty"`
	Why    string    `json:"why,omitempty"`
	Move   *MoveView `json:"move,omitempty"`
}

// MoveView is a move of a route's hostname between tunnels: from and to by
// tunnel name, and the step it reached.
type MoveView struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	Step  string    `json:"step"`
	Since time.Time `json:"since,omitzero"`
}

// ClusterView is the cluster as the controller's last cycle found it.
type ClusterView struct {
	Controller string `json:"controller"`
	Generation int    `json:"generation"`
	// Members are the nodes Proxmox lists, by name.
	Members      []string `json:"members"`
	Quorate      bool     `json:"quorate"`
	QuorumDevice bool     `json:"quorumDevice"`
	// Overrides are the quorum overrides of corosync.conf, as two_node or
	// expected_votes, under which pco relies on its sentinel alone.
	Overrides  []string        `json:"overrides,omitempty"`
	Coverage   CoverageView    `json:"coverage"`
	Upgrade    *UpgradeView    `json:"upgrade,omitempty"`
	CARotation *CARotationView `json:"caRotation,omitempty"`
	Blocks     []BlockView     `json:"blocks,omitempty"`
}

// CoverageView says whether every node has an agent with a running watch,
// so that another node's proof of a port counts.
type CoverageView struct {
	Covered bool `json:"covered"`
	// Since is when coverage was last lost or regained, and PromoteAt when
	// the routes it demoted are shared again, while coverage holds.
	Since     time.Time `json:"since,omitzero"`
	PromoteAt time.Time `json:"promoteAt,omitzero"`
	// Uncovered are the nodes without such an agent; Flapping those that
	// lost it twice in ten minutes.
	Uncovered []string `json:"uncovered,omitempty"`
	Flapping  []string `json:"flapping,omitempty"`
}

// UpgradeView is an upgrade from an earlier release that pco cluster
// finish-upgrade has not finished.
type UpgradeView struct {
	From  string    `json:"from"`
	Since time.Time `json:"since,omitzero"`
	// Older are the nodes that run a build that cannot finish it.
	Older []string `json:"older,omitempty"`
}

// CARotationView is the rotation of every secret a node forgotten as
// compromised held, under way or last made.
type CARotationView struct {
	Node string `json:"node"`
	// Also are the nodes forgotten as compromised while it ran, which
	// joined it.
	Also      []string  `json:"also,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	Done      []string  `json:"done,omitempty"`
	// Next is the step it waits on; empty once finished.
	Next       string    `json:"next,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	// Refused are the nodes whose certificate the new CA refused.
	Refused []string `json:"refused,omitempty"`
	// Held says why a step the CA waits on cannot be done.
	Held string `json:"held,omitempty"`
}

// BlockView is an address blocked on every node because an agent saw the
// MAC its guest is bound to on the port of another guest.
type BlockView struct {
	Address    netip.Addr `json:"address"`
	Guest      string     `json:"guest"`
	MAC        string     `json:"mac,omitempty"`
	Node       string     `json:"node"`
	Port       string     `json:"port"`
	Since      time.Time  `json:"since,omitzero"`
	LastReport time.Time  `json:"lastReport,omitzero"`
}

// ClusterStatus is the answer of GET /v1/cluster: the lease, the cluster and
// its nodes, as pco cluster status shows them.
type ClusterStatus struct {
	Lease   *LeaseView  `json:"lease,omitempty"`
	Cluster ClusterView `json:"cluster"`
	Nodes   []NodeView  `json:"nodes"`
}

// JoinTokenView is the answer of POST /v1/cluster/join-tokens. Token is the
// one copy of the secret there is: the install keeps only its hash.
type JoinTokenView struct {
	ID      string    `json:"id"`
	Node    string    `json:"node"`
	Expires time.Time `json:"expires"`
	Token   string    `json:"token"`
}

// HandoffResult is the answer of POST /v1/cluster/handoff: the lease is
// released, and every node but To holds back from it until Until.
type HandoffResult struct {
	From  string    `json:"from"`
	To    string    `json:"to,omitempty"`
	Until time.Time `json:"until"`
}

// FinishUpgradeResult is the answer of POST /v1/cluster/finish-upgrade: the
// release the upgrade came from, the schema of the store now, and where the
// backup made before it went.
type FinishUpgradeResult struct {
	From   string `json:"from"`
	Schema int    `json:"schema"`
	Backup string `json:"backup"`
}

// ForgetResult is the answer of POST /v1/nodes/:name/forget: the steps done,
// those left, which a controller that takes over finishes, and what the
// node's disk holds that pco cannot rotate.
type ForgetResult struct {
	Node        string   `json:"node"`
	Compromised bool     `json:"compromised"`
	Done        []string `json:"done"`
	Pending     []string `json:"pending,omitempty"`
	NotRotated  []string `json:"notRotated,omitempty"`
}

// PromoteResult is the answer of POST /v1/promote: what differs between the
// store and the agents' reports, and the epoch drawn once confirmed.
type PromoteResult struct {
	Differences []DifferenceView `json:"differences"`
	Drawn       bool             `json:"drawn"`
	Generation  int              `json:"generation,omitempty"`
	Nonce       string           `json:"nonce,omitempty"`
}

// DifferenceView is one thing the store and the agents say differently.
type DifferenceView struct {
	Subject  string `json:"subject"`
	Store    string `json:"store"`
	Reported string `json:"reported"`
}

// UninstallResult is the answer of POST /v1/cluster/uninstall: the nodes
// whose agents confirmed they are clean, and for each node left, the line to
// run on it later.
type UninstallResult struct {
	Clean []string `json:"clean"`
	Left  []string `json:"left,omitempty"`
}

func (l *LeaseView) clone() *LeaseView {
	if l == nil {
		return nil
	}
	c := *l
	return &c
}

func cloneNodes(nodes []NodeView) []NodeView {
	out := slices.Clone(nodes)
	for i := range out {
		n := &out[i]
		if n.Index != nil {
			index := *n.Index
			n.Index = &index
		}
		n.JoinUses, n.Contested = cloneJoinUses(n.JoinUses), cloneJoinUses(n.Contested)
	}
	return out
}

func cloneJoinUses(uses []JoinUseView) []JoinUseView {
	out := slices.Clone(uses)
	for i := range out {
		out[i].MACs = slices.Clone(out[i].MACs)
	}
	return out
}

func (c *ClusterView) clone() *ClusterView {
	if c == nil {
		return nil
	}
	v := *c
	v.Members, v.Overrides, v.Blocks = slices.Clone(c.Members), slices.Clone(c.Overrides), slices.Clone(c.Blocks)
	v.Coverage.Uncovered, v.Coverage.Flapping = slices.Clone(c.Coverage.Uncovered), slices.Clone(c.Coverage.Flapping)
	if u := c.Upgrade; u != nil {
		copied := *u
		copied.Older = slices.Clone(u.Older)
		v.Upgrade = &copied
	}
	if r := c.CARotation; r != nil {
		copied := *r
		copied.Also, copied.Done, copied.Refused = slices.Clone(r.Also), slices.Clone(r.Done), slices.Clone(r.Refused)
		v.CARotation = &copied
	}
	return &v
}

func (p *PlacementView) clone() *PlacementView {
	if p == nil {
		return nil
	}
	c := *p
	if p.Move != nil {
		m := *p.Move
		c.Move = &m
	}
	return &c
}

// clusterWithoutTimes takes the times and ages out of the views of the
// cluster, for the digest: a finished rotation still says so by its steps.
func (s *State) clusterWithoutTimes() {
	if l := s.Lease; l != nil {
		l.AcquiredAt, l.RefreshAge = time.Time{}, 0
	}
	for i := range s.Nodes {
		n := &s.Nodes[i]
		n.StateSince, n.CertExpires, n.StatusAge, n.ClockSkew = time.Time{}, time.Time{}, 0, 0
		for j := range n.JoinUses {
			n.JoinUses[j].At = time.Time{}
		}
		for j := range n.Contested {
			n.Contested[j].At = time.Time{}
		}
	}
	if c := s.Cluster; c != nil {
		c.Coverage.Since, c.Coverage.PromoteAt = time.Time{}, time.Time{}
		if c.Upgrade != nil {
			c.Upgrade.Since = time.Time{}
		}
		if r := c.CARotation; r != nil {
			r.StartedAt, r.FinishedAt = time.Time{}, time.Time{}
		}
		for i := range c.Blocks {
			c.Blocks[i].Since, c.Blocks[i].LastReport = time.Time{}, time.Time{}
		}
	}
	for i := range s.Routes {
		if p := s.Routes[i].Placement; p != nil && p.Move != nil {
			p.Move.Since = time.Time{}
		}
	}
}

// normalizedCluster sorts the nodes and the replicas of each route by node,
// and makes both lists non-nil.
func (s *State) normalizedCluster() {
	slices.SortStableFunc(s.Nodes, func(a, b NodeView) int { return cmp.Compare(a.Name, b.Name) })
	s.Nodes = nonNil(s.Nodes)
	for i := range s.Routes {
		r := &s.Routes[i]
		slices.SortStableFunc(r.Replicas, func(a, b ReplicaView) int { return cmp.Compare(a.Node, b.Node) })
		r.Replicas = nonNil(r.Replicas)
	}
}
