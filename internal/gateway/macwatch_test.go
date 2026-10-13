package gateway

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// frame is an Ethernet frame from src to dst, of the type, with a payload of
// an ARP request's length.
func frame(dst, src string, typ uint16) []byte {
	f := append(append([]byte(nil), mac(dst)...), mac(src)...)
	f = append(f, byte(typ>>8), byte(typ))
	return append(f, make([]byte, 28)...)
}

type seenMACs struct{ macs []string }

func (s *seenMACs) unknown(m net.HardwareAddr) { s.macs = append(s.macs, m.String()) }

func newSourceWatch(seen *seenMACs, leases ...string) SourceWatch {
	return SourceWatch{
		Known: func(m net.HardwareAddr) bool {
			for _, l := range leases {
				if m.String() == l {
					return true
				}
			}
			return false
		},
		Own:     []net.HardwareAddr{ManagedLegMAC},
		Unknown: seen.unknown,
	}
}

func TestTheSourceWatchReportsWhatNoLeaseKnows(t *testing.T) {
	const lease = "06:50:43:4f:76:02"
	for _, c := range []struct {
		name  string
		frame []byte
		want  []string
	}{
		{"a lease's broadcast", frame("ff:ff:ff:ff:ff:ff", lease, 0x0806), nil},
		{"the leg's own", frame("ff:ff:ff:ff:ff:ff", "02:50:43:4f:ff:01", 0x0806), nil},
		{"the leg's bridge end", frame("ff:ff:ff:ff:ff:ff", "fe:50:43:4f:ff:01", 0x0806), nil},
		{"an unknown card's ARP", frame("ff:ff:ff:ff:ff:ff", "bc:24:11:00:99:99", 0x0806), []string{"bc:24:11:00:99:99"}},
		{"an unknown card's DHCP", frame("ff:ff:ff:ff:ff:ff", "bc:24:11:00:99:98", 0x0800), []string{"bc:24:11:00:99:98"}},
		{"an unknown card's unicast", frame("02:50:43:4f:ff:01", "bc:24:11:00:99:97", 0x0800), []string{"bc:24:11:00:99:97"}},
		{"a copied card with a managed MAC", frame("ff:ff:ff:ff:ff:ff", "06:50:43:4f:76:09", 0x0806), []string{"06:50:43:4f:76:09"}},
		{"a multicast source, which no card has", frame("ff:ff:ff:ff:ff:ff", "01:00:5e:00:00:01", 0x0806), nil},
		{"no source", frame("ff:ff:ff:ff:ff:ff", "00:00:00:00:00:00", 0x0806), nil},
		{"a frame too short", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xbc, 0x24}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var seen seenMACs
			newSourceWatch(&seen, lease).Frame(c.frame)
			require.Equal(t, c.want, seen.macs)
		})
	}
}

// Every frame of an unknown card is reported: how often that becomes an
// event is the engine's to say.
func TestEveryFrameOfAnUnknownCardIsReported(t *testing.T) {
	var seen seenMACs
	w := newSourceWatch(&seen)
	for range 3 {
		w.Frame(frame("ff:ff:ff:ff:ff:ff", "bc:24:11:00:99:99", 0x0806))
	}
	require.Equal(t, []string{"bc:24:11:00:99:99", "bc:24:11:00:99:99", "bc:24:11:00:99:99"}, seen.macs)
}

// The MAC passed on is a copy: the frame's buffer is read again.
func TestTheReportedMACIsACopy(t *testing.T) {
	var got net.HardwareAddr
	w := SourceWatch{Known: func(net.HardwareAddr) bool { return false }, Unknown: func(m net.HardwareAddr) { got = m }}
	f := frame("ff:ff:ff:ff:ff:ff", "bc:24:11:00:99:99", 0x0806)
	w.Frame(f)
	copy(f[6:12], mac("bc:24:11:00:00:00"))
	require.Equal(t, "bc:24:11:00:99:99", got.String())
}

// A watch without a lease reader reports nothing: a frame it cannot judge
// is not a finding.
func TestAWatchThatCannotTellReportsNothing(t *testing.T) {
	var seen seenMACs
	w := SourceWatch{Unknown: seen.unknown}
	w.Frame(frame("ff:ff:ff:ff:ff:ff", "bc:24:11:00:99:99", 0x0806))
	require.Empty(t, seen.macs)
}
