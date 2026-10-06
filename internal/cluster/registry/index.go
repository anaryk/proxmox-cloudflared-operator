package registry

import (
	"errors"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// MaxIndex is the highest index a node is given: 255 is reserved for the
// managed leg.
const MaxIndex = 254

// ErrNoFreeIndex is what AssignIndex wraps when every index is taken.
var ErrNoFreeIndex = errors.New("no free node index")

func validIndex(i int) bool { return i >= 0 && i <= MaxIndex }

// AssignIndex gives the node its index: the one its entry holds, else the one
// its name had before when no other node has it now, else the lowest free one
// from 0 to MaxIndex.
//
// An index stays with the name of the node that had it for as long as the
// install lives, so the entry of a removed node keeps its index taken, and so
// does a name that only previous remembers, for the node may be enrolled again
// under it and its leg MACs must not change. A node whose entry has no index
// yet takes none from the list. Asked one after the other, with the entries
// read again in between, two nodes never get the same index.
func AssignIndex(entries []store.NodeEntry, name string, previous map[string]int) (int, error) {
	taken := map[int]bool{}
	for _, e := range entries {
		if e.Index == nil || !validIndex(*e.Index) {
			continue
		}
		if e.Name == name {
			return *e.Index, nil
		}
		taken[*e.Index] = true
	}
	for other, i := range previous {
		if other != name && validIndex(i) {
			taken[i] = true
		}
	}
	if i, ok := previous[name]; ok && validIndex(i) && !taken[i] {
		return i, nil
	}
	for i := 0; i <= MaxIndex; i++ {
		if !taken[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("node %s: %w: all %d indexes are given to other nodes", name, ErrNoFreeIndex, MaxIndex+1)
}
