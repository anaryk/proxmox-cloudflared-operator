package daemon

import "github.com/anaryk/proxmox-cloudflared-operator/internal/store"

// StorePaths returns the roots of the store on a Proxmox node, with the
// directories that are not empty in their place. The check for the cluster
// filesystem belongs to the default roots only: it is cleared when the cluster
// or the private root is moved, as there is nothing mounted behind a directory
// of a test or of an unusual install.
func StorePaths(cluster, private, local string) store.Paths {
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
	return p
}
