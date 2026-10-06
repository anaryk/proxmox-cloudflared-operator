package agentproto

import "time"

// Heartbeat is the controller's answer to a status. Agents judge by it how
// fresh an attestation is, whether a controller with a valid lease is alive,
// and how many replicas of a tunnel are ready, without the snapshot changing
// every cycle.
type Heartbeat struct {
	Epoch Epoch `json:"epoch"`
	// LeaseValid holds only while the controller's cycles make progress.
	LeaseValid    bool                     `json:"leaseValid"`
	LeaseValidFor time.Duration            `json:"leaseValidFor,omitempty"`
	Seq           uint64                   `json:"seq"`
	Nodes         map[string]NodeFreshness `json:"nodes"`
	ReadyReplicas map[string]int           `json:"readyReplicas"` // by tunnel id
	EventAck      uint64                   `json:"eventAck"`
}

// NodeFreshness is how recent a node's last status is at the controller.
type NodeFreshness struct {
	StatusAge    time.Duration `json:"statusAge"`
	WatchRunning bool          `json:"watchRunning"`
}
