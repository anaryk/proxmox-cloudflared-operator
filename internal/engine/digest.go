package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// digestOf names what a state holds: the first 16 hex digits of the SHA-256
// of the JSON of its digest view. Two idle cycles over the same world have the
// same one, so that a client that holds the state does not fetch it again.
func digestOf(st State) string {
	data, err := json.Marshal(st.digestView())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// digestView is a copy of s without its digest and without any time in it:
// when the cycle ran, a proof was stored or a credential checked moves on
// while what the state holds stays. A token that expires still says so. The
// stream carries the times of the cycle.
func (s State) digestView() State {
	v := s.clone()
	v.Digest, v.At, v.FinishedAt = "", time.Time{}, time.Time{}
	for i := range v.Routes {
		if p := v.Routes[i].Path; p != nil {
			p.VerifiedAt, p.Since = time.Time{}, time.Time{}
		}
	}
	for i := range v.Credentials {
		r := &v.Credentials[i].Report
		r.CheckedAt = time.Time{}
		if r.Token.ExpiresOn != nil {
			r.Token.ExpiresOn = new(time.Time)
		}
	}
	for i := range v.Tunnels {
		if r := v.Tunnels[i].Rollout; r != nil {
			r.ConfirmedAt = time.Time{}
		}
	}
	for i := range v.Segments {
		v.Segments[i].AcknowledgedAt = time.Time{}
	}
	for i := range v.RogueConnectors {
		v.RogueConnectors[i].Since = time.Time{}
	}
	v.Egress.Since = time.Time{}
	return v
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
