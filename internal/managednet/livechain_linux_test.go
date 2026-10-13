package managednet

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
)

// The test loads the chains pve-firewall would make for pco's rules into a
// network namespace of its own and reads them back through iptables-save, so
// that nothing of the node it runs on is touched.
func TestLinuxReadLiveChainsReadsTheKernelsRules(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: iptables in a namespace of its own")
	}
	if _, err := os.Stat(iptablesSave); err != nil {
		t.Skip("needs iptables-save at " + iptablesSave)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	require.NoError(t, err)
	defer func() { require.NoError(t, netns.Set(orig)); _ = orig.Close() }()
	ns, err := netns.New()
	require.NoError(t, err)
	defer func() { _ = ns.Close() }()

	iptables := func(args ...string) {
		out, err := exec.Command("iptables", args...).CombinedOutput()
		require.NoError(t, err, "iptables %s: %s", strings.Join(args, " "), out)
	}
	for _, chain := range []string{"PVEFW-SET-ACCEPT-MARK", "tap9399i1-IN", "tap9399i1-OUT"} {
		iptables("-N", chain)
	}
	iptables("-A", "tap9399i1-IN", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
	iptables("-A", "tap9399i1-IN", "-s", "10.203.118.1", "-p", "udp", "--sport", "67", "--dport", "68", "-j", "ACCEPT")
	iptables("-A", "tap9399i1-IN", "-s", "10.203.118.1", "-p", "tcp", "--dport", "443", "-j", "ACCEPT")
	iptables("-A", "tap9399i1-IN", "-j", "DROP")
	iptables("-A", "tap9399i1-IN", "-m", "comment", "--comment", "PVESIG:test signature", "-j", "ACCEPT")
	iptables("-A", "tap9399i1-OUT", "-s", "10.203.118.2", "-d", "10.203.118.1", "-g", "PVEFW-SET-ACCEPT-MARK")
	iptables("-A", "tap9399i1-OUT", "-p", "udp", "--sport", "68", "--dport", "67", "-g", "PVEFW-SET-ACCEPT-MARK")
	iptables("-A", "tap9399i1-OUT", "-j", "DROP")

	chains, err := ReadLiveChains(t.Context())
	require.NoError(t, err)
	require.Empty(t, LiveDrift(chains, "tap9399i1", WantRules(liveCard, "pco:test")))

	iptables("-D", "tap9399i1-IN", "-j", "DROP")
	chains, err = ReadLiveChains(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"the chain tap9399i1-IN lacks pco's rule IN DROP"}, LiveDrift(chains, "tap9399i1", WantRules(liveCard, "pco:test")))
}
