package inventory

import (
	"maps"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/annotation"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// Remembered is what a refresh read of the configuration of a watched
// guest, as much of it as a guest is built from. A process that takes over
// while the node of the guest is offline, and so cannot read it, builds the
// guest from it instead of taking the guest, and its routes, for unknown.
type Remembered struct {
	Ref    model.GuestRef    `json:"ref"`
	Digest string            `json:"digest,omitempty"`
	Values map[string]string `json:"values"`
}

// Remembered returns the configurations of the watched guests as the last
// complete refresh had them, in the order of its snapshot; before one, what
// Remember gave, which is passed on as it came.
func (i *Inventory) Remembered() []Remembered {
	i.keptMu.Lock()
	defer i.keptMu.Unlock()
	out := make([]Remembered, len(i.kept))
	for j, r := range i.kept {
		out[j] = Remembered{Ref: r.Ref, Digest: r.Digest, Values: maps.Clone(r.Values)}
	}
	return out
}

// Remember gives the inventory what an earlier process remembered. A
// refresh uses it only for a guest on an offline node that this process
// never read, and forgets it once the guest was read.
func (i *Inventory) Remember(rs []Remembered) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.remembered = make(map[model.GuestRef]pve.GuestConfig, len(rs))
	kept := make([]Remembered, 0, len(rs))
	for _, r := range rs {
		values := builtFrom(r.Values)
		i.remembered[r.Ref] = pve.GuestConfig{Values: values, Digest: r.Digest}
		kept = append(kept, Remembered{Ref: r.Ref, Digest: r.Digest, Values: maps.Clone(values)})
	}
	i.keptMu.Lock()
	defer i.keptMu.Unlock()
	if !i.keptOwn {
		i.kept = kept
	}
}

// keep notes the watched guests of a complete refresh, for Remembered.
func (i *Inventory) keep(ts []tracked) {
	var kept []Remembered
	for _, t := range ts {
		if !t.watched || t.entry.cfg.Values == nil {
			continue
		}
		kept = append(kept, Remembered{Ref: t.guest.Ref, Digest: t.entry.cfg.Digest, Values: builtFrom(t.entry.cfg.Values)})
	}
	i.keptMu.Lock()
	defer i.keptMu.Unlock()
	i.kept, i.keptOwn = kept, true
}

// builtFrom is the part of a configuration BuildGuest reads: what else a
// configuration holds, as the keys of cloud-init, is not kept.
func builtFrom(values map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range values {
		switch {
		case key == "description":
			out[key] = routeNotes(value)
		case key == "name", key == "hostname", key == "tags", key == "template",
			key == "smbios1", key == "meta",
			numbered(key, "net"), numbered(key, "ipconfig"):
			out[key] = value
		}
	}
	return out
}

// routeNotes is what the routes of a guest are read from in its Notes: the
// route text, and on a line of its own every hostname the Notes name, which
// keeps a claim while its entry is broken. The prose around them is not
// kept, so that a long description costs the memory nothing.
func routeNotes(notes string) string {
	text, _ := annotation.Block(notes)
	hosts := annotation.Hostnames(notes)
	if len(hosts) == 0 {
		return text
	}
	return text + "\n" + strings.Join(hosts, " ") + "\n"
}

// numbered says whether key is prefix followed by a number, as net0.
func numbered(key, prefix string) bool {
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || rest == "" {
		return false
	}
	return strings.Trim(rest, "0123456789") == ""
}
