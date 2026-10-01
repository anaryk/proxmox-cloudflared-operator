package inventory

import (
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// applyStatus decides whether guest runs and remembers what was read in
// entry. Only the statuses running and stopped are taken as they are.
// Proxmox reports unknown when its statistics daemon stalls or a node cannot
// be reached, which says nothing about the guest, so any status but those
// two keeps the state last read for the same guest. A guest for which none
// was ever read counts as not running and is marked StatusUnknown, so that
// "not running" is not mistaken for something Proxmox said. It reports
// whether the status had to be passed over.
func applyStatus(guest *model.Guest, row pve.Resource, entry *cacheEntry) (unread bool) {
	switch {
	case row.Status == "running" || row.Status == "stopped":
		entry.running, entry.identity = guest.Running, guest.Identity
		return false
	case entry.identity != "" && entry.identity == guest.Identity:
		guest.Running = entry.running
	default:
		guest.Running, guest.StatusUnknown = false, true
	}
	return true
}
