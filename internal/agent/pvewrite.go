package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// maxCard is the highest card index of a guest: net0 to net31.
const maxCard = 31

var (
	bridgeName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,14}$`)
	digestText  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	ifaceInside = regexp.MustCompile(`^eth(?:[0-9]|[12][0-9]|3[01])$`)

	// pcoMAC is the prefix of the MACs pco gives the cards of guests.
	pcoMAC = net.HardwareAddr{0x06, 0x50, 0x43, 0x4f}
)

// pveWrite writes one card of a guest on this node with qm or pct, as the
// controller asked: the whole card or its removal, under the digest of the
// configuration the controller read. The agent builds the argument vector
// itself, of the options it knows only, for a guest the desired state places
// on this node, and only for a card of pco's own. The write is bounded by
// pveWriteTimeout.
func (a *Agent) pveWrite(ctx context.Context, s agentproto.Snapshot, args json.RawMessage) error {
	if a.d.Kind != pki.KindHost {
		return errors.New("an appliance agent writes no guest configuration; the controller writes it through the Proxmox API")
	}
	if a.d.Runner == nil {
		return errors.New("this node runs no qm or pct")
	}
	w, err := decodeWrite(args)
	if err != nil {
		return err
	}
	argv, err := a.cardArgv(s, w)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, pveWriteTimeout)
	defer cancel()
	if _, err := a.d.Runner(ctx, argv...); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv[:3], " "), err)
	}
	return nil
}

// decodeWrite reads the request of a pveWrite action, which holds the fields
// of agentproto.PVEWrite and nothing else.
func decodeWrite(raw json.RawMessage) (agentproto.PVEWrite, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var w agentproto.PVEWrite
	if err := dec.Decode(&w); err != nil {
		return agentproto.PVEWrite{}, fmt.Errorf("reading the card to write: %w", err)
	}
	return w, nil
}

// cardArgv is the argument vector of qm set or pct set that writes w.
func (a *Agent) cardArgv(s agentproto.Snapshot, w agentproto.PVEWrite) ([]string, error) {
	g, ok := guestOf(s, w.Guest)
	switch {
	case !ok:
		return nil, fmt.Errorf("%s is not a guest of the desired state", w.Guest)
	case g.Node != a.d.Node:
		return nil, fmt.Errorf("%s runs on node %s, not on %s", w.Guest, g.Node, a.d.Node)
	case w.NIC < 0 || w.NIC > maxCard:
		return nil, fmt.Errorf("card net%d is not net0 to net%d", w.NIC, maxCard)
	case !digestText.MatchString(w.Digest):
		return nil, fmt.Errorf("digest %q is not the digest of a guest's configuration", w.Digest)
	}
	var bin string
	switch w.Guest.Kind {
	case model.KindQEMU:
		bin = "qm"
	case model.KindLXC:
		bin = "pct"
	default:
		return nil, fmt.Errorf("%s is no guest pco writes", w.Guest)
	}
	key := "net" + strconv.Itoa(w.NIC)
	if cur, exists := g.NIC(w.NIC); exists && !ours(cur.MAC) {
		return nil, fmt.Errorf("%s of %s has MAC %s, which is not one of pco's: pco writes only its own cards", key, w.Guest, cur.MAC)
	} else if w.Remove && !exists {
		return nil, fmt.Errorf("%s has no card %s in the desired state", w.Guest, key)
	}
	vmid := strconv.Itoa(w.Guest.VMID)
	if w.Remove {
		if w.Bridge != "" || w.Tag != 0 || w.MAC != "" || w.Name != "" || w.IP.IsValid() || w.Firewall != nil || w.LinkDown != nil {
			return nil, errors.New("a removal names its card and nothing else")
		}
		return []string{bin, "set", vmid, "--delete", key, "--digest", w.Digest}, nil
	}
	spec, err := cardSpec(w)
	if err != nil {
		return nil, err
	}
	return []string{bin, "set", vmid, "--" + key, spec, "--digest", w.Digest}, nil
}

// cardSpec is the value of a card: a VM's virtio card, or a container's with
// its interface inside and its address there, with the options pco sets.
func cardSpec(w agentproto.PVEWrite) (string, error) {
	if w.MAC == "" || w.Bridge == "" {
		return "", errors.New("a card is set with its MAC and its bridge")
	}
	mac, err := net.ParseMAC(w.MAC)
	switch {
	case err != nil || !ours(w.MAC):
		return "", fmt.Errorf("MAC %s is not one of pco's (06:50:43:4f:xx:xx)", w.MAC)
	case !bridgeName.MatchString(w.Bridge):
		return "", fmt.Errorf("bridge %q is not the name of a bridge", w.Bridge)
	case w.Tag != 0 && (w.Tag < 1 || w.Tag > 4094):
		return "", fmt.Errorf("tag %d is not 1 to 4094", w.Tag)
	}
	var parts []string
	switch w.Guest.Kind {
	case model.KindQEMU:
		if w.Name != "" || w.IP.IsValid() {
			return "", errors.New("a name and an address are a container's")
		}
		parts = []string{"virtio=" + mac.String(), "bridge=" + w.Bridge}
	default:
		if !ifaceInside.MatchString(w.Name) {
			return "", fmt.Errorf("name %q is not the name of an interface in a container (eth0 to eth%d)", w.Name, maxCard)
		}
		parts = []string{"name=" + w.Name, "bridge=" + w.Bridge, "hwaddr=" + mac.String()}
		if w.IP.IsValid() {
			if !w.IP.Addr().Is4() {
				return "", fmt.Errorf("address %s is not an IPv4 address", w.IP)
			}
			parts = append(parts, "ip="+w.IP.String())
		}
	}
	if w.Tag != 0 {
		parts = append(parts, "tag="+strconv.Itoa(w.Tag))
	}
	if w.Firewall != nil {
		parts = append(parts, "firewall="+oneOrZero(*w.Firewall))
	}
	if w.LinkDown != nil && *w.LinkDown {
		parts = append(parts, "link_down=1")
	}
	return strings.Join(parts, ","), nil
}

func oneOrZero(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ours reports whether mac is one pco gives the cards of guests.
func ours(mac string) bool {
	hw, err := net.ParseMAC(mac)
	return err == nil && len(hw) == 6 && bytes.Equal(hw[:4], pcoMAC)
}

// guestOf finds a guest in the snapshot.
func guestOf(s agentproto.Snapshot, ref model.GuestRef) (model.Guest, bool) {
	for _, t := range s.Targets {
		if t.Guest != nil && t.Guest.Ref == ref {
			return *t.Guest, true
		}
	}
	for _, g := range s.Guests {
		if g.Ref == ref {
			return g, true
		}
	}
	return model.Guest{}, false
}
