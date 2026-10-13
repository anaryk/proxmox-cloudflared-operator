package engine

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A MAC already told of within five minutes has no event of its own, but
// the guests are read again and, in the appliance, the isolation is dropped
// at every report: a passing cycle in between must not hide a card that
// still speaks.
func TestEveryReportOfAnUnknownMACActs(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.warm()
	mac := hardware(t, "bc:24:11:00:99:99")
	holds := func() {
		m.eng.stateMu.Lock()
		m.eng.state.Managed.Filtered = &FilteredView{Holds: true, Backend: "pve-firewall", Offered: true}
		m.eng.stateMu.Unlock()
	}

	holds()
	m.eng.UnknownMAC(mac, m.clock.now())
	require.False(t, m.eng.State().Managed.Filtered.Holds)
	holds() // a cycle passed
	m.eng.UnknownMAC(mac, m.clock.now().Add(time.Minute))
	require.False(t, m.eng.State().Managed.Filtered.Holds, "the card still speaks")

	events := 0
	for _, ev := range m.managedEvents() {
		if strings.Contains(ev, "bc:24:11:00:99:99") {
			events++
		}
	}
	require.Equal(t, 1, events, "one event per five minutes")
	require.Len(t, m.inv.forcedNodes(), 2, "read again each time")
}

// A guest that sprays MACs fills the record of the reported ones; a spoof
// hidden in that flood is still acted on, without an event of its own, and
// the cycle says how many went without one.
func TestAFloodOfMACsHidesNoSpoof(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.warm()
	for i := range 1000 {
		m.eng.UnknownMAC(net.HardwareAddr{0xbc, 0x24, 0x11, 0x01, byte(i >> 8), byte(i)}, m.clock.now())
	}
	m.eng.macMu.Lock()
	require.Len(t, m.eng.unknownMACs, maxUnknownMACs)
	m.eng.macMu.Unlock()

	m.eng.stateMu.Lock()
	m.eng.state.Managed.Filtered = &FilteredView{Holds: true, Backend: "pve-firewall", Offered: true}
	m.eng.stateMu.Unlock()
	forced := len(m.inv.forcedNodes())
	m.eng.UnknownMAC(hardware(t, "06:50:43:4f:76:02"), m.clock.now())
	require.False(t, m.eng.State().Managed.Filtered.Holds, "the spoof in the flood drops the isolation")
	require.Len(t, m.inv.forcedNodes(), forced+1)

	st := m.next()
	want := fmt.Sprintf(problemUnknownMACsDropped, 1000+1-maxUnknownMACs, maxUnknownMACs)
	require.Contains(t, st.Problems, want)
	st = m.next()
	require.NotContains(t, st.Problems, want, "said for the cycle they were seen in")
}
