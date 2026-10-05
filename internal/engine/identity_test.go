package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	ownVMID  = 9250
	ownMAC   = "bc:24:11:00:92:50"
	cloneMAC = "bc:24:11:00:92:95"
	bootID   = "6c3f2a8e-1b4d-4e0f-9a2b-7d5c8e1f3a6b"
	incA     = bootID + "/100" // the start that wrote leader.json
	incB     = bootID + "/200"
	incC     = bootID + "/300"
)

var (
	ownRef   = model.GuestRef{Kind: model.KindLXC, VMID: ownVMID}
	cloneRef = model.GuestRef{Kind: model.KindLXC, VMID: 9295}

	ownVolume   = appliance.Source{Raw: "pcotestpool/subvol-9250-disk-1 /", Volume: "pcotestpool/subvol-9250-disk-1", VMID: ownVMID}
	cloneVolume = appliance.Source{Raw: "pcotestpool/subvol-9295-disk-1 /", Volume: "pcotestpool/subvol-9295-disk-1", VMID: 9295}
)

// container is what the container shows of itself, and what Proxmox says of
// the uptimes of the guests.
type container struct {
	mu         sync.Mutex
	mount      appliance.Source
	links      []appliance.NamedLink
	uptimes    map[model.GuestRef]time.Duration
	uptimesErr error
	verifyErr  error
}

func (c *container) facts() (appliance.Facts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return appliance.Facts{Mount: c.mount, Uptime: time.Hour, Links: append([]appliance.NamedLink(nil), c.links...)}, nil
}

func (c *container) readUptimes(context.Context) (map[model.GuestRef]time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.uptimes, c.uptimesErr
}

func (c *container) verify() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verifyErr
}

func (c *container) set(change func(c *container)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	change(c)
}

// zeros is a source of random bytes that are all zero: a nonce drawn from it
// is aaaaaaaa.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// applianceEnv is an engine of an appliance, lxc/9250, whose container shows
// the facts of box.
type applianceEnv struct {
	*env
	box    *container
	self   *appliance.Self
	flag   string
	quorum bool
}

// newApplianceEnv is the appliance in a process that started as incarnation,
// with a leader.json of generation 1 written at the start incA.
func newApplianceEnv(t *testing.T, incarnation string) *applianceEnv {
	t.Helper()
	e := newEnv(t)
	require.NoError(t, e.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID: ownVMID, Node: testNode, MACs: []string{ownMAC},
		Endpoints: []store.Endpoint{{Address: "10.92.0.1:8006", ServerName: testNode}},
	}}))
	require.NoError(t, e.store.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "n1", Incarnation: incA}))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC)))
	a := &applianceEnv{
		env: e,
		box: &container{
			mount:   ownVolume,
			links:   []appliance.NamedLink{{Name: "eth0", MAC: ownMAC}},
			uptimes: map[model.GuestRef]time.Duration{ownRef: time.Hour},
		},
		flag:   filepath.Join(t.TempDir(), "pco-appliance", "identity-ok"),
		quorum: true,
	}
	a.restart(incarnation)
	return a
}

// restart is a new process of the daemon in the container, as incarnation.
func (a *applianceEnv) restart(incarnation string) {
	a.t.Helper()
	a.self = &appliance.Self{
		ID:          appliance.Identity{VMID: ownVMID, Node: testNode, MACs: []string{ownMAC}},
		InstallID:   testInstall,
		Incarnation: incarnation,
		Store:       a.store,
		Facts:       a.box.facts,
		Uptimes:     a.box.readUptimes,
		VerifyError: a.box.verify,
		Endpoint:    store.Endpoint{Address: "10.92.0.1:8006", ServerName: testNode},
		Flag:        a.flag,
		Rand:        zeros{},
		Now:         a.clock.now,
	}
	a.identity, a.epochDrawn, a.incarnation = a.self, a.self.EpochDrawn, incarnation
	a.quorate = func(context.Context) (bool, error) { return a.quorum, nil }
	a.eng = a.newEngine()
}

func ownGuest(macs ...string) model.Guest {
	g := model.Guest{Ref: ownRef, Name: "pco", Node: testNode, Running: true, Pool: appliance.Pool, Identity: "uuid:9250"}
	for i, m := range macs {
		g.NICs = append(g.NICs, model.NIC{Index: i, MAC: m, Bridge: "vmbr1"})
	}
	return g
}

func (a *applianceEnv) writer() planner.Writer {
	a.t.Helper()
	w, found, err := a.store.Writer()
	require.NoError(a.t, err)
	require.True(a.t, found)
	return w
}

func TestACopyStopsServingOnceAndTheNextPassServesAgain(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.enforce()
	st := a.cycle()
	require.True(t, st.Identity.OK, st.Identity.Why)
	require.FileExists(t, a.flag)
	require.Len(t, a.conn.ensures(), 1)
	sets := a.egr.setCount()
	served, _ := a.egr.last()
	require.NotEmpty(t, served)
	writes := len(a.writes())

	a.box.set(func(c *container) { c.mount = cloneVolume })
	st = a.cycle()

	require.True(t, st.Identity.Copy)
	require.False(t, st.Identity.OK)
	require.NoFileExists(t, a.flag)
	require.Equal(t, 1, a.conn.stopAlls())
	require.Equal(t, sets+1, a.egr.setCount(), "emptied, and not fed")
	last, _ := a.egr.last()
	require.Empty(t, last)
	require.Contains(t, st.Hold, "a volume of VMID 9295 and not of lxc/9250: this container is a copy")
	require.Len(t, a.writes(), writes, "nothing written")
	require.Len(t, a.conn.ensures(), 1)
	require.Contains(t, unnumbered(a.eng.Events(time.Time{})), Event{At: t0, Level: "error", Kind: "identity", Subject: "lxc/9250", Guest: "lxc/9250",
		Message: "this container is not lxc/9250: " + st.Identity.Why + "; the connectors are stopped"})

	a.cycle()
	require.Equal(t, 1, a.conn.stopAlls(), "stopped once per change")
	require.Equal(t, sets+1, a.egr.setCount())

	a.box.set(func(c *container) { c.mount = ownVolume })
	st = a.cycle()
	require.True(t, st.Identity.OK)
	require.FileExists(t, a.flag)
	require.Len(t, a.conn.ensures(), 2, "Ensure starts the connector again")
	last, _ = a.egr.last()
	require.Equal(t, served, last)
}

func TestAnApplianceThatIsNotProvenHoldsOnly(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.enforce()
	a.cycle()
	sets := a.egr.setCount()

	a.box.set(func(c *container) { c.uptimesErr = errors.New("proxmox does not answer") })
	st := a.cycle()

	require.False(t, st.Identity.OK)
	require.False(t, st.Identity.Copy)
	require.Equal(t, "the uptimes of the guests could not be read; self-identification waits for them", st.Hold)
	require.FileExists(t, a.flag, "the connectors go on")
	require.Zero(t, a.conn.stopAlls())
	require.Equal(t, sets, a.egr.setCount(), "the set is left as it was")
}

func TestAnIncompleteInventoryHoldsAsOnTheHostOrSaysTheCertificate(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.inv.set(incomplete("cluster resources not listed: tls: failed to verify certificate", ownGuest(ownMAC)))

	st := a.cycle()
	require.Equal(t, problemIncomplete, st.Hold)

	a.box.set(func(c *container) { c.verifyErr = errors.New("x509: certificate signed by unknown authority") })
	st = a.cycle()
	const line = "the certificate of 10.92.0.1:8006 no longer verifies under pve1 (the cluster CA or the pveproxy certificate changed?): " +
		"run pco appliance repair --vmid 9250 on the node"
	require.Equal(t, line, st.Hold)
	require.Contains(t, st.Problems, line)
	require.NotContains(t, st.Problems, problemIncomplete)
	require.Equal(t, line, st.Identity.Why)
}

// The clone sequence of report 3: a clone started with the appliance's
// state on a volume of its own never writes and never draws an epoch, and
// the original, started again, draws one before its first write.
func TestTheCloneSequence(t *testing.T) {
	a := newApplianceEnv(t, incB)
	stored := planner.Writer{InstallID: testInstall, Generation: 5, Nonce: "n5", Incarnation: incA}
	require.NoError(t, a.store.SaveWriter(stored))
	a.cf.SeedTunnel(testAccount, tunnelName, append([]planner.IngressRule{hostRule("www.example.com")},
		planner.IngressRule{Hostname: planner.SentinelHostname(stored), Service: "http_status:404"}, planner.IngressRule{Service: "http_status:404"}))
	a.enforce()
	a.box.set(func(c *container) {
		c.mount = cloneVolume
		c.links = []appliance.NamedLink{{Name: "eth0", MAC: cloneMAC}}
		c.uptimes = map[model.GuestRef]time.Duration{ownRef: 5 * time.Hour, cloneRef: time.Hour}
	})
	clone := ownGuest(cloneMAC)
	clone.Ref = cloneRef
	a.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC), clone))

	for range 3 {
		st := a.cycle()
		require.True(t, st.Identity.Copy, st.Identity.Why)
		a.clock.advance(10 * time.Second)
	}
	require.Equal(t, stored, a.writer(), "no epoch drawn")
	require.False(t, a.self.EpochDrawn())
	require.NoFileExists(t, a.flag)
	require.Equal(t, 1, a.conn.stopAlls())
	require.Empty(t, a.writes())

	// The original, started again.
	a.box.set(func(c *container) {
		c.mount = ownVolume
		c.links = []appliance.NamedLink{{Name: "eth0", MAC: ownMAC}}
		c.uptimes = map[model.GuestRef]time.Duration{ownRef: time.Hour}
	})
	a.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC)))
	a.restart(incC)
	st := a.cycle()
	require.True(t, st.Identity.OK)
	require.FileExists(t, a.flag)
	drawn := planner.Writer{InstallID: testInstall, Generation: 6, Nonce: "aaaaaaaa", Incarnation: incC}
	require.Equal(t, drawn, a.writer())
	require.Empty(t, a.writes(), "the cycle that drew the epoch read leader.json before it")
	require.Equal(t, problemEarlierStart, st.Hold)

	a.clock.advance(10 * time.Second)
	st = a.cycle()
	require.Equal(t, VerdictOK, st.WriterVerdict)
	require.NotEmpty(t, a.writes())
	require.True(t, strings.HasPrefix(a.writes()[0], "PutTunnelConfig"), a.writes())
	rules := a.rules()
	require.Equal(t, planner.SentinelHostname(drawn), rules[len(rules)-2].Hostname, "the first write carries generation 6")
}

func TestReadWriterHoldsOnALeaderJSONOfAnotherStart(t *testing.T) {
	a := newApplianceEnv(t, incB)
	a.identity = nil // nothing passes, so nothing is drawn
	a.eng = a.newEngine()

	st := a.cycle()

	require.Equal(t, problemEarlierStart, st.Hold)
	require.Equal(t, VerdictUnknown, st.WriterVerdict)
	require.Equal(t, incA, a.writer().Incarnation)
}

func TestTheRecoverHintIsTheProfiles(t *testing.T) {
	other := planner.Writer{InstallID: "other1", Generation: 1, Nonce: "n1"}
	host := newEnv(t)
	require.NoError(t, host.store.SaveWriter(other))
	require.Contains(t, host.cycle().Problems, "leader.json names install other1, but this is install abc123; run pco setup --recover")

	a := newApplianceEnv(t, incA)
	require.NoError(t, a.store.SaveWriter(other))
	require.Contains(t, a.cycle().Problems, "leader.json names install other1, but this is install abc123; run pco appliance recover")
}

func TestAForeignConnectorIsAdoptedByTheProfilesRecovery(t *testing.T) {
	a := newApplianceEnv(t, incA)
	m, _, _ := withRealConnectors(a.env)
	require.NoError(t, m.Ensure(t.Context(), oldInstall, oldTunnel, "old-token"))

	st := a.cycle()

	require.Contains(t, st.Problems, "connector for tunnel "+oldTunnel+" belongs to install "+oldInstall+
		"; pco appliance recover adopts that install, pco uninstall on this node removes it")
}

// G2: a rollback or a restore whose state another appliance wrote past
// leaves a sentinel of this install that leader.json does not know.
func TestAStateBehindCloudflareIsSaidAsSuch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		remote planner.Writer
	}{
		{name: "the same generation with another nonce", remote: planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "zz"}},
		{name: "a generation above leader.json", remote: planner.Writer{InstallID: testInstall, Generation: 7, Nonce: "n7"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newApplianceEnv(t, incB) // leader.json of generation 1 at incA: generation 2 is drawn
			a.cf.SeedTunnel(testAccount, tunnelName, append([]planner.IngressRule{hostRule("www.example.com")},
				planner.IngressRule{Hostname: planner.SentinelHostname(tt.remote), Service: "http_status:404"}, planner.IngressRule{Service: "http_status:404"}))
			a.enforce()
			a.cycle() // draws the epoch
			require.True(t, a.self.EpochDrawn())

			st := a.cycle()

			require.Equal(t, VerdictBehind, st.WriterVerdict)
			require.Equal(t, problemBehind, st.Hold)
			require.True(t, hasProblem(st, "which leader.json does not know: "+problemBehind+"; writing stops"), st.Problems)
			require.Empty(t, a.writes())
		})
	}
}

func TestAForeignSentinelOfAnApplianceThatKeptItsEpochStaysForeign(t *testing.T) {
	a := newApplianceEnv(t, incA) // the epoch is kept
	a.cf.SeedTunnel(testAccount, tunnelName, append([]planner.IngressRule{hostRule("www.example.com")},
		planner.IngressRule{Hostname: planner.SentinelHostname(planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "zz"}), Service: "http_status:404"},
		planner.IngressRule{Service: "http_status:404"}))
	a.enforce()

	st := a.cycle()

	require.False(t, a.self.EpochDrawn())
	require.Equal(t, VerdictForeign, st.WriterVerdict)
	require.Equal(t, "the tunnel run found a foreign writer", st.Hold)
	require.True(t, hasProblem(st, "which leader.json does not know: another installation uses install id abc123, "+
		"or the store was lost (pco appliance recover); writing stops"), st.Problems)
}

func TestAfterTheFirstVerifiedWriteAForeignSentinelIsAnotherInstallation(t *testing.T) {
	a := newApplianceEnv(t, incB)
	a.enforce()
	a.cycle() // draws generation 2
	a.clock.advance(10 * time.Second)
	st := a.cycle()
	require.Equal(t, VerdictOK, st.WriterVerdict)
	require.NotEmpty(t, a.writes(), "the first write of this process, verified")
	require.True(t, a.self.EpochDrawn())

	// A second installation with the same install id writes over it.
	tun := a.tunnels()[0]
	_, err := a.cf.PutTunnelConfig(t.Context(), testAccount, tun.ID, append([]planner.IngressRule{hostRule("www.example.com")},
		planner.IngressRule{Hostname: planner.SentinelHostname(planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "zz"}), Service: "http_status:404"},
		planner.IngressRule{Service: "http_status:404"}))
	require.NoError(t, err)
	a.clock.advance(20 * time.Second)
	st = a.cycle()

	require.Equal(t, VerdictForeign, st.WriterVerdict)
	require.True(t, hasProblem(st, "another installation uses install id abc123, or the store was lost (pco appliance recover)"), st.Problems)
}

func TestAnEpochDrawnAfterAStartIsAnEventOnce(t *testing.T) {
	a := newApplianceEnv(t, incB)
	st := a.cycle()
	require.Equal(t, t0, st.EpochDrawnAt)
	a.clock.advance(10 * time.Second)
	st = a.cycle()
	require.Equal(t, t0, st.EpochDrawnAt, "it stays the time of the draw")

	var drawn []Event
	for _, ev := range a.eng.Events(time.Time{}) {
		if strings.HasPrefix(ev.Message, eventEpochDrawn) {
			drawn = append(drawn, ev)
		}
	}
	require.Len(t, drawn, 1)
	require.Equal(t, "writer", drawn[0].Kind)
	require.Equal(t, eventEpochDrawn+" (generation 2)", drawn[0].Message)

	kept := newApplianceEnv(t, incA)
	st = kept.cycle()
	require.True(t, st.EpochDrawnAt.IsZero())
	for _, ev := range kept.eng.Events(time.Time{}) {
		require.NotContains(t, ev.Message, eventEpochDrawn)
	}
}

func TestANotQuorateClusterHolds(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.enforce()
	a.quorum = false

	st := a.cycle()

	require.Equal(t, problemNotQuorate, st.Hold)
	require.Empty(t, a.writes())
	require.FileExists(t, a.flag, "a cluster without quorum is no copy")
}

func TestAPendingNICIsANoteAndNoHold(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC, "bc:24:11:00:92:51")))

	st := a.cycle()

	require.True(t, st.Identity.OK)
	require.Empty(t, st.Hold)
	require.True(t, hasProblem(st, "a NIC of the appliance lxc/9250 appeared or vanished within the last minute"))
}

func TestATenantLosesItsRoutesAndNoOtherGuestDoes(t *testing.T) {
	a := newApplianceEnv(t, incA)
	tenant := guest(140, "borrowed", "t.example.com -> :8080")
	tenant.NICs = []model.NIC{{Index: 0, MAC: ownMAC, Bridge: "vmbr1"}}
	a.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC), tenant))

	st := a.cycle()

	require.True(t, st.Identity.OK)
	require.Equal(t, []string{"qemu/140"}, st.Identity.Tenants)
	require.Contains(t, st.Issues, planner.Issue{Guest: tenant.Ref, Msg: "configured with the MAC bc:24:11:00:92:50 of the appliance lxc/9250"})
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
	require.NotEqual(t, planner.StateActive, route(st, "t.example.com").State)
	require.Contains(t, unnumbered(a.eng.Events(time.Time{})), Event{At: t0, Level: "warn", Kind: "identity", Subject: "qemu/140", Guest: "qemu/140",
		Message: "qemu/140 is configured with a MAC of the appliance lxc/9250; its routes are not served"})
}

// grantToken gives a token of user role on path; the user and the role are
// made when missing.
func (f *fakeAccess) grantToken(user, token string, privsep bool, path, role string, privs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for i := range f.users {
		if f.users[i].ID == user {
			if f.users[i].Tokens == nil {
				f.users[i].Tokens = map[string]bool{}
			}
			f.users[i].Tokens[token], found = privsep, true
		}
	}
	if !found {
		f.users = append(f.users, pve.User{ID: user, Enabled: true, Tokens: map[string]bool{token: privsep}})
	}
	f.roles = append(f.roles, pve.Role{ID: role, Privs: privs})
	f.acl = append(f.acl, pve.ACLEntry{Path: path, Type: "token", UGID: user + "!" + token, RoleID: role, Propagate: true})
}

func TestAPrincipalThatCanReachIntoTheApplianceStopsItUntilItCannot(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.enforce()
	a.cycle()
	require.FileExists(t, a.flag)

	a.acc.grant("alice@pve", "/vms/9250", "PVEVMUser", "VM.Audit", "VM.Console", "VM.PowerMgmt")
	a.acc.grant("bob@pve", "/vms/9250", "PVEVMUser", "VM.Audit", "VM.Console", "VM.PowerMgmt")
	a.acc.grantToken("bob@pve", "ci", true, "/vms/9250", "PVEVMUser", "VM.Audit", "VM.Console", "VM.PowerMgmt")
	a.clock.advance(accessEvery + time.Second)
	writes := len(a.writes())
	st := a.cycle()

	require.Equal(t, []string{"alice@pve", "bob@pve", "bob@pve!ci"}, st.Identity.Exposed)
	require.NoFileExists(t, a.flag)
	require.Equal(t, 1, a.conn.stopAlls())
	last, _ := a.egr.last()
	require.Empty(t, last)
	for _, line := range []string{
		"alice@pve holds VM.Console, VM.PowerMgmt on the appliance lxc/9250; pco serves nothing while it does: " +
			"pveum acl modify /vms/9250 --users alice@pve --roles NoAccess",
		"bob@pve!ci holds VM.Console, VM.PowerMgmt on the appliance lxc/9250; pco serves nothing while it does: " +
			"pveum acl modify /vms/9250 --tokens bob@pve!ci --roles NoAccess",
	} {
		require.Contains(t, st.Problems, line)
	}
	require.True(t, strings.HasPrefix(st.Hold, "alice@pve holds"))
	require.Len(t, a.writes(), writes)

	a.acc.revokeAll()
	a.clock.advance(accessEvery + time.Second)
	ensured := len(a.conn.ensures())
	st = a.cycle()

	require.Empty(t, st.Identity.Exposed)
	require.FileExists(t, a.flag)
	require.Greater(t, len(a.conn.ensures()), ensured, "the cycle's Ensure starts the connectors")
	require.Contains(t, unnumbered(a.eng.Events(time.Time{})), Event{At: a.clock.now(), Level: "info", Kind: "identity", Subject: "lxc/9250", Guest: "lxc/9250",
		Message: "no principal can reach into the appliance any more"})
}

// G3: a token without privilege separation holds the roles of its user, so
// NoAccess on the token would change nothing.
func TestATokenWithoutPrivilegeSeparationIsAnsweredOnItsUser(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.acc.grant("carol@pve", "/vms/9250", "PVEVMAdmin", "VM.Config.Network", "VM.Snapshot")
	a.acc.grantToken("carol@pve", "auto", false, "/", "Nothing")

	st := a.cycle()

	require.Contains(t, st.Problems, "carol@pve!auto holds VM.Config.Network, VM.Snapshot on the appliance lxc/9250; pco serves nothing while it does: "+
		"carol@pve!auto is not privilege-separated: it holds the roles of carol@pve; pveum acl modify /vms/9250 --users carol@pve --roles NoAccess")
	require.Contains(t, st.Problems, "carol@pve holds VM.Config.Network, VM.Snapshot on the appliance lxc/9250; pco serves nothing while it does: "+
		"pveum acl modify /vms/9250 --users carol@pve --roles NoAccess")
}

func TestPoolAllocateIsAnsweredOnThePool(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.acc.grant("dave@pve", "/pool/pco", "PVEPoolAdmin", "Pool.Allocate")

	st := a.cycle()

	require.Contains(t, st.Problems, "dave@pve holds Pool.Allocate on the appliance lxc/9250; pco serves nothing while it does: "+
		"pveum acl modify /pool/pco --users dave@pve --roles NoAccess")
}

func TestAccessThatCannotBeReadLeavesTheConnectorsAlone(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.acc.grant("alice@pve", "/vms/9250", "PVEVMUser", "VM.Console")
	a.acc.fail(errors.New("proxmox does not answer"))

	st := a.cycle()

	require.Empty(t, st.Identity.Exposed)
	require.FileExists(t, a.flag)
	require.Zero(t, a.conn.stopAlls())
}

func TestAPrincipalKnownBeforeTheAccessControlWentUnreadableStaysStopped(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.acc.grant("alice@pve", "/vms/9250", "PVEVMUser", "VM.Console")
	st := a.cycle()
	require.Equal(t, []string{"alice@pve"}, st.Identity.Exposed)

	a.acc.revokeAll()
	a.acc.fail(errors.New("proxmox does not answer"))
	a.clock.advance(accessKeptFor + accessEvery)
	st = a.cycle()

	require.Equal(t, []string{"alice@pve"}, st.Identity.Exposed, "only a read that finds none lets the appliance serve again")
	require.NoFileExists(t, a.flag)
	require.Equal(t, 1, a.conn.stopAlls())
}

// An address verified again after its MAC moved does not fill the egress set
// of a copy.
func TestAMovedAddressVerifiedAgainLeavesTheSetOfACopyEmpty(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.enforce()
	a.cycle()
	require.NotEmpty(t, a.eng.Bound())
	a.box.set(func(c *container) { c.mount = cloneVolume })
	a.cycle()
	log := &callLog{}
	a.egr.log = log

	a.eng.Moved(t.Context(), guestAddr)
	a.eng.moving.Wait()

	require.Equal(t, []string{"egress remove 10.0.0.11"}, log.all())
}

// Recommended 1 and failure mode 5: a NIC configured without a link, for
// more than the minute, holds writes and cuts nothing when the mount proves
// the container; without that proof it is a copy.
func TestANICWithoutALinkHoldsWritesUnlessNothingButTheUptimeProves(t *testing.T) {
	for _, tt := range []struct {
		name  string
		mount appliance.Source
		copy  bool
	}{
		{name: "proven by the mount", mount: ownVolume},
		{name: "proven by the uptime", mount: appliance.Source{Raw: "10.92.0.5:/export/pco /"}, copy: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newApplianceEnv(t, incA)
			a.enforce()
			a.box.set(func(c *container) { c.mount = tt.mount })
			a.cycle()
			writes := len(a.writes())
			a.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC, "bc:24:11:00:92:51")))
			a.cycle()
			a.clock.advance(61 * time.Second)

			st := a.cycle()

			require.False(t, st.Identity.OK)
			require.Equal(t, tt.copy, st.Identity.Copy)
			require.Contains(t, st.Hold, "for more than 60 s")
			require.Len(t, a.writes(), writes, "writes held")
			if tt.copy {
				require.NoFileExists(t, a.flag)
				require.Equal(t, 1, a.conn.stopAlls())
			} else {
				require.FileExists(t, a.flag, "the connectors go on")
				require.Zero(t, a.conn.stopAlls())
			}
		})
	}
}

func TestARotationIsRefusedWhileTheApplianceServesNothing(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.enforce()
	a.cycle()
	a.box.set(func(c *container) { c.mount = cloneVolume })
	a.cycle()
	calls := len(a.cf.Calls())

	_, err := a.eng.RotateTunnel(t.Context(), "")

	require.ErrorIs(t, err, ErrRefused)
	require.Empty(t, a.callsSince(calls), "nothing asked of Cloudflare")
}
