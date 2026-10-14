package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// TakenOver says whether a store restored from a bundle was taken into the
// cluster it is in now completely, and when not, why. It is read from what
// the store holds, not only from what the install says: one node claimed it,
// the CA is the one that node made and no previous one is kept, and every
// other node of the registry is removed, so that nothing of the lost cluster
// is trusted. An install that was not restored is taken over.
func (s *Store) TakenOver(inst Install) (bool, string, error) {
	r := inst.Restored
	switch {
	case r == nil:
		return true, "", nil
	case r.TakenOverBy == "":
		return false, "no node took it into this cluster yet", nil
	case r.CASHA256 == "":
		return false, fmt.Sprintf("node %s began to take it into this cluster and did not finish", r.TakenOverBy), nil
	}
	current, previous, _, err := s.CA()
	if err != nil {
		return false, "", fmt.Errorf("reading the CA: %w", err)
	}
	cert, err := parseCertPEM(current.CertPEM)
	if err != nil {
		return false, "", fmt.Errorf("the CA of the install: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	if hex.EncodeToString(sum[:]) != r.CASHA256 || previous != nil {
		return false, fmt.Sprintf("its CA is not the one node %s made when it took it", r.TakenOverBy), nil
	}
	nodes, err := s.Nodes()
	if err != nil {
		return false, "", fmt.Errorf("reading the node registry: %w", err)
	}
	for _, n := range nodes {
		if n.Name != r.TakenOverBy && n.State != NodeRemoved {
			return false, fmt.Sprintf("node %s of the lost cluster is %s, not removed", n.Name, stateOrNone(n.State)), nil
		}
	}
	return true, "", nil
}

func stateOrNone(state string) string {
	if state == "" {
		return "without a state"
	}
	return state
}
