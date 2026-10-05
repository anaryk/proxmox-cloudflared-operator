package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// digestOf names what a state holds: the first 16 hex digits of the SHA-256
// of its JSON without the digest and the times of its cycle. Two idle cycles
// over the same world have the same one, so that a client that holds the
// state does not fetch it again.
func digestOf(st State) string {
	st.Digest, st.At, st.FinishedAt = "", time.Time{}, time.Time{}
	data, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// stateNotice is what the stream is told of a state.
func (s State) stateNotice() StateNotice {
	return StateNotice{At: s.At, FinishedAt: s.FinishedAt, Digest: s.Digest}
}

// redigest names the state served again after it changed between two
// cycles, and tells the stream when it did. The caller holds stateMu.
func (e *Engine) redigest() {
	d := digestOf(e.state)
	if d == e.state.Digest {
		return
	}
	e.state.Digest = d
	e.notify.state(e.state.stateNotice())
}
