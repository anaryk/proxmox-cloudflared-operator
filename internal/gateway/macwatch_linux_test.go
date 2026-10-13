//go:build linux

package gateway

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// macSeen is what a watch of a test reported, with when.
type macSeen struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (m *macSeen) unknown(mac net.HardwareAddr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.seen[mac.String()]; !ok {
		m.seen[mac.String()] = time.Now()
	}
}

func (m *macSeen) at(mac string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.seen[mac]
	return at, ok
}

// The watch runs on the leg end of a pair in a namespace of its own, the
// guest sends from the other end: a frame from a source no lease knows is
// reported within 100 ms; a lease's, the leg's own, a frame the leg sends and
// the unicast traffic the leg carries are not.
func TestLinuxTheSourceWatchReportsAForeignCard(t *testing.T) {
	l := newLabNet(t)
	l.bridge(ManagedBridge, false)
	leg := l.guest("leg", ManagedBridge, 0, "10.203.118.1/24", "")
	guest := l.guest("g", ManagedBridge, 0, "10.203.118.2/24", "")
	legMAC, err := macIn(leg, "eth0")
	require.NoError(t, err)
	const lease = "06:50:43:4f:76:02"

	seen := &macSeen{seen: map[string]time.Time{}}
	w := SourceWatch{
		Known:   func(m net.HardwareAddr) bool { return m.String() == lease },
		Unknown: seen.unknown,
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(t.Context(), leg, "eth0") }()
	// The socket is open once a frame from an unknown source is seen.
	require.Eventually(t, func() bool {
		sendFrom(t, guest, mac("bc:24:11:00:00:01"))
		_, ok := seen.at("bc:24:11:00:00:01")
		return ok
	}, 2*time.Second, 20*time.Millisecond)

	sendFrom(t, guest, mac(lease))
	sendFrom(t, guest, bridgeEndMAC(legMAC))
	sendFrame(t, leg, broadcastARP(mac("bc:24:11:00:00:02")))
	unicast := frame(legMAC.String(), "bc:24:11:00:00:03", 0x0800)
	sendFrame(t, guest, unicast)

	sent := time.Now()
	sendFrom(t, guest, mac("bc:24:11:00:99:99"))
	require.Eventually(t, func() bool { _, ok := seen.at("bc:24:11:00:99:99"); return ok }, 100*time.Millisecond, time.Millisecond)
	at, _ := seen.at("bc:24:11:00:99:99")
	t.Logf("reported %s after the frame was sent", at.Sub(sent))

	for _, m := range []string{lease, bridgeEndMAC(legMAC).String(), "bc:24:11:00:00:02", "bc:24:11:00:00:03"} {
		_, ok := seen.at(m)
		require.False(t, ok, "%s was reported", m)
	}
	select {
	case err := <-done:
		t.Fatalf("the watch ended: %v", err)
	default:
	}
}

// A leg that goes away ends the watch with an error, so that the daemon
// starts it again on the leg there then.
func TestLinuxTheSourceWatchEndsWhenItsLegGoes(t *testing.T) {
	l := newLabNet(t)
	l.bridge(ManagedBridge, false)
	leg := l.guest("leg", ManagedBridge, 0, "10.203.118.1/24", "")
	guest := l.guest("g", ManagedBridge, 0, "10.203.118.2/24", "")
	seen := &macSeen{seen: map[string]time.Time{}}
	w := SourceWatch{Known: func(net.HardwareAddr) bool { return false }, Unknown: seen.unknown}
	done := make(chan error, 1)
	go func() { done <- w.Run(t.Context(), leg, "eth0") }()
	require.Eventually(t, func() bool {
		sendFrom(t, guest, mac("bc:24:11:00:00:01"))
		_, ok := seen.at("bc:24:11:00:00:01")
		return ok
	}, 2*time.Second, 20*time.Millisecond, "the watch listens")
	l.ip("-n", l.hostName, "link", "del", "gleg")
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the watch still runs on a leg that is gone")
	}
}
