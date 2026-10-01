package model

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuestRefRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		ref  GuestRef
		text string
	}{
		{"qemu", GuestRef{Kind: KindQEMU, VMID: 101}, "qemu/101"},
		{"lxc", GuestRef{Kind: KindLXC, VMID: 200}, "lxc/200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.text, tt.ref.String())
			got, err := ParseGuestRef(tt.text)
			require.NoError(t, err)
			require.Equal(t, tt.ref, got)
		})
	}
}

func TestParseGuestRefRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"unknown kind", "vm/1"},
		{"non numeric vmid", "qemu/x"},
		{"zero vmid", "qemu/0"},
		{"negative vmid", "qemu/-3"},
		{"missing separator", "qemu"},
		{"missing vmid", "qemu/"},
		{"missing kind", "/101"},
		{"extra segment", "qemu/1/2"},
		{"leading zeros", "qemu/007"},
		{"plus sign", "qemu/+7"},
		{"trailing space", "qemu/7 "},
		{"leading space", " qemu/7"},
		{"space before the vmid", "qemu/ 7"},
		{"upper case kind", "QEMU/7"},
		{"vmid above the range", "qemu/2147483648"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGuestRef(tt.in)
			require.Error(t, err)
		})
	}
}

func TestNormalizeMAC(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"dash separated upper case", "BC-24-11-00-AA-B5", "bc:24:11:00:aa:b5"},
		{"colon separated upper case", "BC:24:11:00:AA:B5", "bc:24:11:00:aa:b5"},
		{"already normalized", "bc:24:11:00:aa:b5", "bc:24:11:00:aa:b5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeMAC(tt.in)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeMACRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"non hex digits", "zz:24:11:00:aa:b5"},
		{"too short", "bc:24:11:00:aa"},
		{"too long", "bc:24:11:00:aa:b5:c6"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeMAC(tt.in)
			require.Error(t, err)
		})
	}
}

func TestGuestHasTag(t *testing.T) {
	g := Guest{Tags: []string{"prod", "cf-tunnel"}}
	require.True(t, g.HasTag("cf-tunnel"))
	require.True(t, g.HasTag("prod"))
	require.False(t, g.HasTag("dev"))
	require.False(t, Guest{}.HasTag("prod"))
}

func TestGuestHasTagIsCaseSensitive(t *testing.T) {
	require.False(t, Guest{Tags: []string{"cf-tunnel"}}.HasTag("CF-Tunnel"))
	require.False(t, Guest{Tags: []string{"CF-Tunnel"}}.HasTag("cf-tunnel"))
	require.True(t, Guest{Tags: []string{"CF-Tunnel"}}.HasTag("CF-Tunnel"))
}

func TestGuestNIC(t *testing.T) {
	g := Guest{NICs: []NIC{{Index: 0, Bridge: "vmbr0"}, {Index: 1, Bridge: "vmbr1"}}}

	nic, ok := g.NIC(1)
	require.True(t, ok)
	require.Equal(t, "vmbr1", nic.Bridge)

	_, ok = g.NIC(2)
	require.False(t, ok)
}

func TestGuestJSONRoundTrip(t *testing.T) {
	g := Guest{
		Ref:      GuestRef{Kind: KindQEMU, VMID: 101},
		Name:     "web",
		Node:     "pve1",
		Running:  true,
		Tags:     []string{"cf-tunnel"},
		Identity: "abc",
		NICs: []NIC{{
			Index:  0,
			MAC:    "bc:24:11:00:aa:b5",
			Bridge: "vmbr0",
			VLAN:   20,
			Static: []netip.Addr{netip.MustParseAddr("10.0.20.5")},
		}},
		Reported: []ReportedAddr{{
			Iface: "eth0",
			MAC:   "bc:24:11:00:aa:b5",
			Addr:  netip.MustParseAddr("10.0.20.5"),
		}},
	}

	data, err := json.Marshal(g)
	require.NoError(t, err)

	var got Guest
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, g, got)
}
