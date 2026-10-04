package store

import (
	"bytes"
	"encoding/json"
	"maps"
	"sync"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// rewriteProofAfter is how far the time of a proof moves on before the file
// of its binding is written for that alone.
const rewriteProofAfter = resolve.DefaultMaxProofAge / 4

// keepsProof reports whether the binding stored may stand for b: the two
// differ in the time of the proof only, which moved on by no more than
// rewriteProofAfter. One that moved back is written.
func keepsProof(stored json.RawMessage, b resolve.Binding) bool {
	var old resolve.Binding
	if err := json.Unmarshal(stored, &old); err != nil {
		return false
	}
	moved := b.VerifiedAt.Sub(old.VerifiedAt)
	return moved >= 0 && moved <= rewriteProofAfter && sameButProof(old, b)
}

// sameButProof reports whether a and b differ in the time of the proof at
// most, as their files would say.
func sameButProof(a, b resolve.Binding) bool {
	a.VerifiedAt = b.VerifiedAt
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// proofTimes holds the bindings last saved, whose time of proof their files
// may not have.
type proofTimes struct {
	mu    sync.Mutex
	saved map[string]resolve.Binding
}

func (p *proofTimes) keep(next map[string]resolve.Binding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saved = maps.Clone(next)
}

// restore gives every binding read from a file that differs from the one last
// saved only in an older time of the proof the time it was saved with.
func (p *proofTimes) restore(stored map[string]resolve.Binding) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for host, b := range stored {
		if saved, ok := p.saved[host]; ok && saved.VerifiedAt.After(b.VerifiedAt) && sameButProof(b, saved) {
			b.VerifiedAt = saved.VerifiedAt
			stored[host] = b
		}
	}
}
