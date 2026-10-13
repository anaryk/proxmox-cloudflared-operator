//go:build e2e

package e2e

import (
	"encoding/json"
	"maps"
	"path/filepath"
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

// physicalPort gives a bridge of the run a port that stands for the NIC of a
// node: Proxmox makes the VLAN bridge of a tagged guest, <bridge>v<tag>,
// from the bridge's physical ports (a NIC, a bond or a VLAN device, never a
// veth), and refuses a bridge without one. A bond without slaves is one,
// and leads nowhere. The port goes with the bridge at the end.
func (s *suite) physicalPort(t testing.TB, bridge string) {
	t.Helper()
	require.True(t, s.bridges[bridge], "the suite did not make %s", bridge)
	port := bridgePort(bridge)
	if _, err := s.run("ip", "link", "show", "dev", port); err == nil {
		return
	}
	s.must(t, "ip", "link", "add", port, "type", "bond")
	s.must(t, "ip", "link", "set", "dev", port, "master", bridge, "up")
}

func bridgePort(bridge string) string { return bridge + "p0" }

func (s *suite) recordBridges(t testing.TB) {
	t.Helper()
	names := slices.Sorted(maps.Keys(s.bridges))
	marker(t, bridgesFile, strings.Join(names, "\n")+"\n")
}

// removeBridges deletes the bridges the run made. It runs after the guests
// are destroyed and pco is uninstalled, so that nothing is enslaved to them
// any more. A bridge Proxmox made for a tagged guest on one of them,
// <bridge>v<tag>, stays after the guest, and goes first; the port that
// stood for a NIC goes with its VLAN devices.
func (s *suite) removeBridges(t *testing.T) {
	failed := false
	for name := range s.bridges {
		vlanBridges, _ := filepath.Glob("/sys/class/net/" + name + "v[0-9]*")
		for _, dir := range vlanBridges {
			if _, err := s.run("ip", "link", "del", filepath.Base(dir)); err != nil {
				t.Errorf("removing the VLAN bridge %s: %v", filepath.Base(dir), err)
				failed = true
			}
		}
		gone := true
		for _, dev := range []string{bridgePort(name), name} {
			if _, err := s.run("ip", "link", "show", "dev", dev); err != nil {
				continue
			}
			if _, err := s.run("ip", "link", "del", dev); err != nil {
				t.Errorf("removing %s: %v", dev, err)
				failed, gone = true, false
			}
		}
		if gone {
			delete(s.bridges, name)
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
		clear(s.sysctls)
		unmark(t, sysctlFile)
	}
}

// requireFirewallService makes sure pve-firewall runs, as it does on a node
// with the firewall in use: with it bridged frames pass the host's conntrack
// (bridge-nf-call-iptables), which is what the SNAT port blocks of the legs
// are for. A service the suite started is stopped at the end.
func (s *suite) requireFirewallService(t testing.TB) {
	t.Helper()
	s.requireService(t, "pve-firewall")
}

// requireStatusService makes sure pvestatd runs: it is what tells the
// cluster's resource listing whether a guest runs, and without it the daemon
// sees every guest it has not seen running as of unknown state, and serves
// none of them. A test node may have it stopped; a node in use never has.
func (s *suite) requireStatusService(t testing.TB) {
	t.Helper()
	s.requireService(t, "pvestatd")
}

// requireService starts a service of the node for the run when it is not
// running, and records it under markerDir as <name>-started, so that it is
// stopped again at the end, by the suite or by cleanup.sh.
func (s *suite) requireService(t testing.TB, name string) {
	t.Helper()
	if active, _ := s.run("systemctl", "is-active", name); strings.TrimSpace(active) == "active" {
		return
	}
	if s.started == nil {
		s.started = map[string]bool{}
	}
	if !s.started[name] {
		marker(t, serviceMarker(name), "")
		s.started[name] = true
	}
	s.must(t, "systemctl", "start", name)
	t.Logf("started %s for the run", name)
}

func serviceMarker(name string) string { return markerDir + "/" + name + "-started" }

// stopFirewallService stops pve-firewall when the suite started it.
func (s *suite) stopFirewallService(t *testing.T) { s.stopService(t, "pve-firewall") }

// stopServices stops the services the suite started for the run.
func (s *suite) stopServices(t *testing.T) {
	for name := range s.started {
		s.stopService(t, name)
	}
}

// stopService stops a service the suite started for the run, and forgets it.
func (s *suite) stopService(t *testing.T, name string) {
	if !s.started[name] {
		return
	}
	if _, err := s.run("systemctl", "stop", name); err != nil {
		t.Errorf("stopping %s again: %v", name, err)
		return
	}
	delete(s.started, name)
	unmark(t, serviceMarker(name))
}

// registeredTags reads the tags only an admin may set, as the node has them:
// a string of tags separated by ; or , on Proxmox VE 8, a list on 9.
func (s *suite) registeredTags(t testing.TB) []string {
	t.Helper()
	out := s.must(t, "pvesh", "get", "/cluster/options", "--output-format", "json")
	var opts struct {
		Tags json.RawMessage `json:"registered-tags"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &opts), out)
	if len(opts.Tags) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(opts.Tags, &list); err == nil {
		return list
	}
	var joined string
	require.NoError(t, json.Unmarshal(opts.Tags, &joined), "registered-tags is neither a list nor a string: %s", opts.Tags)
	return strings.FieldsFunc(joined, func(r rune) bool { return r == ';' || r == ',' })
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
	s.tagsRecorded = false
	unmark(t, tagsFile)
}
