package daemon

import (
	"errors"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// StorePaths returns the roots of the store on a Proxmox node, with the
// directories that are not empty in their place. The check for the cluster
// filesystem belongs to the default roots only: it is cleared when the cluster
// or the private root is moved, as there is nothing mounted behind a directory
// of a test or of an unusual install.
//
// The local directory holds the lock of the node, so it is moved only together
// with the cluster and the private directory: a daemon that kept the cluster
// store and had a lock of its own would reconcile it a second time.
func StorePaths(cluster, private, local string) (store.Paths, error) {
	if local != "" && (cluster == "" || private == "") {
		return store.Paths{}, errors.New("--local-dir is refused without --cluster-dir and --private-dir: " +
			"the lock of the node lives in the local directory, and a daemon with another one but the same " +
			"cluster store would reconcile it a second time; a separate store needs all three")
	}
	p := store.DefaultPaths()
	if cluster != "" {
		p.Cluster = cluster
	}
	if private != "" {
		p.Private = private
	}
	if local != "" {
		p.Local = local
	}
	if cluster != "" || private != "" {
		p.MountCheck = ""
	}
	return p, nil
}
