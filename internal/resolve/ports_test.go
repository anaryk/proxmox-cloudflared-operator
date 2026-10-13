package resolve

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyPort(t *testing.T) {
	tests := []struct {
		name     string
		linkType string
		mac      string
		want     PortKind
	}{
		{name: "tap101i0", linkType: "tuntap", want: PortGuest},
		{name: "veth200i1", linkType: "veth", want: PortGuest},
		{name: "fwpr101p0", linkType: "veth", want: PortGuest},
		{name: "vethpco3", linkType: "veth", want: PortLeg},
		{name: "vethpcop", linkType: "veth", want: PortLeg},
		{name: "port7", linkType: "veth", mac: "fe:50:43:4f:00:07", want: PortLeg},
		{name: "eno1", linkType: "device", want: PortUplink},
		{name: "bond0", linkType: "bond", want: PortUplink},
		{name: "eno1.20", linkType: "vlan", want: PortUplink},
		{name: "vxlan_labx", linkType: "vxlan", want: PortUplink},
		{name: "dummy0", linkType: "dummy", want: PortUnknown},
		{name: "tap101", linkType: "tuntap", want: PortUnknown},
		{name: "veth3f2a1c", linkType: "veth", want: PortUnknown},
		{name: "fwbr101i0", linkType: "bridge", want: PortUnknown},
		{name: "tap101i0", linkType: "device", want: PortGuest},
	}
	for _, tt := range tests {
		t.Run(tt.name+" "+tt.linkType, func(t *testing.T) {
			require.Equal(t, tt.want, ClassifyPort(tt.name, tt.linkType, tt.mac))
		})
	}
}
