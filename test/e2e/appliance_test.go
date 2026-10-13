//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The appliance's run: its install and the first segment, the scenarios of
// the host at the observed level, then what only the appliance has, the
// networking scenarios once its restores are done, and the uninstall last.
var applianceScenarios = []scenario{
	{"A-S1 install", testApplianceInstall},
	{"A-S2 cloudflare through the relay", testApplianceCloud},
	{"A-S3 a segment and the first pin", testFirstPin},
	s11, s1, s2, s3, s5, s6, s7, s8, s10, s12, s13, s14, s15, s9,
	{"A-S4 mac change and renumber", testMACChangeAndRenumber},
	{"A-S5 a delegated guest", testDelegatedGuest},
	{"A-S6 soft deny", testSoftDeny},
	{"A-S13 a second segment", testSecondSegment},
	{"A-S14 a restarted nftables.service", testNftablesRestarted},
	{"A-S12 doctor", testApplianceDoctor},
	{"A-S10 a tenant with the appliance's mac", testTenant},
	{"A-S11 reboot and restart", testRebootAndRestart},
	{"A-S7 a clone from a snapshot", testCloneFromSnapshot},
	{"A-S8 the clone with the mac set back", testCloneWithMAC},
	{"A-S16 rollback after a reboot", testRollback},
	{"A-S17 upgrade and rollback of the package", testUpgrade},
	{"A-S9 restore", testRestore},
	{"S17 attach an isolated bridge, in the appliance", testApplianceAttach},
	{"S19 two networks with one subnet, in the appliance", testApplianceIdenticalSubnets},
	{"S21 managed network, in the appliance", testApplianceManaged},
	{"S22 managed network isolation, in the appliance", testApplianceIsolation},
	{"A-S15 uninstall", testApplianceUninstall},
}

// pveFlag is a flag as pveum prints it, 0 or 1, or false or true.
type pveFlag bool

func (f *pveFlag) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "1", "true":
		*f = true
	case "0", "false", "null", "":
		*f = false
	default:
		return fmt.Errorf("%s is no flag", b)
	}
	return nil
}

func testApplianceInstall(t *testing.T, s *suite) {
	a := s.app
	id := strconv.Itoa(a.vmid)
	marker := "pco appliance vm" + id

	t.Log("the container, as the installer configured it")
	cfg := s.pctConfig(t, a.vmid)
	t.Logf("pct config %s: %v", id, cfg)
	for key, want := range map[string]string{
		"unprivileged": "1", "features": "nesting=1", "onboot": "1", "startup": "order=1,up=20",
		"protection": "1", "tags": "pco-appliance;pco-combined", "hostname": "pco",
	} {
		require.Equal(t, want, cfg[key], key)
	}
	require.NotContains(t, cfg, "cmode")
	require.True(t, strings.HasPrefix(cfg["description"], marker+", installed "), cfg["description"])
	mp0 := cfg["mp0"]
	require.True(t, strings.HasPrefix(mp0, s.disk+":"), mp0)
	for _, part := range []string{"-" + id + "-disk-", "mp=/var/lib/pco", "backup=0"} {
		require.Contains(t, mp0, part)
	}
	net0 := cfg["net0"]
	if a.real {
		require.Contains(t, net0, "bridge="+a.apiBridge)
		require.Contains(t, net0, "ip=dhcp")
	} else {
		for _, part := range []string{"name=eth0", "bridge=" + bridge, "ip=" + s.lay.applianceAddr + "/24", "gw=" + s.lay.applianceGW} {
			require.Contains(t, net0, part)
		}
	}

	t.Log("the objects in Proxmox, each with its mark")
	var roles []struct {
		RoleID string `json:"roleid"`
		Privs  string `json:"privs"`
	}
	s.pveJSON(t, &roles, "pveum", "role", "list")
	i := slices.IndexFunc(roles, func(r struct {
		RoleID string `json:"roleid"`
		Privs  string `json:"privs"`
	}) bool {
		return r.RoleID == "PCO"
	})
	require.GreaterOrEqual(t, i, 0, "no role PCO")
	privs := strings.Split(roles[i].Privs, ",")
	want := setup.RolePrivileges(a.major)
	slices.Sort(privs)
	slices.Sort(want)
	require.Equal(t, want, privs)
	var users []struct {
		UserID  string `json:"userid"`
		Comment string `json:"comment"`
	}
	s.pveJSON(t, &users, "pveum", "user", "list")
	require.True(t, slices.ContainsFunc(users, func(u struct {
		UserID  string `json:"userid"`
		Comment string `json:"comment"`
	}) bool {
		return u.UserID == "pco@pve" && u.Comment == "pco operator"
	}), "%+v", users)
	var tokens []struct {
		TokenID string  `json:"tokenid"`
		Privsep pveFlag `json:"privsep"`
		Comment string  `json:"comment"`
	}
	s.pveJSON(t, &tokens, "pveum", "user", "token", "list", "pco@pve")
	require.Len(t, tokens, 1, "%+v", tokens)
	require.Equal(t, "vm"+id, tokens[0].TokenID)
	require.True(t, bool(tokens[0].Privsep))
	require.Equal(t, marker, tokens[0].Comment)
	lines := s.acls(t)
	for _, who := range []acl{{Type: "user", UGID: "pco@pve"}, {Type: "token", UGID: "pco@pve!vm" + id}} {
		require.True(t, slices.ContainsFunc(lines, func(l acl) bool {
			return l.Path == "/" && l.RoleID == "PCO" && l.Type == who.Type && l.UGID == who.UGID
		}), "no line for %s on /: %+v", who.UGID, lines)
	}
	var pool struct {
		Comment string `json:"comment"`
		Members []struct {
			VMID int `json:"vmid"`
		} `json:"members"`
	}
	s.pveJSON(t, &pool, "pvesh", "get", "/pools/"+applianceinstallPool)
	require.Equal(t, "pco appliances", pool.Comment)
	require.Len(t, pool.Members, 1)
	require.Equal(t, a.vmid, pool.Members[0].VMID)
	require.Contains(t, s.registeredTags(t), gateTag)

	t.Log("the node, as the installer found it")
	s.requireNodeAsBefore(t, false)

	t.Log("inside the appliance")
	require.Equal(t, "appliance", strings.TrimSpace(s.hereMust(t, "cat", "/etc/pco/profile")))
	s.hereMust(t, "test", "-f", stateRoot+"/.volume")
	source := strings.TrimSpace(s.hereMust(t, "findmnt", "-n", "-o", "SOURCE", stateRoot))
	t.Logf("the source of %s: %s", stateRoot, source)
	require.Contains(t, source, "-"+id+"-disk-")
	_, err := s.here("test", "-e", stateRoot+"/bootstrap.json")
	require.Error(t, err, "the bootstrap is still there")
	s.hereMust(t, "test", "-s", stateRoot+"/pve-ca.pem")
	inst := s.applianceInstall(t)
	require.Equal(t, store.ProfileAppliance, inst.Profile)
	require.NotNil(t, inst.Appliance)
	require.Equal(t, a.vmid, inst.Appliance.VMID)
	require.Equal(t, a.node, inst.Appliance.Node)
	require.Contains(t, inst.Appliance.MACs, s.cardMAC(t, a.vmid, "net0"))
	require.NotEmpty(t, inst.Appliance.Endpoints)
	require.Equal(t, a.node, inst.Appliance.Endpoints[0].ServerName)
	if !a.real {
		require.Equal(t, s.lay.nodeAddr+":8006", inst.Appliance.Endpoints[0].Address)
	}
	settings := s.applianceSettings(t)
	require.Equal(t, "observed", settings.IdentityMinimum)
	require.True(t, settings.ObserveOnly)
	require.Contains(t, s.applianceNodeAddrs(t), s.lay.nodeAddr)
	status, _ := s.pcoCombined("status")
	t.Logf("pco status:\n%s", status)
	require.Regexp(t, `(?m)^Profile:\s+appliance$`, status)
	require.Regexp(t, `(?m)^Identity:\s+ok \(lxc/`+id+` on `+regexp.QuoteMeta(a.node)+`\)$`, status)
	masked, _ := s.here("systemctl", "is-enabled", "nftables.service")
	require.Equal(t, "masked", strings.TrimSpace(masked))
	net, err := s.pcoCombined("net", "show")
	t.Logf("pco net show:\n%s", net)
	require.NoError(t, err)
	require.Contains(t, net, "Rejected since the table was loaded: 0 packets")
	units := s.hereMust(t, "systemctl", "list-units", "--all", "--plain", "--no-legend", "--no-pager", "pco-cloudflared@*")
	require.Empty(t, strings.TrimSpace(units), "a connector unit before apply")
}

func testApplianceCloud(t *testing.T, s *suite) {
	if s.fake == nil {
		t.Log("against the real API: neither the relay nor the drop-in")
	} else {
		active, _ := s.here("systemctl", "is-active", relayUnit)
		require.Equal(t, "active", strings.TrimSpace(active))
		enabled, _ := s.here("systemctl", "is-enabled", relayUnit)
		require.Equal(t, "enabled", strings.TrimSpace(enabled))
		require.Contains(t, s.hereMust(t, "cat", pcoDropIn), overrideOf+"="+relayURL)
	}
	_, err := s.here("test", "-e", tokenInside)
	require.Error(t, err, "the token file is still in the appliance")
	st := s.waitState(t, "a complete cycle with the credential", 2*time.Minute, func(st engine.State) bool { return st.Complete })
	require.Len(t, st.Credentials, 1)
	s.requireOverrideLine(t, st)
	rest := slices.DeleteFunc(otherProblems(st), func(p string) bool { return strings.Contains(p, "observe") })
	require.Empty(t, rest, "problems besides the override and observe-only")
}

// segments is what pco segment list --json prints.
func (s *suite) segments(t testing.TB) []engine.SegmentView {
	t.Helper()
	var out []engine.SegmentView
	raw := s.pcoMust(t, "segment", "list", "--json")
	require.NoError(t, json.Unmarshal([]byte(raw), &out), raw)
	return out
}

// waitHeld waits until the route of host by owner is not active and its
// reason has text.
func (s *suite) waitHeld(t testing.TB, host, owner, text string) engine.RouteView {
	t.Helper()
	var found engine.RouteView
	s.waitState(t, fmt.Sprintf("route %s held with %q", host, text), 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, owner)
		found = r
		return ok && r.State != planner.StateActive && strings.Contains(strings.ToLower(r.Reason), strings.ToLower(text))
	})
	return found
}

// twoCycles is how long two cycles take at most, with the time to ask.
const twoCycles = 2*pollInterval + 20*time.Second

// waitActiveWithin waits until the route of host is active, for at most d.
func (s *suite) waitActiveWithin(t testing.TB, host string, d time.Duration) engine.RouteView {
	t.Helper()
	var found engine.RouteView
	s.waitState(t, "route "+host+" active", d, func(st engine.State) bool {
		r, ok := routeOf(st, host, "")
		found = r
		return ok && r.State == planner.StateActive
	})
	return found
}

// fakeWrites are the writes the fake saw from the index from on; none
// against the real API, which the suite cannot watch.
func (s *suite) fakeWrites(from int) []string {
	if s.fake == nil {
		return nil
	}
	return s.fake.writes(from)
}

func testFirstPin(t *testing.T, s *suite) {
	from := s.calls()
	since := time.Now()
	g := s.g(20, guest{notes: "cf-tunnel: e2e-as3.ZONE -> :8080"})
	s.create(t, g)
	host := s.host("as3")
	r := s.waitHeld(t, host, g.owner(), "segment "+bridge+" is not acknowledged; pco segment acknowledge "+bridge)
	t.Logf("before the acknowledgement: %s (%s)", r.State, r.Reason)
	segs := s.segments(t)
	i := slices.IndexFunc(segs, func(v engine.SegmentView) bool { return v.Bridge == bridge && v.VLAN == 0 })
	require.GreaterOrEqual(t, i, 0, "%+v", segs)
	require.False(t, segs[i].Acknowledged)
	require.Equal(t, 1, segs[i].Routes)

	out := s.pcoMust(t, "segment", "acknowledge", bridge, "--yes")
	t.Logf("pco segment acknowledge:\n%s", out)
	s.sync(t)
	route := s.waitActiveWithin(t, host, twoCycles)
	require.Equal(t, "observed", route.Level)
	s.waitEvent(t, since, "claim", "pinned MAC "+s.cardMAC(t, g.vmid, "net0")+" for "+host)
	require.Empty(t, s.fakeWrites(from), "an install that observes wrote at Cloudflare")
}

// unapprovedOf is the entry of an owner among the guests that wait for an
// approval.
func unapprovedOf(st engine.State, owner string) (engine.UnapprovedGuest, bool) {
	i := slices.IndexFunc(st.Unapproved, func(u engine.UnapprovedGuest) bool { return u.String() == owner })
	if i < 0 {
		return engine.UnapprovedGuest{}, false
	}
	return st.Unapproved[i], true
}

// waitsFor says whether an owner waits for an approval with a reason that has
// text.
func waitsFor(st engine.State, owner, text string) bool {
	u, ok := unapprovedOf(st, owner)
	return ok && slices.ContainsFunc(u.Why, func(w string) bool { return strings.Contains(strings.ToLower(w), strings.ToLower(text)) })
}

func testMACChangeAndRenumber(t *testing.T, s *suite) {
	g := s.g(21, guest{notes: "cf-tunnel: e2e-as4.ZONE -> :8080"})
	other := s.g(22, guest{tags: "e2e"})
	s.create(t, g)
	s.create(t, other)
	s.enforce(t)
	host := s.host("as4")
	s.waitOwnerRoute(t, host, g.owner(), planner.StateActive)
	own, theirs := s.cardMAC(t, g.vmid, "net0"), s.cardMAC(t, other.vmid, "net0")
	id := strconv.Itoa(g.vmid)
	setCard := func(mac, addr string) {
		s.stop(t, g.vmid)
		s.must(t, "pct", "set", id, "--net0", "name=eth0,bridge="+bridge+",hwaddr="+mac+",ip="+addr)
	}

	t.Log("(a) the other guest's MAC and its address in the notes: held until the admin approves")
	setCard(theirs, g.addr)
	s.setNotes(t, g.vmid, "cf-tunnel: "+host+" -> http://"+other.ip()+":8080")
	s.start(t, g.vmid)
	st := s.waitState(t, "the route held for the changed MAC", 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, g.owner())
		return ok && r.State != planner.StateActive && waitsFor(st, g.owner(), "MAC changed from "+own+" to "+theirs)
	})
	u, _ := unapprovedOf(st, g.owner())
	t.Logf("%s waits: %q", g.owner(), u.Why)
	s.requireNoTarget(t, other.ip()+":8080")
	if tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install)); found {
		for _, r := range s.cloud.Ingress(t, tn.ID) {
			require.NotContains(t, r.Service, other.ip(), "%+v", r)
		}
	}
	out := s.pcoMust(t, "guest", "approve", g.owner())
	t.Logf("pco guest approve:\n%s", out)
	route := s.waitOwnerRoute(t, host, g.owner(), planner.StateActive)
	require.Equal(t, "http://"+other.ip()+":8080", route.Service)

	t.Log("(b) its own MAC again, then another address under the same MAC: served without an approval")
	setCard(own, g.addr)
	s.setNotes(t, g.vmid, g.notes)
	s.start(t, g.vmid)
	// The approval pinned the other guest's MAC: going back is a change of
	// MAC as well, which the admin approves as before.
	st = s.waitState(t, "the route back on its own MAC", 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, g.owner())
		_, waits := unapprovedOf(st, g.owner())
		return ok && (r.State == planner.StateActive || waits)
	})
	if _, waits := unapprovedOf(st, g.owner()); waits {
		t.Logf("back on its own MAC it waits again: %s", s.pcoMust(t, "guest", "approve", g.owner()))
	}
	route = s.waitOwnerRoute(t, host, g.owner(), planner.StateActive)
	require.Equal(t, "http://"+g.ip()+":8080", route.Service)
	renumbered := time.Now()
	moved := s.lay.addr(31)
	setCard(own, moved+"/24")
	s.start(t, g.vmid)
	s.waitState(t, "the route at the new address", twoCycles+time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, g.owner())
		return ok && r.State == planner.StateActive && r.Service == "http://"+moved+":8080"
	})
	_, waits := unapprovedOf(s.state(t), g.owner())
	require.False(t, waits, "the renumbered guest waits for an approval")
	for _, ev := range s.events(t, renumbered) {
		require.NotContains(t, ev.Message, "MAC changed", "%+v", ev)
	}
	s.destroy(t, g.vmid)
	s.destroy(t, other.vmid)
	s.sync(t)
}

// waitDelegated waits until the guest waits as delegated, with the test user
// named, or no longer waits and is served.
func (s *suite) waitDelegated(t testing.TB, g guest, host string, delegated bool) {
	t.Helper()
	s.waitState(t, fmt.Sprintf("%s delegated: %v", g.owner(), delegated), 3*time.Minute, func(st engine.State) bool {
		if delegated {
			return waitsFor(st, g.owner(), testUser)
		}
		r, ok := routeOf(st, host, g.owner())
		_, waits := unapprovedOf(st, g.owner())
		return !waits && ok && r.State == planner.StateActive
	})
}

// noAccessCommands are the pveum lines the problem lines of an exposed
// appliance give, as an admin copies them.
var noAccessCommands = regexp.MustCompile(`pveum acl modify (\S+) (--users|--tokens) (\S+) --roles NoAccess`)

func exposedLines(st engine.State) []string {
	return slices.DeleteFunc(slices.Clone(st.Problems), func(p string) bool { return !strings.Contains(p, "pco serves nothing while it does") })
}

// waitExposed waits until the appliance serves nothing while exactly the
// principals given can reach into it, and returns the state.
func (s *suite) waitExposed(t testing.TB, principals ...string) engine.State {
	t.Helper()
	slices.Sort(principals)
	return s.waitState(t, fmt.Sprintf("the appliance failing closed for %v", principals), 3*time.Minute, func(st engine.State) bool {
		if st.Identity == nil {
			return false
		}
		got := slices.Sorted(slices.Values(st.Identity.Exposed))
		return slices.Equal(got, principals) && len(exposedLines(st)) == len(principals)
	})
}

// waitServing waits until no principal can reach into the appliance and it
// serves again: the flag back, targets in the egress set, S1 served.
func (s *suite) waitServing(t testing.TB) {
	t.Helper()
	s.waitState(t, "nobody exposed", 3*time.Minute, func(st engine.State) bool { return st.Identity != nil && len(st.Identity.Exposed) == 0 })
	s.waitActiveWithin(t, s.host("s1"), 3*time.Minute)
	require.Eventually(t, func() bool { return s.flagged() && len(s.targetsNow()) > 0 },
		2*time.Minute, 2*time.Second, "the flag and the egress set did not come back")
}

func testDelegatedGuest(t *testing.T, s *suite) {
	s.ensurePrincipals(t)
	s.ensureS1(t)
	g := s.g(23, guest{notes: "cf-tunnel: e2e-as5.ZONE -> :8080"})
	s.create(t, g)
	host := s.host("as5")
	s.waitOwnerRoute(t, host, g.owner(), planner.StateActive)
	id := strconv.Itoa(g.vmid)

	t.Log("(a) VM.Config.Network on the guest, then through a pool: the route waits while it is granted")
	vms := "/vms/" + id
	s.grant(t, vms, "--users", testUser, testRole)
	s.waitDelegated(t, g, host, true)
	s.ungrant(t, vms, testRole)
	s.waitDelegated(t, g, host, false)
	s.must(t, "pveum", "pool", "modify", testPool, "--vms", id)
	s.grant(t, "/pool/"+testPool, "--users", testUser, testRole)
	s.waitDelegated(t, g, host, true)
	s.ungrant(t, "/pool/"+testPool, testRole)
	s.waitDelegated(t, g, host, false)
	s.must(t, "pveum", "pool", "modify", testPool, "--vms", id, "--delete", "1")
	s.grant(t, vms, "--users", testUser, "PVEVMUser")
	for range 3 {
		st := s.cycles(t, 1)
		if waitsFor(st, g.owner(), testUser) {
			t.Logf("PVEVMUser on this node holds VM.Config.Network: %s waits", g.owner())
			break
		}
		r, ok := routeOf(st, host, g.owner())
		require.True(t, ok && r.State == planner.StateActive, "%+v", r)
	}
	s.ungrant(t, vms, "PVEVMUser")
	s.waitDelegated(t, g, host, false)

	t.Log("(b) a principal that can reach into the appliance: it serves nothing until the printed line ran")
	self := "/vms/" + strconv.Itoa(s.app.vmid)
	s.grant(t, self, "--users", testUser, "PVEVMUser")
	st := s.waitExposed(t, testUser)
	line := "pveum acl modify " + self + " --users " + testUser + " --roles NoAccess"
	require.True(t, slices.ContainsFunc(exposedLines(st), func(p string) bool { return strings.HasSuffix(p, line) }), "%q", exposedLines(st))
	require.NotEmpty(t, st.Hold, "the writes are not held")
	require.Eventually(t, func() bool { return !s.flagged() }, time.Minute, 2*time.Second, "the flag stayed")
	s.requireServesNothing(t)
	s.grant(t, self, "--users", testUser, "NoAccess")
	s.waitServing(t)
	s.ungrant(t, self, "NoAccess")
	s.ungrant(t, self, "PVEVMUser")

	t.Log("tokens: one without privilege separation is answered on its user, one with it on itself")
	for _, tok := range []struct{ name, privsep string }{{"np", "0"}, {"ps", "1"}} {
		// The token's secret is printed once; the run keeps none.
		_, err := s.run("pveum", "user", "token", "add", testUser, tok.name, "--privsep", tok.privsep, "--comment", "pco end-to-end suite")
		require.NoError(t, err)
	}
	np, ps := testUser+"!np", testUser+"!ps"
	s.grant(t, self, "--users", testUser, "PVEVMUser")
	s.grant(t, self, "--tokens", ps, "PVEVMUser")
	st = s.waitExposed(t, testUser, np, ps)
	lines := exposedLines(st)
	t.Logf("the lines: %q", lines)
	require.True(t, slices.ContainsFunc(lines, func(p string) bool {
		return strings.HasPrefix(p, np+" ") &&
			strings.Contains(p, np+" is not privilege-separated: it holds the roles of "+testUser) &&
			strings.HasSuffix(p, "--users "+testUser+" --roles NoAccess")
	}), "no line for %s", np)
	require.True(t, slices.ContainsFunc(lines, func(p string) bool {
		return strings.HasPrefix(p, ps+" ") && strings.HasSuffix(p, "--tokens "+ps+" --roles NoAccess")
	}), "no line for %s", ps)
	seen := map[string]bool{}
	for _, p := range lines {
		for _, m := range noAccessCommands.FindAllStringSubmatch(p, -1) {
			if !seen[m[0]] {
				seen[m[0]] = true
				t.Logf("running %s", m[0])
				s.grant(t, m[1], m[2], m[3], "NoAccess")
			}
		}
	}
	s.waitServing(t)
	s.removeGrants(t)
	for _, tok := range []string{"np", "ps"} {
		s.must(t, "pveum", "user", "token", "remove", testUser, tok)
	}
	s.destroy(t, g.vmid)
	s.sync(t)
}

func testSoftDeny(t *testing.T, s *suite) {
	if s.app.real {
		t.Skip("against the real API the appliance's gateway is its uplink's, which DHCP gives it")
	}
	g := s.g(49, guest{notes: "cf-tunnel: e2e-as6.ZONE -> :8080"})
	require.Equal(t, s.lay.applianceGW, g.ip())
	s.create(t, g)
	s.enforce(t)
	host := s.host("as6")
	why := "address " + s.lay.applianceGW + " is the gateway of the appliance"
	s.waitState(t, "the gateway's route held", 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, g.owner())
		return ok && r.State != planner.StateActive && waitsFor(st, g.owner(), why)
	})
	out := s.pcoMust(t, "guest", "approve", g.owner(), "--allow-address", s.lay.applianceGW)
	t.Logf("pco guest approve:\n%s", out)
	s.waitOwnerRoute(t, host, g.owner(), planner.StateActive)
	s.destroy(t, g.vmid)
	s.sync(t)
}

func testSecondSegment(t *testing.T, s *suite) {
	a := s.app
	s.ensureS1(t)
	s.bridgeUp(t, segBridge, false)
	iface := "eth" + strings.TrimPrefix(a.segNet, "net")
	added := time.Now()
	s.must(t, "pct", "set", strconv.Itoa(a.vmid), "--"+a.segNet, "name="+iface+",bridge="+segBridge+",ip="+segApplianceIP+"/24")
	pending := "a NIC of the appliance lxc/" + strconv.Itoa(a.vmid) + " appeared or vanished"
	s.waitState(t, "the new card taken", 2*time.Minute, func(st engine.State) bool {
		require.NotContains(t, st.Hold, "NIC", "the new card holds the writes")
		return st.At.After(added) && st.Identity != nil && st.Identity.OK &&
			!slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, pending) })
	})
	took := time.Since(added)
	t.Logf("the new card was taken after %s", took.Round(time.Second))
	require.LessOrEqual(t, took, time.Minute+twoCycles)

	g := s.in(guest{vmid: s.lay.vmid(25), bridge: segBridge, addr: segNet + ".175/24", notes: "cf-tunnel: e2e-as13.ZONE -> :8080"})
	s.create(t, g)
	host := s.host("as13")
	s.waitHeld(t, host, g.owner(), "segment "+segBridge+" is not acknowledged")
	t.Logf("pco segment acknowledge:\n%s", s.pcoMust(t, "segment", "acknowledge", segBridge, "--yes"))
	s.sync(t)
	s.waitActiveWithin(t, host, twoCycles)

	removed := time.Now()
	s.must(t, "pct", "set", strconv.Itoa(a.vmid), "--delete", a.segNet)
	s.sync(t)
	s.waitState(t, "the route without its segment, the identity ok", time.Minute+twoCycles, func(st engine.State) bool {
		r, ok := routeOf(st, host, g.owner())
		return st.At.After(removed) && ok && r.State != planner.StateActive && st.Identity != nil && st.Identity.OK &&
			!slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, pending) })
	})
	s.destroy(t, g.vmid)
	s.sync(t)
}

// pcoTables are pco's tables in the appliance.
var pcoTables = []string{"inet pco_egress", "inet pco_net"}

// tablesHere lists the tables of the ruleset where pco runs.
func (s *suite) tablesHere(t testing.TB) []string {
	t.Helper()
	var out []string
	for line := range strings.Lines(s.hereMust(t, "nft", "list", "tables")) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "table "); ok {
			out = append(out, rest)
		}
	}
	slices.Sort(out)
	return out
}

// tableContent is a table where pco runs, without the values of its
// counters.
func (s *suite) tableContent(t testing.TB, table string) string {
	t.Helper()
	out, err := s.here("nft", append([]string{"list", "table"}, strings.Fields(table)...)...)
	if err != nil {
		return ""
	}
	return counterValuesText.ReplaceAllString(out, "packets 0 bytes 0")
}

// requireTablesBack waits for the events of both of pco's tables loaded
// again since a time, and for both to be as they were.
func (s *suite) requireTablesBack(t testing.TB, since time.Time, before map[string]string) {
	t.Helper()
	s.waitEvent(t, since, "egress", "was loaded again")
	s.waitEvent(t, since, "net", "was loaded again")
	require.Eventually(t, func() bool {
		for table, content := range before {
			if s.tableContent(t, table) != content {
				return false
			}
		}
		return true
	}, 10*time.Second, 200*time.Millisecond, "pco's tables did not come back as they were")
	t.Logf("both tables back %s after %s", strings.Join(slices.Sorted(maps.Keys(before)), " and "), since.Format(time.TimeOnly))
}

// testApplianceRulesetFlushed is S14 in the appliance: the ruleset flushed
// inside, both of pco's tables back with their content, and no other table
// there to restore, as nftables.service is masked.
func (s *suite) testApplianceRulesetFlushed(t *testing.T) {
	tables := s.tablesHere(t)
	t.Logf("the tables of the appliance: %v", tables)
	require.Equal(t, pcoTables, tables, "the appliance has tables besides pco's")
	before := map[string]string{}
	for _, table := range pcoTables {
		before[table] = s.tableContent(t, table)
		require.NotEmpty(t, before[table], table)
	}
	flushed := time.Now()
	s.hereMust(t, "nft", "flush", "ruleset")
	t.Log("the ruleset of the appliance is flushed")
	s.requireTablesBack(t, flushed, before)
}

func testNftablesRestarted(t *testing.T, s *suite) {
	s.ensureS1(t)
	before := map[string]string{}
	for _, table := range pcoTables {
		before[table] = s.tableContent(t, table)
		require.NotEmpty(t, before[table], table)
	}
	t.Cleanup(func() {
		if _, err := s.here("systemctl", "mask", "--now", "nftables.service"); err != nil {
			t.Errorf("masking nftables.service again: %v", err)
		}
	})
	s.hereMust(t, "systemctl", "unmask", "nftables.service")
	restarted := time.Now()
	s.hereMust(t, "systemctl", "restart", "nftables.service")
	s.requireTablesBack(t, restarted, before)
	stopped := time.Now()
	s.hereMust(t, "systemctl", "stop", "nftables.service")
	s.requireTablesBack(t, stopped, before)
	s.hereMust(t, "systemctl", "mask", "nftables.service")
}

// doctorFindings runs pco doctor --json where pco runs.
func (s *suite) doctorFindings(t testing.TB) []doctor.Finding {
	t.Helper()
	// It exits 1 when a check fails; what it printed is what counts.
	out, _ := s.pcoOut("doctor", "--json")
	var findings []doctor.Finding
	require.NoError(t, json.Unmarshal([]byte(out), &findings), out)
	return findings
}

func findingOf(findings []doctor.Finding, check string) (doctor.Finding, bool) {
	i := slices.IndexFunc(findings, func(f doctor.Finding) bool { return f.Check == check })
	if i < 0 {
		return doctor.Finding{}, false
	}
	return findings[i], true
}

func testApplianceDoctor(t *testing.T, s *suite) {
	s.ensureS1(t)
	s.settle(t)
	findings := s.doctorFindings(t)
	var notOK []string
	for _, f := range findings {
		t.Logf("%-28s %-4s %s", f.Check, f.Level, f.Detail)
		if f.Level == doctor.LevelOK {
			continue
		}
		check := f.Check
		if strings.HasPrefix(check, "connector ") {
			// The check of a connector names its tunnel and account.
			check = "connector"
		}
		notOK = append(notOK, check)
	}
	slices.Sort(notOK)
	notOK = slices.Compact(notOK)
	// Without an uplink the appliance reaches neither Cloudflare nor a name
	// server nor a time server, and a connector with the fake's token is
	// never ready; the QUIC buffer is the node's default; the override of
	// the API is a standing problem.
	want := []string{"clock", "cloudflare api", "connector", "dns", "egress probe", "outbound", "problems", "quic buffer"}
	if s.fake == nil {
		want = []string{"quic buffer"}
	}
	require.Equal(t, want, notOK)
}

func testTenant(t *testing.T, s *suite) {
	a := s.app
	s.ensureS1(t)
	b := s.takeBefore(t)
	from := s.calls()
	since := time.Now()
	mac := s.applianceMAC(t)
	tenant := s.g(24, guest{mac: mac, notes: "cf-tunnel: e2e-as10.ZONE -> :8080"})
	s.create(t, tenant)
	host := s.host("as10")
	self := "lxc/" + strconv.Itoa(a.vmid)
	s.waitEvent(t, since, "identity", tenant.owner()+" is configured with a MAC of the appliance "+self)
	reason := "configured with the MAC " + mac + " of the appliance " + self
	s.waitState(t, "the tenant's route rejected", 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, tenant.owner())
		return ok && r.State == planner.StateRejected && strings.Contains(strings.ToLower(r.Reason), strings.ToLower(reason))
	})
	s1 := s.host("s1")
	for range 3 {
		st := s.cycles(t, 1)
		require.NotNil(t, st.Identity)
		require.True(t, st.Identity.OK, "the tenant made the appliance doubt itself: %s", st.Identity.Why)
		if r, _, ok := ruleOf(s.cloud.Ingress(t, b.tunnel), s1); ok {
			require.NotEqual(t, planner.BlockedService, r.Service, "the route of S1 was withdrawn")
		}
	}
	for _, w := range s.fakeWrites(from) {
		require.NotContains(t, w, host, "a write names the tenant's hostname")
	}
	s.destroy(t, tenant.vmid)
	s.sync(t)
	s.waitState(t, "every route of before active", 2*time.Minute, func(st engine.State) bool {
		active := activeRoutes(st)
		return !slices.ContainsFunc(b.active, func(r string) bool { return !slices.Contains(active, r) })
	})
}

// putsPerTunnel counts the configuration writes of each tunnel among writes,
// and returns the other writes.
func putsPerTunnel(writes []string) (map[string]int, []string) {
	puts := map[string]int{}
	var other []string
	for _, w := range writes {
		f := strings.Fields(w)
		if f[0] == "PutTunnelConfig" {
			puts[f[len(f)-1]]++
			continue
		}
		other = append(other, w)
	}
	return puts, other
}

func testRebootAndRestart(t *testing.T, s *suite) {
	a := s.app
	id := strconv.Itoa(a.vmid)
	s.ensureS1(t)
	s.settle(t)
	w, found := s.leader(t)
	require.True(t, found)
	tunnel := s.tunnel(t)
	from := s.calls()
	rebooted := time.Now()
	s.must(t, "pct", "reboot", id)
	s.waitAppliance(t, a.vmid)
	st := s.waitState(t, "the first complete cycle after the reboot", 3*time.Minute, func(st engine.State) bool {
		return st.At.After(rebooted) && st.Complete && !st.EpochDrawnAt.IsZero() && st.EpochDrawnAt.After(rebooted)
	})
	drawn := st.EpochDrawnAt

	t.Log("the order of the boot: the service prefix and the egress filter first, the connectors after the identity")
	out, err := s.here("journalctl", "-b", "-o", "short-unix", "--no-pager",
		"-u", "pco-net.service", "-u", "pco-egress.service", "-u", "pco-cloudflared@*")
	require.NoError(t, err)
	var netDone, egressDone, firstConnector float64
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		at, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		switch {
		case strings.Contains(line, "Finished pco-net.service") && netDone == 0:
			netDone = at
		case strings.Contains(line, "Finished pco-egress.service") && egressDone == 0:
			egressDone = at
		case strings.Contains(line, "Starting pco-cloudflared@") && firstConnector == 0:
			firstConnector = at
		}
	}
	t.Logf("pco-net done at %.3f, pco-egress at %.3f, the epoch drawn at %s, the first connector at %.3f",
		netDone, egressDone, drawn.Format(time.RFC3339Nano), firstConnector)
	require.NotZero(t, netDone, "pco-net.service did not finish:\n%s", out)
	require.NotZero(t, egressDone, "pco-egress.service did not finish:\n%s", out)
	if firstConnector != 0 {
		require.Less(t, netDone, firstConnector)
		require.Less(t, egressDone, firstConnector)
		require.GreaterOrEqual(t, firstConnector, float64(drawn.Truncate(time.Second).Unix()),
			"a connector started before the identity passed")
	}
	net, err := s.pcoCombined("net", "show")
	require.NoError(t, err, net)
	require.Contains(t, net, "Rejected since the table was loaded: 0 packets")
	after, _ := s.leader(t)
	require.Equal(t, w.Generation+1, after.Generation, "the epoch of the new incarnation")
	require.NotEqual(t, w.Incarnation, after.Incarnation)
	s.cycles(t, 3)
	if s.fake != nil {
		puts, other := putsPerTunnel(s.fake.writes(from))
		require.Equal(t, map[string]int{tunnel: 1}, puts, "one configuration write per tunnel")
		require.Empty(t, other)
	} else {
		require.Equal(t, after.Generation, s.sentinel(t))
	}

	t.Log("a restart of the daemon: nothing written, the epoch kept")
	from = s.calls()
	restarted := time.Now()
	s.hereMust(t, "systemctl", "restart", "pco")
	s.waitState(t, "a cycle of the restarted daemon", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(restarted) && st.Complete && !st.FinishedAt.IsZero()
	})
	s.cycles(t, 3)
	require.Empty(t, s.fakeWrites(from), "the restarted daemon wrote")
	again, _ := s.leader(t)
	require.Equal(t, after.Generation, again.Generation)
}

// copyServesNothing checks a copy of the appliance for a few cycles of the
// original: it knows itself a copy for why, and serves nothing.
func (s *suite) copyServesNothing(t testing.TB, c *suite, why string) {
	t.Helper()
	for range 3 {
		s.cycles(t, 1)
		st := c.state(t)
		require.NotNil(t, st.Identity)
		require.True(t, st.Identity.Copy, "the copy does not know it is one: %+v", st.Identity)
		require.Contains(t, st.Identity.Why, why)
		c.requireServesNothing(t)
	}
}

func testCloneFromSnapshot(t *testing.T, s *suite) {
	a := s.app
	id := strconv.Itoa(a.vmid)
	clone := s.lay.vmid(45)
	cid := strconv.Itoa(clone)
	s.ensureS1(t)
	s.settle(t)
	tunnel := s.tunnel(t)
	ingress := s.cloud.Ingress(t, tunnel)
	w, _ := s.leader(t)
	gen := s.sentinel(t)

	s.must(t, "pct", "snapshot", id, "pcotestclone")
	a.copies[clone] = true
	s.must(t, "pct", "clone", id, cid, "--snapname", "pcotestclone", "--pool", applianceinstallPool, "--hostname", "pco-clone")
	s.must(t, "pct", "delsnapshot", id, "pcotestclone")
	// Without hwaddr Proxmox makes yet another MAC, which is read back.
	s.must(t, "pct", "set", cid, "--"+a.guestNet, "name="+a.guestIface+",bridge="+bridge+",ip="+s.lay.addr(45)+"/24")
	require.NotEqual(t, s.applianceMAC(t), s.cardMAC(t, clone, a.guestNet))
	from := s.calls()
	s.must(t, "pct", "start", cid)
	started := time.Now()
	c := s.inCT(clone)
	s.waitAppliance(t, clone)
	for {
		st, err := c.tryState()
		if err == nil && st.Identity != nil && st.Identity.Copy {
			t.Logf("the clone knew itself a copy %s after its start", time.Since(started).Round(time.Second))
			break
		}
		if time.Since(started) > 30*time.Second {
			t.Fatalf("the clone did not know itself a copy within 30 s of its start: %+v", st.Identity)
		}
		time.Sleep(time.Second)
	}

	added := s.g(28, guest{notes: "cf-tunnel: e2e-as7.ZONE -> :8080"})
	s.create(t, added)
	s.copyServesNothing(t, c, "the volume at /var/lib/pco is")
	cw, found := c.leader(t)
	require.True(t, found)
	require.Equal(t, w, cw, "the clone drew an epoch")
	host := s.host("as7")
	s.waitRoute(t, host, planner.StateActive)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	s.waitRoute(t, s.host("s1"), planner.StateActive)
	require.Equal(t, gen, s.sentinel(t), "a generation other than the original's reached Cloudflare")
	for _, w := range s.fakeWrites(from) {
		require.False(t, strings.HasPrefix(w, "CreateTunnel") || strings.HasPrefix(w, "DeleteTunnel"), "%s", w)
	}
	now := s.cloud.Ingress(t, tunnel)
	without := slices.DeleteFunc(slices.Clone(now), func(r planner.IngressRule) bool { return r.Hostname == host })
	require.Equal(t, ingress, without, "the ingress changed besides the route added")
}

func testCloneWithMAC(t *testing.T, s *suite) {
	a := s.app
	clone := s.lay.vmid(45)
	cid := strconv.Itoa(clone)
	if !a.copies[clone] {
		t.Skip("A-S7 left no clone")
	}
	s.must(t, "pct", "stop", cid)
	mac := s.applianceMAC(t)
	s.must(t, "pct", "set", cid, "--"+a.guestNet, "name="+a.guestIface+",bridge="+bridge+",hwaddr="+mac+",ip="+s.lay.addr(45)+"/24")
	since := time.Now()
	s.must(t, "pct", "start", cid)
	c := s.inCT(clone)
	s.waitAppliance(t, clone)
	copyOf := "lxc/" + cid
	s.waitEvent(t, since, "identity", copyOf)
	st := s.waitState(t, "the original naming the copy", 3*time.Minute, func(st engine.State) bool {
		return st.Identity != nil && slices.Contains(st.Identity.Copies, copyOf) && !st.Identity.OK && !st.Identity.Copy
	})
	t.Logf("the original: %s", st.Identity.Why)
	added := s.g(27, guest{notes: "cf-tunnel: e2e-as8.ZONE -> :8080"})
	s.create(t, added)
	host := s.host("as8")
	s.copyServesNothing(t, c, "")
	for range 3 {
		s.cycles(t, 1)
		require.Empty(t, s.records(t, host), "a route was published while a copy runs")
	}
	s.must(t, "pct", "stop", cid)
	s.must(t, "pct", "set", cid, "--protection", "0")
	s.must(t, "pct", "destroy", cid, "--purge", "1")
	delete(a.copies, clone)
	gone := time.Now()
	s.sync(t)
	s.waitState(t, "the original ok again", twoCycles+time.Minute, func(st engine.State) bool {
		return st.At.After(gone) && st.Identity != nil && st.Identity.OK
	})
	s.waitRecord(t, host, s.tunnel(t), twoCycles+time.Minute)
}

// waitHeldBehind waits until the writer is behind Cloudflare and the hold
// says to recover.
func (s *suite) waitHeldBehind(t testing.TB) engine.State {
	t.Helper()
	return s.waitState(t, "the hold of a state behind Cloudflare", 3*time.Minute, func(st engine.State) bool {
		return st.WriterVerdict == engine.VerdictBehind &&
			slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, "run pco appliance recover") })
	})
}

// recoverAndApply recovers the appliance at once and leaves observe-only
// mode, and returns the generation recover drew.
func (s *suite) recoverAndApply(t testing.TB) int {
	t.Helper()
	out, err := s.pcoCombined("appliance", "recover")
	t.Logf("pco appliance recover:\n%s", out)
	require.NoError(t, err)
	s.waitAppliance(t, s.at.vmid)
	w, found := s.leader(t)
	require.True(t, found)
	s.pcoMust(t, "apply")
	s.waitState(t, "enforce mode", time.Minute, func(st engine.State) bool { return st.Mode == engine.ModeEnforce })
	return w.Generation
}

// waitWritten waits until Cloudflare carries the generation of leader.json.
func (s *suite) waitWritten(t testing.TB) int {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(s.every()) {
		gen := s.leaderNow()
		if gen >= 0 && s.sentinel(t) == gen {
			return gen
		}
		if time.Now().After(deadline) {
			t.Fatalf("the generation %d of leader.json never reached Cloudflare, which has %d", gen, s.sentinel(t))
		}
	}
}

func testRollback(t *testing.T, s *suite) {
	a := s.app
	id := strconv.Itoa(a.vmid)
	s.ensureS1(t)
	s.settle(t)
	snap := "pco-pre-upgrade-" + time.Now().Format("20060102")
	s.must(t, "pct", "snapshot", id, snap)
	t.Cleanup(func() { _, _ = s.run("pct", "delsnapshot", id, snap) })
	g := s.g(26, guest{notes: "cf-tunnel: e2e-as16.ZONE -> :8080"})
	s.create(t, g)
	host := s.host("as16")
	tunnel := s.tunnel(t)
	s.waitRoute(t, host, planner.StateActive)
	s.waitRecord(t, host, tunnel, 2*time.Minute)

	t.Log("a reboot writes the new epoch")
	s.must(t, "pct", "reboot", id)
	s.waitAppliance(t, a.vmid)
	written := s.waitWritten(t)

	t.Log("the rollback to the snapshot taken before: held behind Cloudflare until recover")
	from := s.calls()
	s.must(t, "pct", "rollback", id, snap, "--start", "1")
	s.waitAppliance(t, a.vmid)
	st := s.waitHeldBehind(t)
	t.Logf("the hold: %s", st.Hold)
	writer, ok := findingOf(s.doctorFindings(t), "writer")
	require.True(t, ok, "doctor has no writer check")
	t.Logf("doctor's writer: %s; %s", writer.Detail, writer.Fix)
	require.Contains(t, writer.Detail+" "+writer.Fix, "pco appliance recover")
	require.NotContains(t, strings.ToLower(writer.Detail+" "+writer.Fix), "another install")
	s.cycles(t, 3)
	require.Empty(t, s.fakeWrites(from), "the appliance behind Cloudflare wrote")
	gen := s.recoverAndApply(t)
	require.Greater(t, gen, written)
	s.waitRoute(t, host, planner.StateActive)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	s.must(t, "pct", "delsnapshot", id, snap)

	t.Log("(b) a rollback without a start in between proceeds with the snapshot's state")
	s.ensurePrincipals(t)
	d := s.g(29, guest{notes: "cf-tunnel: e2e-as16b.ZONE -> :8080"})
	s.create(t, d)
	dhost := s.host("as16b")
	vms := "/vms/" + strconv.Itoa(d.vmid)
	s.grant(t, vms, "--users", testUser, testRole)
	s.waitDelegated(t, d, dhost, true)
	s.must(t, "pct", "snapshot", id, "pcotestnoboot")
	t.Cleanup(func() { _, _ = s.run("pct", "delsnapshot", id, "pcotestnoboot") })
	t.Logf("pco guest approve:\n%s", s.pcoMust(t, "guest", "approve", d.owner()))
	s.waitOwnerRoute(t, dhost, d.owner(), planner.StateActive)
	rolled := time.Now()
	s.must(t, "pct", "rollback", id, "pcotestnoboot", "--start", "1")
	s.waitAppliance(t, a.vmid)
	for deadline := time.Now().Add(twoCycles + 2*time.Minute); ; time.Sleep(2 * time.Second) {
		st, err := s.tryState()
		if err == nil {
			require.NotEqual(t, engine.VerdictBehind, st.WriterVerdict, "the rollback without a start was taken for one behind")
			if gen := s.leaderNow(); st.WriterVerdict == engine.VerdictOK && st.At.After(rolled) && gen >= 0 && s.sentinel(t) == gen {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no write verified after the rollback")
		}
	}
	s.waitEvent(t, rolled, "writer", "epoch drawn after a container start; the state is the volume's")
	epoch, ok := findingOf(s.doctorFindings(t), "epoch")
	require.True(t, ok, "doctor has no epoch note")
	require.Contains(t, epoch.Detail, "after a container start")
	s.waitDelegated(t, d, dhost, true)
	s.ungrant(t, vms, testRole)
	s.must(t, "pct", "delsnapshot", id, "pcotestnoboot")
	s.destroy(t, d.vmid)
	s.destroy(t, g.vmid)
	s.sync(t)
}

// refusedForNoVolume checks a container whose volume holds no marker: pco
// does not run, and says to repair it, without restarting again and again.
func (s *suite) refusedForNoVolume(t testing.TB, vmid int) {
	t.Helper()
	c := s.inCT(vmid)
	s.waitExec(t, vmid)
	repair := "pco appliance repair --vmid " + strconv.Itoa(vmid)
	require.Eventually(t, func() bool {
		out, _ := c.here("journalctl", "-b", "-u", "pco.service", "--no-pager")
		return strings.Contains(out, repair)
	}, time.Minute, 2*time.Second, "the journal does not name %s", repair)
	show := func(prop string) string {
		out, _ := c.here("systemctl", "show", "pco.service", "-p", prop, "--value")
		return strings.TrimSpace(out)
	}
	for until := time.Now().Add(30 * time.Second); time.Now().Before(until); time.Sleep(5 * time.Second) {
		require.Equal(t, "0", show("NRestarts"), "pco.service restarts")
	}
	require.Equal(t, "78", show("ExecMainStatus"))
	out, err := c.pcoCombined("status")
	t.Logf("pco status in lxc/%d:\n%s", vmid, out)
	require.Equal(t, 2, exitCode(err))
	require.Contains(t, out, repair)
}

// repair runs pco appliance repair --recover with the token of the run from
// a file only root reads, which goes right after.
func (s *suite) repair(t testing.TB, vmid int) {
	t.Helper()
	marker(t, tokenFile, s.app.token+"\n")
	defer unmark(t, tokenFile)
	args := []string{"appliance", "repair", "--vmid", strconv.Itoa(vmid), "--recover", "--cf-token-file", tokenFile, "--yes"}
	out, err := s.nodePcoRun(installTimeout, args...)
	t.Logf("pco %s:\n%s", strings.Join(args, " "), out)
	require.NoError(t, err)
	s.waitAppliance(t, vmid)
	c := s.inCT(vmid)
	c.editSettings(t, func(v *store.Settings) {
		v.PollInterval = store.Duration(pollInterval)
		v.Grace = store.Duration(grace)
	})
}

func testRestore(t *testing.T, s *suite) {
	a := s.app
	id := strconv.Itoa(a.vmid)
	s.ensureS1(t)
	s.settle(t)
	out := s.must(t, "vzdump", id, "--mode", "snapshot", "--storage", "local", "--compress", "zstd")
	t.Logf("vzdump:\n%s", out)
	archives, _ := filepath.Glob("/var/lib/vz/dump/vzdump-lxc-" + id + "-*.tar.zst")
	require.Len(t, archives, 1)
	archive := archives[0]
	defer func() { _ = os.Remove(archive) }()

	// A restore to another VMID while the appliance is there is a copy
	// beside its original, which repair refuses: repaired, it would write
	// the original's install with its credentials. The appliance falling
	// behind the writes of another is A-S16's rollback.
	t.Log("(a) a restore to another VMID: refused without a restart loop, writes nothing, and repair refuses it beside the original")
	s.must(t, "pct", "stop", id)
	restored := s.lay.vmid(46)
	rid := strconv.Itoa(restored)
	a.copies[restored] = true
	from := s.calls()
	s.must(t, "pct", "restore", rid, archive, "--storage", s.disk, "--unique", "1", "--pool", applianceinstallPool)
	s.must(t, "pct", "start", rid)
	s.refusedForNoVolume(t, restored)
	require.Empty(t, s.fakeWrites(from), "the restore wrote")
	marker(t, tokenFile, a.token+"\n")
	out, err := s.nodePcoRun(installTimeout, "appliance", "repair", "--vmid", rid, "--recover", "--cf-token-file", tokenFile, "--yes")
	unmark(t, tokenFile)
	t.Logf("pco appliance repair of the restore:\n%s", out)
	require.Error(t, err, "repair took a copy beside its original")
	require.Contains(t, out, "lxc/"+rid+" is a copy of lxc/"+id+", which is still there")
	out, err = s.nodePcoRun(installTimeout, "appliance", "uninstall", "--vmid", rid, "--yes", "--keep-cloudflare")
	t.Logf("pco appliance uninstall of lxc/%s:\n%s", rid, out)
	require.NoError(t, err)
	delete(a.copies, restored)
	var tokens []struct {
		TokenID string `json:"tokenid"`
	}
	s.pveJSON(t, &tokens, "pveum", "user", "token", "list", "pco@pve")
	require.Len(t, tokens, 1, "%+v", tokens)
	require.Equal(t, "vm"+id, tokens[0].TokenID)
	require.Empty(t, s.fakeWrites(from), "the copy wrote")
	s.must(t, "pct", "start", id)
	s.waitAppliance(t, a.vmid)
	s.waitState(t, "the original as it was", 3*time.Minute, func(st engine.State) bool {
		return st.Identity != nil && st.Identity.OK && st.WriterVerdict == engine.VerdictOK && st.Complete
	})
	s.waitRoute(t, s.host("s1"), planner.StateActive)

	t.Log("(b) a restore over the appliance: refused until repaired, then served again")
	s.must(t, "pct", "set", id, "--protection", "0")
	s.must(t, "pct", "stop", id)
	s.must(t, "pct", "restore", id, archive, "--force", "1", "--storage", s.disk)
	s.must(t, "pct", "set", id, "--protection", "1")
	s.must(t, "pct", "start", id)
	s.refusedForNoVolume(t, a.vmid)
	before := s.sentinel(t)
	s.repair(t, a.vmid)
	require.Equal(t, s.install, s.applianceInstall(t).ID, "the repair adopted another install")
	require.True(t, s.applianceSettings(t).ObserveOnly)
	w, _ := s.leader(t)
	require.Greater(t, w.Generation, before)
	s.pcoMust(t, "apply")
	s.waitRoute(t, s.host("s1"), planner.StateActive)
	require.Equal(t, w.Generation, s.waitWritten(t))
	require.NoError(t, os.Remove(archive))
}

func testApplianceUninstall(t *testing.T, s *suite) {
	a := s.app
	id := strconv.Itoa(a.vmid)
	s.restoreRegisteredTags(t)
	s.destroyPconetGuests(t)
	s.removeGrants(t)
	s.uninstallAppliance(t)
	require.False(t, a.installed, "the appliance was not uninstalled")

	t.Log("nothing of the appliance is left in Proxmox")
	_, err := s.run("pct", "status", id)
	require.Error(t, err, "lxc/%s is still there", id)
	users := s.must(t, "pveum", "user", "list", "--output-format", "json")
	require.NotContains(t, users, `"pco@pve"`)
	roles := s.must(t, "pveum", "role", "list", "--output-format", "json")
	require.NotContains(t, roles, `"roleid":"PCO"`)
	for _, l := range s.acls(t) {
		require.False(t, strings.HasPrefix(l.UGID, "pco@pve"), "%+v", l)
	}
	_, err = s.run("pvesh", "get", "/pools/"+applianceinstallPool)
	require.Error(t, err, "the pool %s is still there", applianceinstallPool)
	tags := s.registeredTags(t)
	slices.Sort(tags)
	want := slices.Clone(a.tags)
	slices.Sort(want)
	require.Equal(t, want, tags, "the registered tags")

	t.Log("the rest the run made goes, and the node is as it was")
	s.destroyGuests(t)
	s.removePrincipals(t)
	s.removeArchives(t)
	s.restoreFirewall(t)
	s.stopFirewallService(t)
	s.restoreSysctls(t)
	s.removeBridges(t)
	s.removeAddress(t)
	addrs := s.must(t, "ip", "-4", "-o", "addr", "show", "dev", bridge)
	require.NotContains(t, addrs, "inet "+s.lay.nodeAddr+"/")
	_, err = s.run("ip", "link", "show", "dev", segBridge)
	require.Error(t, err, "%s is still there", segBridge)
	require.Empty(t, s.dumps())
	s.requireNodeAsBefore(t, true)
}
