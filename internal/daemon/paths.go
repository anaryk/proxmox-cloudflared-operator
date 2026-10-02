package daemon

import (
	"errors"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// StorePaths returns the roots of the store on a Proxmox node, or the three
// directories given instead: a store of its own, for a test or an unusual
// install, where the check for the cluster filesystem is cleared, as nothing
// is mounted behind them.
//
// The directories move all three or none. The local one holds the lock of the
// node, the private one the tokens and the cluster one what the connectors of
// the node are kept by: a daemon that kept some of them would reconcile the
// store of the node a second time, or mix its own with the tokens and the
// connectors of the node.
func StorePaths(cluster, private, local string) (store.Paths, error) {
	given := 0
	for _, dir := range []string{cluster, private, local} {
		if dir != "" {
			given++
		}
	}
	p := store.DefaultPaths()
	switch given {
	case 0:
		return p, nil
	case 3:
		p.Cluster, p.Private, p.Local, p.MountCheck = cluster, private, local, ""
		return p, nil
	}
	return store.Paths{}, errors.New("--cluster-dir, --private-dir and --local-dir are given all three or none: " +
		"a daemon that kept some of the directories of the node would reconcile its store a second time, " +
		"or mix a store of its own with the tokens and the connectors of the node")
}
