package reconcile

import (
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// writerFunc is the writer callback both reconcilers take: it returns the
// identity this process writes as (us) and leader.json as stored now
// (stored), and fails whenever this process may not write at all.
type writerFunc = func() (us, stored planner.Writer, err error)

// writerFault says why an answer of the writer callback forbids writing: a
// run writes only as the identity it started with, and only while
// leader.json names that identity.
func writerFault(started, us, stored planner.Writer) string {
	switch {
	case us != started:
		return "the writer identity changed during the run"
	case stored.Generation != us.Generation || stored.Nonce != us.Nonce:
		// The install id is not compared: leader.json does not hold one.
		return fmt.Sprintf("leader.json names generation %d nonce %s, not this writer (generation %d nonce %s)",
			stored.Generation, stored.Nonce, us.Generation, us.Nonce)
	}
	return ""
}

// startWriter reads the identity a run writes as. fault says why the run may
// not write at all, and stale whether that is because leader.json names
// another writer. us is the zero Writer unless the identity is valid.
func startWriter(writer writerFunc) (us planner.Writer, fault string, stale bool) {
	us, stored, err := writer()
	if err != nil {
		return planner.Writer{}, fmt.Sprintf("reading the writer identity: %v", err), false
	}
	if err := us.Validate(); err != nil {
		return planner.Writer{}, fmt.Sprintf("cannot write as this writer: %v", err), false
	}
	fault = writerFault(us, us, stored)
	return us, fault, fault != ""
}

// recheckWriter asks the writer callback again whether a run that started as
// started may still write. fault says why not; stale is false when the answer
// could not be read at all.
func recheckWriter(writer writerFunc, started planner.Writer) (fault string, stale bool) {
	us, stored, err := writer()
	if err != nil {
		return fmt.Sprintf("reading the writer identity: %v", err), false
	}
	fault = writerFault(started, us, stored)
	return fault, fault != ""
}
