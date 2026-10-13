//go:build e2e

package e2e

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The networking scenarios reach guests on bridges the node has no address
// on. Those bridges are made for the run with ip link, never written to
// /etc/network/interfaces, and removed at the end; cleanup.sh removes the
// ones a run that was cut off left, from the file that lists them.
const (
	// isolatedBridge and secondBridge carry guests with the same prefix,
	// 10.50.0.0/24, reached through a leg each.
	isolatedBridge = "pcot1"
	secondBridge   = "pcot2"
	// vlanBridge filters VLANs; its guests are tagged.
	vlanBridge = "pcotv"
	// managedBridge is where the guests that need the managed network sit:
	// the node has no address there, so only a card on pconet reaches them.
	managedBridge = "pcoti"

	legPrefix     = "10.50.0.0/24"
	legAddr       = "10.50.0.250/24"
	vlanLegAddr   = "10.60.0.250/24"
	taggedLegAddr = "10.70.0.250/24"
	vlanTag       = 20
	// taggedVLAN is a tag on a bridge that does not filter VLANs, which
	// Proxmox serves with a bridge of its own, <bridge>v<tag>, made when the
	// first tagged guest starts.
	taggedVLAN = 30

	bridgesFile   = markerDir + "/bridges"
	sysctlFile    = markerDir + "/sysctl"
	tagsFile      = markerDir + "/registered-tags"
	firewallFile  = markerDir + "/pve-firewall-started"
	managedMarker = markerDir + "/managed"
)

// bridgeUp makes a runtime bridge, unless the run made it already, and
// records it. vlanAware makes it filter VLANs. A bridge of that name that the
// run did not make is refused: the suite removes what it lists at the end.
func (s *suite) bridgeUp(t testing.TB, name string, vlanAware bool) {
	t.Helper()
	if s.bridges[name] {
		return
	}
	if _, err := s.run("ip", "link", "show", "dev", name); err == nil {
		t.Fatalf("the bridge %s exists, and the suite did not make it: it makes %s for the run and removes it; %s",
			name, name, cleanupHint)
	}
	args := []string{"link", "add", name, "type", "bridge"}
	if vlanAware {
		args = append(args, "vlan_filtering", "1")
	}
	s.must(t, "ip", args...)
	s.bridges[name] = true
	s.recordBridges(t)
	// Without this the bridge gets a link-local address, which is nothing
	// a guest there should see from the node.
	_, _ = s.run("sysctl", "-qw", "net.ipv6.conf."+name+".disable_ipv6=1")
	s.must(t, "ip", "link", "set", "dev", name, "up")
}

func (s *suite) recordBridges(t testing.TB) {
	t.Helper()
	names := slices.Sorted(maps.Keys(s.bridges))
	marker(t, bridgesFile, strings.Join(names, "\n")+"\n")
}

// removeBridges deletes the bridges the run made. It runs after the guests
// are destroyed and pco is uninstalled, so that nothing is enslaved to them
// any more; a Proxmox VLAN bridge made for a tagged guest goes with its
// parent.
func (s *suite) removeBridges(t *testing.T) {
	failed := false
	for name := range s.bridges {
		if _, err := s.run("ip", "link", "show", "dev", name); err != nil {
			continue
		}
		if _, err := s.run("ip", "link", "del", name); err != nil {
			t.Errorf("removing the bridge %s: %v", name, err)
			failed = true
		}
	}
	if !failed {
		unmark(t, bridgesFile)
	}
}

// setSysctl sets a kernel parameter for the run and records its value before,
// so that it is put back at the end; the first value recorded for a name is
// the one put back.
func (s *suite) setSysctl(t testing.TB, name, value string) {
	t.Helper()
	if _, recorded := s.sysctls[name]; !recorded {
		before := strings.TrimSpace(s.must(t, "sysctl", "-n", name))
		s.sysctls[name] = before
		var lines []string
		for k, v := range s.sysctls {
			lines = append(lines, k+"="+v)
		}
		slices.Sort(lines)
		marker(t, sysctlFile, strings.Join(lines, "\n")+"\n")
	}
	s.must(t, "sysctl", "-qw", name+"="+value)
}

func (s *suite) restoreSysctls(t *testing.T) {
	failed := false
	for name, value := range s.sysctls {
		if _, err := s.run("sysctl", "-qw", name+"="+value); err != nil {
			t.Errorf("putting %s back to %s: %v", name, value, err)
			failed = true
		}
	}
	if !failed {
		unmark(t, sysctlFile)
	}
}

// requireFirewallService makes sure pve-firewall runs, as it does on a node
// with the firewall in use: with it bridged frames pass the host's conntrack
// (bridge-nf-call-iptables), which is what the SNAT port blocks of the legs
// are for. A service the suite started is stopped at the end.
func (s *suite) requireFirewallService(t testing.TB) {
	t.Helper()
	if active, _ := s.run("systemctl", "is-active", "pve-firewall"); strings.TrimSpace(active) == "active" {
		return
	}
	if !s.firewallStarted {
		marker(t, firewallFile, "")
		s.firewallStarted = true
	}
	s.must(t, "systemctl", "start", "pve-firewall")
	t.Log("started pve-firewall for the run")
}

func (s *suite) stopFirewallService(t *testing.T) {
	if !s.firewallStarted {
		return
	}
	if _, err := s.run("systemctl", "stop", "pve-firewall"); err != nil {
		t.Errorf("stopping pve-firewall again: %v", err)
		return
	}
	unmark(t, firewallFile)
}

// registeredTags reads the tags only an admin may set, as the node has them.
func (s *suite) registeredTags(t testing.TB) []string {
	t.Helper()
	out := s.must(t, "pvesh", "get", "/cluster/options", "--output-format", "json")
	var opts struct {
		Tags string `json:"registered-tags"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &opts), out)
	return strings.FieldsFunc(opts.Tags, func(r rune) bool { return r == ';' || r == ',' })
}

// registerTag adds a tag to the registered tags, and records what they were
// before the run first changed them, which is put back before pco is
// uninstalled: uninstall takes away the tag setup registered.
func (s *suite) registerTag(t testing.TB, tag string) {
	t.Helper()
	tags := s.registeredTags(t)
	if !s.tagsRecorded {
		marker(t, tagsFile, strings.Join(tags, ";")+"\n")
		s.tagsBefore, s.tagsRecorded = tags, true
	}
	if slices.Contains(tags, tag) {
		return
	}
	s.setRegisteredTags(t, append(tags, tag))
}

func (s *suite) setRegisteredTags(t testing.TB, tags []string) {
	t.Helper()
	if len(tags) == 0 {
		s.must(t, "pvesh", "set", "/cluster/options", "--delete", "registered-tags")
		return
	}
	s.must(t, "pvesh", "set", "/cluster/options", "--registered-tags", strings.Join(tags, ";"))
}

func (s *suite) restoreRegisteredTags(t *testing.T) {
	if !s.tagsRecorded {
		return
	}
	var err error
	if len(s.tagsBefore) == 0 {
		_, err = s.run("pvesh", "set", "/cluster/options", "--delete", "registered-tags")
	} else {
		_, err = s.run("pvesh", "set", "/cluster/options", "--registered-tags", strings.Join(s.tagsBefore, ";"))
	}
	if err != nil {
		t.Errorf("putting the registered tags back to %v: %v", s.tagsBefore, err)
		return
	}
	unmark(t, tagsFile)
}
