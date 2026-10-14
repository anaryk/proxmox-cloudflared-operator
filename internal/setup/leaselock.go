package setup

import (
	"errors"
	"os"
	"syscall"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
)

// removeLeaseLock removes the lock of the lease that the daemon of a node
// standing alone keeps when it stops, so that its next start keeps the epoch.
// An uninstall is no such stop: the install goes with the node, and a setup
// after it would wait two minutes for the lock to expire. The lock goes only
// when the install goes with this node, the node stands alone by the members
// file, and leader.json, read without error, gives the lease of this install
// to this node or to nobody: the lock of a node of a cluster is as its daemon
// left it, given back on a clean stop or left to expire, a lock that
// leader.json gives to another node or another install is theirs, and a
// leader.json that cannot be read says nothing of whose the lock is, so the
// lock stays. A lock with a file in it never expires and is never removed by
// pco. It runs once the daemon has stopped.
func (u *uninstall) removeLeaseLock() {
	path := u.host.leaseLock
	if _, err := os.Lstat(path); err != nil {
		return
	}
	if !u.last || !u.standsAlone() {
		return
	}
	leader, _, found, err := u.st.Leader()
	if err != nil {
		u.ask.Warn("lease: leader.json cannot be read (%v), so the lock %s is kept; the next setup on this node waits for it to expire", err, path)
		return
	}
	if found && (leader.Node != "" && leader.Node != u.node ||
		leader.InstallID != "" && u.installID != "" && leader.InstallID != u.installID) {
		return
	}
	switch err := os.Remove(path); {
	case err == nil:
		u.ask.Info("lease: removed the lock %s, which this node kept as it stands alone", path)
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		u.ask.Warn("lease: the lock %s holds a file, so it is kept and never expires; look at it and remove it by hand", path)
	default:
		u.ask.Warn("lease: the lock %s could not be removed (%v); the next setup on this node waits for it to expire", path, err)
	}
}

// standsAlone reports whether the members file says the node is in no
// cluster. A file that cannot be read counts as a cluster.
func (u *uninstall) standsAlone() bool {
	m, err := members.ReadFiles(members.Files{
		Members: u.host.members, Corosync: u.host.corosync, LocalCorosync: u.host.localCorosync,
	}, u.now())
	return err == nil && m.Standalone
}
