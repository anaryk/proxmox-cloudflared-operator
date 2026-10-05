package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// maxNICs is the number of net<N> slots Proxmox offers (net0 to net31).
const maxNICs = 32

// maxVLAN is the highest usable 802.1Q tag.
const maxVLAN = 4094

// BuildGuest combines a resource row and its config into a guest. Reported
// addresses are filled in by the caller. Running is true only for the status
// running; for a status that is neither running nor stopped the refresh
// keeps the last known state instead, or sets StatusUnknown when there is
// none.
//
// The NIC list is what the identity check trusts, so anything that does not
// parse cleanly is dropped rather than interpreted: a malformed NIC is
// skipped, a malformed address is ignored. Only an unknown guest kind is an
// error.
//
// Identity tells a guest apart from a different one that later holds the same
// VMID, and must not change while the guest itself is unchanged. A QEMU guest
// is identified by its SMBIOS UUID, then by its creation time, then by the
// MAC of its first NIC; a container by that MAC; anything without one by its
// kind and VMID. The MAC is the one on the lowest-numbered netN key that
// carries a readable MAC, whether or not that NIC is otherwise valid, so a
// rename, an added NIC or an edited bridge or tag leaves the identity alone.
// Adding a NIC at a lower index than the current first one, removing the
// first NIC or changing its MAC changes a MAC-based identity, which every
// container has.
func BuildGuest(res pve.Resource, cfg pve.GuestConfig) (model.Guest, error) {
	if res.Kind != model.KindQEMU && res.Kind != model.KindLXC {
		return model.Guest{}, fmt.Errorf("building guest %d: unknown guest kind %q", res.VMID, res.Kind)
	}
	ref := model.GuestRef{Kind: res.Kind, VMID: res.VMID}
	return model.Guest{
		Ref:         ref,
		Name:        guestName(res, cfg.Values),
		Node:        res.Node,
		Running:     res.Status == "running",
		Template:    res.Template || cfg.Values["template"] == "1",
		Tags:        guestTags(res, cfg.Values),
		Pool:        res.Pool,
		Description: cfg.Values["description"],
		Digest:      cfg.Digest,
		Identity:    guestIdentity(ref, cfg.Values),
		NICs:        parseNICs(res.Kind, cfg.Values),
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
// config's tag string.
func guestTags(res pve.Resource, values map[string]string) []string {
	if len(res.Tags) > 0 {
		return slices.Clone(res.Tags)
	}
	return pve.SplitTags(values["tags"])
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

// isNICModel reports whether s is a QEMU NIC model. In a net value the model
// is either a key carrying the MAC ("virtio=MAC") or a bare first item
// followed by "macaddr=MAC".
func isNICModel(s string) bool {
	switch s {
	case "e1000", "e1000-82540em", "e1000-82544gc", "e1000-82545em", "e1000e",
		"i82551", "i82557b", "i82559er", "ne2k_isa", "ne2k_pci", "pcnet",
		"rtl8139", "virtio", "vmxnet3":
		return true
	}
	return false
}

// parseQEMUNIC reads "virtio=MAC,bridge=vmbr0,tag=10,firewall=1" and the
// older "e1000,macaddr=MAC,bridge=vmbr0". The static address comes from the
// ipconfig value with the same index.
func parseQEMUNIC(index int, raw, ipconfig string) (model.NIC, bool) {
	pairs, ok := splitPairs(raw, isNICModel)
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
		if !carriesMAC(model.KindQEMU, p.key) {
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
		Firewall: isEnabled(lookup(pairs, "firewall")),
	}, true
}

// isEnabled reads a Proxmox boolean; only the spellings for true count.
func isEnabled(s string) bool {
	switch strings.ToLower(s) {
	case "1", "on", "yes", "true":
		return true
	}
	return false
}

// parseVLAN reads a tag. An absent tag is VLAN 0; ok is false when the tag is
// not a number from 1 to 4094.
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
// that is not a usable unicast IPv4 address (including the broadcast address)
// yield nothing.
func staticAddrs(pairs []pair) []netip.Addr {
	prefix, err := netip.ParsePrefix(lookup(pairs, "ip"))
	if err != nil {
		return nil
	}
	addr := prefix.Addr()
	if !addr.Is4() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() ||
		addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return nil
	}
	return []netip.Addr{addr}
}

// guestIdentity names a guest; see BuildGuest for the scheme.
func guestIdentity(ref model.GuestRef, values map[string]string) string {
	switch ref.Kind {
	case model.KindQEMU:
		if uuid, ok := smbiosUUID(values["smbios1"]); ok {
			return "uuid:" + uuid
		}
		if ctime, ok := creationTime(values["meta"]); ok {
			return "ctime:" + ctime
		}
		return macIdentity(ref, values)
	default:
		return macIdentity(ref, values)
	}
}

// macIdentity hashes the MAC of the lowest-numbered NIC that has one. It reads
// the raw net values rather than the parsed NICs so that a NIC that is
// skipped for another reason still counts.
func macIdentity(ref model.GuestRef, values map[string]string) string {
	for i := range maxNICs {
		mac, ok := nicMAC(ref.Kind, values["net"+strconv.Itoa(i)])
		if !ok {
			continue
		}
		sum := sha256.Sum256([]byte(mac))
		return "mac:" + hex.EncodeToString(sum[:16])
	}
	return "vmid:" + ref.String()
}

// carriesMAC reports whether a net value item with this key holds a MAC.
func carriesMAC(kind model.GuestKind, key string) bool {
	if kind == model.KindLXC {
		return key == "hwaddr"
	}
	return key == "macaddr" || isNICModel(key)
}

// nicMAC returns the first readable MAC of a net value, ignoring items it
// does not understand.
func nicMAC(kind model.GuestKind, raw string) (string, bool) {
	for _, item := range strings.Split(raw, ",") {
		key, value, found := strings.Cut(item, "=")
		if !found || !carriesMAC(kind, key) {
			continue
		}
		if mac, err := model.NormalizeMAC(value); err == nil {
			return mac, true
		}
	}
	return "", false
}

// smbiosUUID returns the first well-formed uuid of a QEMU "smbios1" value,
// lower-cased. Other items, well-formed or not, do not matter.
func smbiosUUID(raw string) (string, bool) {
	uuid, ok := findItem(raw, "uuid", validUUID)
	return strings.ToLower(uuid), ok
}

// creationTime returns the first well-formed ctime of a QEMU "meta" value.
func creationTime(raw string) (string, bool) {
	return findItem(raw, "ctime", func(s string) bool {
		_, err := strconv.ParseUint(s, 10, 64)
		return err == nil
	})
}

// findItem returns the value of the first "key=value" item of a
// comma-separated property string whose value passes valid. Unlike
// splitPairs it does not care about the rest of the string.
func findItem(raw, key string, valid func(string) bool) (string, bool) {
	for _, item := range strings.Split(raw, ",") {
		k, v, found := strings.Cut(item, "=")
		if found && k == key && valid(v) {
			return v, true
		}
	}
	return "", false
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
