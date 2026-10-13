package managednet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// Configurer changes the cards of a guest pco may write. On the host it runs
// qm set and pct set as root with --digest; in the appliance it calls the
// Proxmox API under the admin's grant.
type Configurer interface {
	// AddNIC adds net<index> with spec and returns whether the guest took it
	// live; false when the change waits for a restart, as on a QEMU guest
	// without network hotplug, which the pending list shows. A card on the
	// managed network at that index is replaced; a card on another bridge
	// is never touched.
	AddNIC(ctx context.Context, g model.Guest, index int, spec NICSpec) (live bool, err error)
	// RemoveNIC deletes net<index>. On a QEMU guest without network hotplug
	// the deletion waits for a restart, and the caller reads the
	// configuration until the card is gone.
	RemoveNIC(ctx context.Context, g model.Guest, index int) error
	// SetLinkDown sets link_down=1 on net<index> and leaves the rest of the
	// card as it is: the restrictive answer to a foreign card. On a QEMU
	// guest without network hotplug the change waits for a restart, and the
	// error is ErrPending.
	SetLinkDown(ctx context.Context, g model.Guest, index int) error
	// SetLinkUp takes link_down off net<index>, a card on the managed
	// network pco added with its link down, and leaves the rest of the
	// card as it is.
	SetLinkUp(ctx context.Context, g model.Guest, index int) error
	// CanWrite reports whether this configurer may change the guest now, and
	// why not when it may not: on the host, for a guest on this node; in the
	// appliance, when the access control grants VM.Config.Network on the
	// guest, directly or through its pool, and the reason is then the pveum
	// line that grants it.
	CanWrite(ctx context.Context, g model.Guest) (bool, string, error)
}

// NICSpec is the card pco puts on a guest.
type NICSpec struct {
	// Name is the interface name of an LXC card. Empty, the configurer takes
	// the lowest eth<k> no other card of the guest is named; empty for QEMU.
	Name     string
	MAC      string
	Bridge   string       // the managed network, pconet
	Addr     netip.Prefix // LXC only, as ip=<addr>/<len>; zero for QEMU
	Firewall bool         // false on the host, true in the appliance
	// LinkDown adds the card with link_down=1: in the appliance its link
	// comes up only once the firewall that isolates it is enforced.
	LinkDown bool
}

// ErrPending is the error of a change that Proxmox made pending: the guest
// takes it at its next start.
var ErrPending = errors.New("the change waits for the guest's restart")

// ErrLinkDownRefused is the error of a SetLinkDown whose write Proxmox
// refused for the option link_down itself, as a container of a release
// without it does; the caller may remove the card instead. Any other failure
// of the write is not it.
var ErrLinkDownRefused = errors.New("link_down is refused on this card")

// linkDownRefusal is how the schema of Proxmox refuses an option a card does
// not have, for link_down: the command line the error repeats holds
// "link_down=1", which is not it.
const linkDownRefusal = "link_down: property is not defined in schema"

// maxNICIndex is the highest netN a guest can have.
const maxNICIndex = 31

// FreeIndex returns the lowest N for which the guest has no card net<N>, the
// index a new card of pco's takes; false when every one is taken. A card the
// snapshot could not read is not in g.NICs: AddNIC never replaces a card that
// is not on the managed network, so such a card is refused, not overwritten.
func FreeIndex(g model.Guest) (int, bool) {
	for i := 0; i <= maxNICIndex; i++ {
		if _, taken := g.NIC(i); !taken {
			return i, true
		}
	}
	return 0, false
}

// guestConfig is how a configurer reads and changes the configuration of a
// guest.
type guestConfig interface {
	// read returns the configuration with its pending changes applied, as
	// the guest will run after its next start, every value as text, and the
	// digest of the configuration. A card whose addition waits for a restart
	// is there, one whose deletion waits is not.
	read(ctx context.Context, g model.Guest) (map[string]string, string, error)
	// set sets key under digest; a stale digest is pve.ErrStaleDigest.
	set(ctx context.Context, g model.Guest, key, value, digest string) error
	// remove deletes key under digest; a stale digest is pve.ErrStaleDigest.
	remove(ctx context.Context, g model.Guest, key, digest string) error
	// pending lists the keys of a QEMU guest whose change waits for a
	// restart.
	pending(ctx context.Context, g model.Guest) ([]string, error)
}

// cards changes the cards of guests through a guestConfig. LXC and QEMU
// share it: they differ in the value of a card and in whether a change can
// wait for a restart.
type cards struct{ cfg guestConfig }

func (c cards) add(ctx context.Context, g model.Guest, index int, spec NICSpec) (bool, error) {
	key, err := nicKey(index)
	if err != nil {
		return false, err
	}
	mac, err := model.NormalizeMAC(spec.MAC)
	if err != nil {
		return false, err
	}
	err = c.change(ctx, g, key, func(values map[string]string) (string, bool, error) {
		if other, ok := cardWithMAC(values, g.Ref.Kind, mac); ok && other != key {
			return "", false, fmt.Errorf("the card %s is %s already", mac, other)
		}
		if cur, ok := values[key]; ok {
			if bridge := field(cur, "bridge"); bridge != gateway.ManagedBridge {
				return "", false, fmt.Errorf("%s is a card on %s; pco replaces only a card on %s", key, bridge, gateway.ManagedBridge)
			}
			if strings.EqualFold(macOf(cur, g.Ref.Kind), mac) {
				return "", false, nil
			}
		}
		value, err := nicValue(g.Ref.Kind, values, key, spec, mac)
		return value, err == nil, err
	})
	if err != nil {
		return false, err
	}
	return c.live(ctx, g, key)
}

func (c cards) removeNIC(ctx context.Context, g model.Guest, index int) error {
	key, err := nicKey(index)
	if err != nil {
		return err
	}
	err = c.change(ctx, g, key, func(values map[string]string) (string, bool, error) {
		cur, ok := values[key]
		if !ok {
			return "", false, nil
		}
		if bridge := field(cur, "bridge"); bridge != gateway.ManagedBridge {
			return "", false, fmt.Errorf("%s is a card on %s; pco removes only a card on %s", key, bridge, gateway.ManagedBridge)
		}
		return "", true, nil
	})
	return err
}

func (c cards) linkDown(ctx context.Context, g model.Guest, index int) error {
	key, err := nicKey(index)
	if err != nil {
		return err
	}
	err = c.change(ctx, g, key, func(values map[string]string) (string, bool, error) {
		cur, ok := values[key]
		switch {
		case !ok:
			return "", false, fmt.Errorf("the guest has no %s", key)
		case field(cur, "bridge") != gateway.ManagedBridge:
			return "", false, fmt.Errorf("%s is a card on %s; pco sets down only a card on %s", key, field(cur, "bridge"), gateway.ManagedBridge)
		case isTrue(field(cur, "link_down")):
			return "", false, nil
		}
		return withLinkDown(cur), true, nil
	})
	switch {
	case err != nil && !errors.Is(err, pve.ErrStaleDigest) && strings.Contains(err.Error(), linkDownRefusal):
		return fmt.Errorf("%w: %w", ErrLinkDownRefused, err)
	case err != nil:
		return err
	}
	live, err := c.live(ctx, g, key)
	if err == nil && !live {
		err = ErrPending
	}
	return err
}

func (c cards) linkUp(ctx context.Context, g model.Guest, index int) error {
	key, err := nicKey(index)
	if err != nil {
		return err
	}
	err = c.change(ctx, g, key, func(values map[string]string) (string, bool, error) {
		cur, ok := values[key]
		switch {
		case !ok:
			return "", false, fmt.Errorf("the guest has no %s", key)
		case field(cur, "bridge") != gateway.ManagedBridge:
			return "", false, fmt.Errorf("%s is a card on %s; pco sets up only a card on %s", key, field(cur, "bridge"), gateway.ManagedBridge)
		case !isTrue(field(cur, "link_down")):
			return "", false, nil
		}
		return withoutField(cur, "link_down"), true, nil
	})
	if err != nil {
		return err
	}
	live, err := c.live(ctx, g, key)
	if err == nil && !live {
		err = ErrPending
	}
	return err
}

// change reads the configuration of g and lets plan say what key becomes:
// its new value, or nothing to delete it, and whether to write at all. The
// write is made under the digest of what was read; a digest that went stale
// meanwhile is read again once.
func (c cards) change(ctx context.Context, g model.Guest, key string, plan func(values map[string]string) (value string, write bool, err error)) error {
	for attempt := 0; ; attempt++ {
		values, digest, err := c.cfg.read(ctx, g)
		switch {
		case err != nil:
			return fmt.Errorf("reading the configuration: %w", err)
		case digest == "":
			return errors.New("reading the configuration: it has no digest")
		}
		value, write, err := plan(values)
		if err != nil || !write {
			return err
		}
		if value == "" {
			err = c.cfg.remove(ctx, g, key, digest)
		} else {
			err = c.cfg.set(ctx, g, key, value, digest)
		}
		if errors.Is(err, pve.ErrStaleDigest) && attempt == 0 {
			continue
		}
		return err
	}
}

// live reports whether the guest runs with key as it is configured: always
// for a container, and for a VM unless the change waits in its pending list.
func (c cards) live(ctx context.Context, g model.Guest, key string) (bool, error) {
	if g.Ref.Kind != model.KindQEMU {
		return true, nil
	}
	keys, err := c.cfg.pending(ctx, g)
	if err != nil {
		return false, fmt.Errorf("reading the pending changes: %w", err)
	}
	return !slices.Contains(keys, key), nil
}

func nicKey(index int) (string, error) {
	if index < 0 || index > maxNICIndex {
		return "", fmt.Errorf("net%d is not net0 to net%d", index, maxNICIndex)
	}
	return "net" + strconv.Itoa(index), nil
}

// nicValue is the value of the card spec at key, in a configuration with the
// values given: a virtio card for QEMU, a veth card with its name and static
// address for LXC.
func nicValue(kind model.GuestKind, values map[string]string, key string, spec NICSpec, mac string) (string, error) {
	firewall := 0
	if spec.Firewall {
		firewall = 1
	}
	link := ""
	if spec.LinkDown {
		link = ",link_down=1"
	}
	if kind == model.KindQEMU {
		return fmt.Sprintf("virtio=%s,bridge=%s,firewall=%d%s", mac, spec.Bridge, firewall, link), nil
	}
	if !spec.Addr.IsValid() {
		return "", errors.New("a container's card on the managed network needs its address")
	}
	name := spec.Name
	if name == "" {
		name = freeName(values, key)
	}
	return fmt.Sprintf("name=%s,bridge=%s,hwaddr=%s,ip=%s,firewall=%d%s", name, spec.Bridge, mac, spec.Addr, firewall, link), nil
}

// freeName returns the lowest eth<k> that no card of a container but the one
// at key is named.
func freeName(values map[string]string, key string) string {
	taken := map[string]bool{}
	for k, v := range values {
		if _, ok := pve.NICIndex(k); ok && k != key {
			taken[field(v, "name")] = true
		}
	}
	for k := 0; ; k++ {
		if name := "eth" + strconv.Itoa(k); !taken[name] {
			return name
		}
	}
}

// cardWithMAC returns the key of the card with the MAC.
func cardWithMAC(values map[string]string, kind model.GuestKind, mac string) (string, bool) {
	for k, v := range values {
		if _, ok := pve.NICIndex(k); ok && strings.EqualFold(macOf(v, kind), mac) {
			return k, true
		}
	}
	return "", false
}

// macOf returns the MAC of a card: hwaddr= on a container; on a VM
// macaddr=, or the value of the model that leads the card, as in
// virtio=<mac>.
func macOf(value string, kind model.GuestKind) string {
	if kind == model.KindLXC {
		return field(value, "hwaddr")
	}
	if mac := field(value, "macaddr"); mac != "" {
		return mac
	}
	first, _, _ := strings.Cut(value, ",")
	_, mac, _ := strings.Cut(first, "=")
	return mac
}

// field returns the value of key in a card's comma-separated key=value list.
func field(value, key string) string {
	for part := range strings.SplitSeq(value, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// withLinkDown returns the card with link_down=1 at its end, and without the
// link_down it had.
func withLinkDown(value string) string {
	return withoutField(value, "link_down") + ",link_down=1"
}

// withoutField returns the card without key.
func withoutField(value, key string) string {
	var parts []string
	for part := range strings.SplitSeq(value, ",") {
		if k, _, _ := strings.Cut(part, "="); k != key {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ",")
}

// isTrue reads a Proxmox boolean.
func isTrue(s string) bool {
	switch strings.ToLower(s) {
	case "1", "on", "yes", "true":
		return true
	}
	return false
}
