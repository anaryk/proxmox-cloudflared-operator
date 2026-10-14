//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// consentTag is the per-guest consent to a card on the managed network,
	// honoured while it is registered.
	consentTag = "cf-tunnel-managed"
	// detachGrace is the grace of the run before a card goes: the shortest
	// allowed, so that a detach can be waited for.
	detachGrace = time.Minute
	// A guest that ran attached for longer than the grace while the
	// registration was gone proves that nothing was detached meanwhile.
	unregisteredFor = detachGrace + 30*time.Second
)

// enableManaged makes the managed network as an admin does on a host, with
// the grace of the run, registers the consent tag, and waits until the leg
// is up and the DHCP server listens.
func (s *suite) enableManaged(t testing.TB) {
	t.Helper()
	if s.managed {
		return
	}
	s.registerTag(t, consentTag)
	s.editSettings(t, func(v *store.Settings) { v.Managed.DetachGrace = store.Duration(detachGrace) })

	marker(t, managedMarker, "")
	s.managed = true
	out, err := s.pcoCombined("network", "managed", "enable", "--yes")
	t.Logf("pco network managed enable:\n%s", out)
	require.NoError(t, err)
	// The SDN apply reloads the node's network, which takes the address
	// the run gave the bridge.
	s.restoreAddress(t)
	s.must(t, "pvesh", "get", "/cluster/sdn/vnets/"+gateway.ManagedBridge)
	s.sync(t)
	s.waitState(t, "the managed network with its leg up and DHCP listening", 3*time.Minute, func(st engine.State) bool {
		m := st.Managed
		return m.Enabled && m.Leg != nil && m.Leg.Up && m.DHCP != nil && m.DHCP.Bound && m.Vnet.OK
	})
}

// setConsent switches the consent mode of the managed network, as an admin
// edits the settings.
func (s *suite) setConsent(t testing.TB, mode string) {
	t.Helper()
	s.editSettings(t, func(v *store.Settings) { v.Managed.Consent = mode })
	s.sync(t)
	s.waitState(t, "consent "+mode, time.Minute, func(st engine.State) bool { return st.Managed.Consent == mode })
}

func attachmentOf(st engine.State, owner string) (engine.AttachmentView, bool) {
	i := slices.IndexFunc(st.Managed.Attachments, func(a engine.AttachmentView) bool { return a.Owner == owner })
	if i < 0 {
		return engine.AttachmentView{}, false
	}
	return st.Managed.Attachments[i], true
}

// waitAttachment waits until the guest's card is in one of the states.
func (s *suite) waitAttachment(t testing.TB, owner string, states ...string) engine.AttachmentView {
	t.Helper()
	var found engine.AttachmentView
	s.waitState(t, fmt.Sprintf("the card of %s to be %s", owner, strings.Join(states, " or ")), 3*time.Minute, func(st engine.State) bool {
		a, ok := attachmentOf(st, owner)
		found = a
		return ok && slices.Contains(states, a.State)
	})
	return found
}

// vmidOf is the id of a guest named as lxc/9361.
func vmidOf(owner string) int {
	_, id, _ := strings.Cut(owner, "/")
	vmid, _ := strconv.Atoi(id)
	return vmid
}

func foreignOf(st engine.State, vmid, nic int) (engine.ForeignView, bool) {
	i := slices.IndexFunc(st.Managed.Foreign, func(f engine.ForeignView) bool { return f.VMID == vmid && f.NIC == nic })
	if i < 0 {
		return engine.ForeignView{}, false
	}
	return st.Managed.Foreign[i], true
}

// managedCard is the line of the guest's configuration of its card on
// pconet, "" when there is none.
func (s *suite) managedCard(t testing.TB, vmid int) string {
	t.Helper()
	for _, line := range s.configLines(t, vmid) {
		if strings.HasPrefix(line, "net") && strings.Contains(line, "bridge="+gateway.ManagedBridge) {
			return line
		}
	}
	return ""
}

// configLines are the lines of a container's configuration.
func (s *suite) configLines(t testing.TB, vmid int) []string {
	t.Helper()
	var lines []string
	for line := range strings.Lines(s.must(t, "pct", "config", strconv.Itoa(vmid))) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// withoutCard is a configuration without the lines of cards on pconet: what
// an attach must leave as it was.
func withoutCard(lines []string) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool {
		return strings.HasPrefix(l, "net") && strings.Contains(l, "bridge="+gateway.ManagedBridge)
	})
}

// cardValue reads a key of a card's line, as hwaddr=.
func cardValue(line, key string) string {
	_, rest, _ := strings.Cut(line, ": ")
	for part := range strings.SplitSeq(rest, ",") {
		if v, ok := strings.CutPrefix(part, key+"="); ok {
			return strings.ToLower(v)
		}
	}
	return ""
}

// guardLists says whether the host's guard table keeps a card to its port:
// the port, MAC and address of a managed guest are one element of its set.
func (s *suite) guardLists(t testing.TB, port, mac string, addr netip.Addr) bool {
	t.Helper()
	out, err := s.run("nft", "-j", "list", "table", "bridge", "pco_guard")
	require.NoError(t, err)
	var listing struct {
		Nftables []struct {
			Set *struct {
				Name string `json:"name"`
				Elem []any  `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listing), out)
	for _, item := range listing.Nftables {
		if item.Set == nil || item.Set.Name != "cards" {
			continue
		}
		for _, e := range item.Set.Elem {
			raw, _ := json.Marshal(e)
			text := strings.ToLower(string(raw))
			if strings.Contains(text, `"`+port+`"`) && strings.Contains(text, strings.ToLower(mac)) && strings.Contains(text, addr.String()) {
				return true
			}
		}
	}
	return false
}

// stickyPort is the port of pconet that holds the static sticky entry of a
// MAC, "" when there is none.
func (s *suite) stickyPort(t testing.TB, mac string) string {
	t.Helper()
	for line := range strings.Lines(s.must(t, "bridge", "fdb", "show", "br", gateway.ManagedBridge)) {
		f := strings.Fields(strings.ToLower(line))
		if len(f) > 2 && f[0] == strings.ToLower(mac) && slices.Contains(f, "sticky") && f[1] == "dev" {
			return f[2]
		}
	}
	return ""
}

// pinnedIn says whether the namespace pins the address to the MAC.
func (s *suite) pinnedIn(t testing.TB, addr netip.Addr, mac string) bool {
	t.Helper()
	out, err := s.netns("ip", "neigh", "show", "to", addr.String())
	require.NoError(t, err)
	l := strings.ToLower(out)
	return strings.Contains(l, strings.ToLower(mac)) && strings.Contains(l, "permanent")
}

// nodeAddr is the address of the node on its LAN, and gw its gateway there.
func (s *suite) nodeLAN(t testing.TB) (addr, gw string) {
	t.Helper()
	out := s.must(t, "ip", "-j", "route", "get", "1.1.1.1")
	var routes []struct {
		Gateway string `json:"gateway"`
		PrefSrc string `json:"prefsrc"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &routes), out)
	require.NotEmpty(t, routes, out)
	return routes[0].PrefSrc, routes[0].Gateway
}

// lanPort is the Proxmox port of a container's card: veth<vmid>i<n>, or
// fwpr<vmid>p<n> when the card has its firewall on.
func lanPort(vmid, nic int, firewall bool) string {
	if firewall {
		return fmt.Sprintf("fwpr%dp%d", vmid, nic)
	}
	return fmt.Sprintf("veth%di%d", vmid, nic)
}
