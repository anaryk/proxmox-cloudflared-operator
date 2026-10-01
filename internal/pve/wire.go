package pve

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"unicode"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// flexBool reads the three spellings Proxmox uses for a boolean: 0/1,
// true/false and "0"/"1".
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch string(bytes.Trim(bytes.TrimSpace(data), `"`)) {
	case "1", "true":
		*b = true
	case "0", "false", "", "null":
		*b = false
	default:
		return fmt.Errorf("%s is not a boolean", data)
	}
	return nil
}

type versionWire struct {
	Release string `json:"release"`
	Version string `json:"version"`
}

type resourceWire struct {
	Type     string   `json:"type"`
	VMID     int      `json:"vmid"`
	Name     string   `json:"name"`
	Node     string   `json:"node"`
	Status   string   `json:"status"`
	Template flexBool `json:"template"`
	Tags     string   `json:"tags"`
}

type agentWire struct {
	Result []agentIfaceWire `json:"result"`
}

type agentIfaceWire struct {
	Name        string          `json:"name"`
	MAC         string          `json:"hardware-address"`
	IPAddresses []agentAddrWire `json:"ip-addresses"`
}

type agentAddrWire struct {
	Address string `json:"ip-address"`
	Type    string `json:"ip-address-type"`
}

type lxcIfaceWire struct {
	Name string `json:"name"`
	MAC  string `json:"hwaddr"`
	Inet string `json:"inet"`
}

type clusterEntryWire struct {
	Type   string   `json:"type"`
	Name   string   `json:"name"`
	IP     string   `json:"ip"`
	Online flexBool `json:"online"`
	Local  flexBool `json:"local"`
}

type nodeIfaceWire struct {
	Name        string   `json:"iface"`
	Type        string   `json:"type"`
	Active      flexBool `json:"active"`
	CIDR        string   `json:"cidr"`
	BridgePorts string   `json:"bridge_ports"`
}

// parseVersion reads major and minor from the release ("9.1"), or from the
// full version ("9.1.1") when the release is missing or unreadable.
func parseVersion(release, version string) (Version, error) {
	if major, minor, ok := majorMinor(release); ok {
		return Version{Release: release, Major: major, Minor: minor}, nil
	}
	if major, minor, ok := majorMinor(version); ok {
		return Version{Release: fmt.Sprintf("%d.%d", major, minor), Major: major, Minor: minor}, nil
	}
	return Version{}, fmt.Errorf("unrecognised proxmox version (release %q, version %q)", release, version)
}

func majorMinor(s string) (major, minor int, ok bool) {
	first, rest, _ := strings.Cut(s, ".")
	second, _, _ := strings.Cut(rest, ".")
	major, okMajor := leadingInt(first)
	minor, okMinor := leadingInt(second)
	return major, minor, okMajor && okMinor
}

// leadingInt reads the digits at the start of s, so "0~rc1" is 0.
func leadingInt(s string) (int, bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	return n, err == nil
}

// SplitTags splits a Proxmox tag string on ';', ',' and white space and drops
// empty parts. Case is kept: tags are compared exactly as Proxmox stores them.
func SplitTags(s string) []string {
	tags := strings.FieldsFunc(s, func(r rune) bool {
		return r == ';' || r == ',' || unicode.IsSpace(r)
	})
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// fields splits s on white space, returning nil when there is nothing.
func fields(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

// stringify renders a JSON value as text: strings as themselves, anything
// else as its JSON spelling. A null is reported as absent.
func stringify(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return "", false
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", false
		}
		return s, true
	default:
		return string(raw), true
	}
}

// lenientMAC normalises a hardware address. Some interfaces report none, or
// one that is not 6 bytes long; they get an empty MAC instead of failing the
// whole answer.
func lenientMAC(s string) string {
	mac, err := model.NormalizeMAC(s)
	if err != nil {
		return ""
	}
	return mac
}

// parseAddr parses an IP address and reports whether it is IPv4. A valid IPv6
// address is not an error; callers ignore it.
func parseAddr(s string) (addr netip.Addr, isV4 bool, err error) {
	addr, err = netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false, err
	}
	return addr, addr.Is4(), nil
}

// parsePrefix is parseAddr for an address in CIDR form.
func parsePrefix(s string) (prefix netip.Prefix, isV4 bool, err error) {
	prefix, err = netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, false, err
	}
	return prefix, prefix.Addr().Is4(), nil
}
