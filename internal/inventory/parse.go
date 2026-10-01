// Package inventory builds the operator's view of Proxmox guests.
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// maxNICs is the number of net<N> slots Proxmox offers (net0 to net31).
const maxNICs = 32

// maxVLAN is the highest usable 802.1Q tag.
const maxVLAN = 4094

// BuildGuest combines a resource row and its config into a guest. Reported
// addresses are filled in by the caller.
//
// The NIC list is what the identity check trusts, so anything that does not
// parse cleanly is dropped rather than interpreted: a malformed NIC is
// skipped, a malformed address is ignored. Only an unknown guest kind is an
// error.
func BuildGuest(res pve.Resource, cfg pve.GuestConfig) (model.Guest, error) {
	if res.Kind != model.KindQEMU && res.Kind != model.KindLXC {
		return model.Guest{}, fmt.Errorf("building guest %d: unknown guest kind %q", res.VMID, res.Kind)
	}
	ref := model.GuestRef{Kind: res.Kind, VMID: res.VMID}
	nics := parseNICs(res.Kind, cfg.Values)
	return model.Guest{
		Ref:         ref,
		Name:        guestName(res, cfg.Values),
		Node:        res.Node,
		Running:     res.Status == "running",
		Template:    res.Template || cfg.Values["template"] == "1",
		Tags:        guestTags(res, cfg.Values),
		Description: cfg.Values["description"],
		Digest:      cfg.Digest,
		Identity:    guestIdentity(ref, nics, cfg.Values),
		NICs:        nics,
	}, nil
}

func guestName(res pve.Resource, values map[string]string) string {
	switch {
	case res.Name != "":
		return res.Name
	case res.Kind == model.KindLXC:
		return values["hostname"]
	default:
		return values["name"]
	}
}

// guestTags prefers the tags of the resource row and falls back to the
// config's tag string, which Proxmox separates with ';', ',' or spaces.
func guestTags(res pve.Resource, values map[string]string) []string {
	if len(res.Tags) > 0 {
		return slices.Clone(res.Tags)
	}
	tags := strings.FieldsFunc(values["tags"], func(r rune) bool {
		return r == ';' || r == ',' || unicode.IsSpace(r)
	})
	if len(tags) == 0 {
		return nil
	}
	for i, t := range tags {
		tags[i] = strings.ToLower(t)
	}
	return tags
}

// parseNICs reads net0 to net31 in index order.
func parseNICs(kind model.GuestKind, values map[string]string) []model.NIC {
	var nics []model.NIC
	for i := range maxNICs {
		raw, found := values["net"+strconv.Itoa(i)]
		if !found {
			continue
		}
		var (
			nic model.NIC
			ok  bool
		)
		if kind == model.KindQEMU {
			nic, ok = parseQEMUNIC(i, raw, values["ipconfig"+strconv.Itoa(i)])
		} else {
			nic, ok = parseLXCNIC(i, raw)
		}
		if ok {
			nics = append(nics, nic)
		}
	}
	return nics
}

// nicModels are the QEMU NIC models that carry the MAC as their value.
var nicModels = map[string]bool{
	"virtio":  true,
	"e1000":   true,
	"e1000e":  true,
	"rtl8139": true,
	"vmxnet3": true,
}

// parseQEMUNIC reads "virtio=MAC,bridge=vmbr0,tag=10,firewall=1" and the
// older "e1000,macaddr=MAC,bridge=vmbr0". The static address comes from the
// ipconfig value with the same index.
func parseQEMUNIC(index int, raw, ipconfig string) (model.NIC, bool) {
	pairs, ok := splitPairs(raw, func(s string) bool { return nicModels[s] })
	if !ok {
		return model.NIC{}, false
	}
	mac, ok := qemuMAC(pairs)
	if !ok {
		return model.NIC{}, false
	}
	nic, ok := buildNIC(index, mac, pairs)
	if ok {
		nic.Static = ipconfigAddrs(ipconfig)
	}
	return nic, ok
}

// qemuMAC finds the MAC given either as "<model>=MAC" or "macaddr=MAC". A NIC
// that names two different MACs is ambiguous and rejected.
func qemuMAC(pairs []pair) (string, bool) {
	var mac string
	for _, p := range pairs {
		if p.key != "macaddr" && !nicModels[p.key] {
			continue
		}
		norm, err := model.NormalizeMAC(p.value)
		if err != nil || (mac != "" && mac != norm) {
			return "", false
		}
		mac = norm
	}
	return mac, mac != ""
}

// parseLXCNIC reads "name=eth0,bridge=vmbr0,hwaddr=MAC,ip=10.20.0.30/24,...".
func parseLXCNIC(index int, raw string) (model.NIC, bool) {
	pairs, ok := splitPairs(raw, nil)
	if !ok {
		return model.NIC{}, false
	}
	mac, err := model.NormalizeMAC(lookup(pairs, "hwaddr"))
	if err != nil {
		return model.NIC{}, false
	}
	nic, ok := buildNIC(index, mac, pairs)
	if ok {
		nic.Static = staticAddrs(pairs)
	}
	return nic, ok
}

// buildNIC fills in the keys QEMU and LXC spell the same way.
func buildNIC(index int, mac string, pairs []pair) (model.NIC, bool) {
	bridge := lookup(pairs, "bridge")
	if bridge == "" {
		return model.NIC{}, false
	}
	vlan, ok := parseVLAN(lookup(pairs, "tag"))
	if !ok {
		return model.NIC{}, false
	}
	return model.NIC{
		Index:    index,
		MAC:      mac,
		Bridge:   bridge,
		VLAN:     vlan,
		Firewall: lookup(pairs, "firewall") == "1",
	}, true
}

// parseVLAN reads a tag; an absent tag is VLAN 0, an invalid one is an error.
func parseVLAN(tag string) (int, bool) {
	if tag == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(tag, 10, 16)
	if err != nil || n < 1 || n > maxVLAN {
		return 0, false
	}
	return int(n), true
}

// ipconfigAddrs reads the static IPv4 address from a QEMU "ipconfigN" value.
func ipconfigAddrs(raw string) []netip.Addr {
	pairs, ok := splitPairs(raw, nil)
	if !ok {
		return nil
	}
	return staticAddrs(pairs)
}

// staticAddrs reads an "ip=<cidr>" pair; "dhcp", "manual", IPv6 and anything
// that is not a usable unicast IPv4 address yield nothing.
func staticAddrs(pairs []pair) []netip.Addr {
	prefix, err := netip.ParsePrefix(lookup(pairs, "ip"))
	if err != nil {
		return nil
	}
	addr := prefix.Addr()
	if !addr.Is4() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() {
		return nil
	}
	return []netip.Addr{addr}
}

// guestIdentity names a guest so a recreated one is not mistaken for the
// original. QEMU guests prefer their SMBIOS UUID; otherwise the identity is
// a hash over things that change when the guest is rebuilt.
func guestIdentity(ref model.GuestRef, nics []model.NIC, values map[string]string) string {
	if ref.Kind == model.KindQEMU {
		if uuid, ok := smbiosUUID(values["smbios1"]); ok {
			return "uuid:" + uuid
		}
	}
	if len(nics) == 0 {
		return "vmid:" + ref.String()
	}
	seed := nics[0].MAC + "|" + values["hostname"]
	if ref.Kind == model.KindQEMU {
		seed = creationTime(values["meta"]) + "|" + nics[0].MAC
	}
	sum := sha256.Sum256([]byte(seed))
	return "mac:" + hex.EncodeToString(sum[:16])
}

// smbiosUUID returns the lower-cased uuid of a QEMU "smbios1" value.
func smbiosUUID(raw string) (string, bool) {
	pairs, ok := splitPairs(raw, nil)
	if !ok {
		return "", false
	}
	uuid := lookup(pairs, "uuid")
	if !validUUID(uuid) {
		return "", false
	}
	return strings.ToLower(uuid), true
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case !isHex(r):
			return false
		}
	}
	return true
}

func isHex(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
}

// creationTime returns the ctime of a QEMU "meta" value, or "" when it is
// missing or not a number.
func creationTime(raw string) string {
	pairs, ok := splitPairs(raw, nil)
	if !ok {
		return ""
	}
	ctime := lookup(pairs, "ctime")
	if _, err := strconv.ParseUint(ctime, 10, 64); err != nil {
		return ""
	}
	return ctime
}

// pair is one key=value item of a Proxmox property string.
type pair struct {
	key, value string
}

// splitPairs splits a comma-separated property string. It fails on empty
// items, empty keys or values and repeated keys. An item without '=' is only
// accepted when bare approves it, and is then reported as model=<item>, the
// spelling of QEMU's default key.
func splitPairs(raw string, bare func(string) bool) ([]pair, bool) {
	items := strings.Split(raw, ",")
	pairs := make([]pair, 0, len(items))
	for _, item := range items {
		key, value, found := strings.Cut(item, "=")
		if !found {
			if bare == nil || !bare(item) {
				return nil, false
			}
			key, value = "model", item
		}
		if key == "" || value == "" || lookup(pairs, key) != "" {
			return nil, false
		}
		pairs = append(pairs, pair{key, value})
	}
	return pairs, true
}

// lookup returns the value of key, or "" when it is absent.
func lookup(pairs []pair, key string) string {
	for _, p := range pairs {
		if p.key == key {
			return p.value
		}
	}
	return ""
}
