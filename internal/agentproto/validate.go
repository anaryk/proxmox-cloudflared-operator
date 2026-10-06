package agentproto

import (
	"errors"
	"fmt"
)

// What Validate refuses. Each is wrapped with the values that differ.
var (
	ErrProtocol        = errors.New("snapshot in a protocol this agent does not speak")
	ErrOtherNode       = errors.New("snapshot for another node")
	ErrOtherInstall    = errors.New("snapshot of another install")
	ErrBadEpoch        = errors.New("snapshot without a valid epoch")
	ErrGenerationBound = errors.New("snapshot of a generation above 2^53")
	ErrLowerEpoch      = errors.New("snapshot of a lower epoch")
	ErrOtherNonce      = errors.New("snapshot of another epoch of the same generation")
	ErrSeqBack         = errors.New("snapshot whose seq goes back")
)

// Validate reports whether the agent of node may apply s. remembered is the
// epoch the agent follows, its install the one the agent was enrolled for, and
// applied the seq it last applied under it. An agent that has heard no
// controller yet passes its install with generation 0.
//
// It refuses a snapshot for another node or install, of a lower epoch, of the
// same generation with another nonce, of a generation above MaxGeneration, or
// whose seq goes back within the epoch. A higher epoch is taken whatever its
// seq: seq starts again in each epoch.
func (s Snapshot) Validate(node string, remembered Epoch, applied uint64) error {
	e := s.Epoch
	switch {
	case s.Protocol < ProtocolMin || s.Protocol > ProtocolMax:
		return fmt.Errorf("%w: %d, it speaks %d to %d", ErrProtocol, s.Protocol, ProtocolMin, ProtocolMax)
	case s.Node != node:
		return fmt.Errorf("%w: %s, this is %s", ErrOtherNode, s.Node, node)
	case remembered.InstallID == "":
		return errors.New("no install to check the snapshot against")
	case e.InstallID != remembered.InstallID:
		return fmt.Errorf("%w: %s, this agent is of %s", ErrOtherInstall, e.InstallID, remembered.InstallID)
	case e.Generation < 1 || !lowerAlnum(e.Nonce):
		return fmt.Errorf("%w: generation %d, nonce %q", ErrBadEpoch, e.Generation, e.Nonce)
	case e.Generation > MaxGeneration:
		return fmt.Errorf("%w: %d", ErrGenerationBound, e.Generation)
	}
	switch c, same := e.Order(remembered); {
	case c < 0:
		return fmt.Errorf("%w: %s, this agent follows %s", ErrLowerEpoch, e, remembered)
	case c == 0 && !same:
		return fmt.Errorf("%w: %s, this agent follows %s", ErrOtherNonce, e, remembered)
	case same && s.Seq < applied:
		return fmt.Errorf("%w: %d after %d in epoch %s", ErrSeqBack, s.Seq, applied, e)
	}
	return nil
}
