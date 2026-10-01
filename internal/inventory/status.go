package inventory

import (
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// applyStatus decides whether guest runs and remembers it in entry. Only the
// statuses running and stopped are taken as they are. Proxmox reports unknown
// when its statistics daemon stalls or a node cannot be reached, which says
// nothing about the guest, so any status but those two keeps the state last
// read for the same guest; a guest never seen running counts as not running.
// It reports whether the status had to be passed over.
func applyStatus(guest *model.Guest, row pve.Resource, entry *cacheEntry) (unread bool) {
	switch row.Status {
	case "running", "stopped":
	default:
		guest.Running = entry.running && entry.identity == guest.Identity
		unread = true
	}
	entry.running, entry.identity = guest.Running, guest.Identity
	return unread
}
