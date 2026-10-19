package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// guardWorld is what the guard of an agent container on pve2 reads, each
// part changed by a test.
type guardWorld struct {
	verdict appliance.Verdict
	node    string
	lock    string
	ha      bool
	placeErr,
	accErr error
	acc access.Data
	at  time.Time
}

func (w *guardWorld) guard() *agentGuard {
	return &agentGuard{
		now:  func() time.Time { return w.at },
		vmid: 9301, node: "pve2", ownUser: "pco@pve",
		verdict: func(context.Context) appliance.Verdict { return w.verdict },
		placement: func(context.Context) ([]pve.Resource, []pve.HAResource, error) {
			res := []pve.Resource{{Kind: model.KindLXC, VMID: 9301, Node: w.node, Lock: w.lock}}
			var ha []pve.HAResource
			if w.ha {
				ha = []pve.HAResource{{SID: "ct:9301", Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9301}}}
			}
			return res, ha, w.placeErr
		},
		access: func(context.Context) (access.Data, error) { return w.acc, w.accErr },
	}
}

func goodGuardWorld() *guardWorld {
	return &guardWorld{verdict: appliance.Verdict{OK: true}, node: "pve2"}
}

func TestAnAgentServesOnlyWhileItIsProvenHomeAndUnreachable(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(w *guardWorld)
		want   string
	}{
		{"a copy", func(w *guardWorld) { w.verdict = appliance.Verdict{Copy: true, Why: "this container is a copy"} }, "this container is a copy"},
		{"not proven", func(w *guardWorld) { w.verdict = appliance.Verdict{Why: "no uptime"} }, "no uptime"},
		{"moved", func(w *guardWorld) { w.node = "pve3" }, "agent 9301 runs on pve3, enrolled for pve2"},
		{"an HA resource", func(w *guardWorld) { w.ha = true }, "agent 9301 is a Proxmox HA resource"},
		{"never placed", func(w *guardWorld) { w.placeErr = errors.New("no answer") }, "where agent 9301 runs has not been read yet"},
		{"access never read", func(w *guardWorld) { w.accErr = errors.New("no answer") }, "the access control of Proxmox has not been read yet"},
		{"reachable", func(w *guardWorld) {
			w.acc = access.Data{
				Roles: []pve.Role{{ID: "PVEVMAdmin", Privs: []string{"VM.Console"}}},
				ACL:   []pve.ACLEntry{{Path: "/vms/9301", Type: "user", UGID: "eve@pve", RoleID: "PVEVMAdmin", Propagate: true}},
				Users: []pve.User{{ID: "eve@pve", Enabled: true}},
			}
		}, "eve@pve can reach into the agent lxc/9301"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := goodGuardWorld()
			tt.change(w)

			serve, why := w.guard().judge(t.Context())

			require.False(t, serve)
			require.Contains(t, why, tt.want)
		})
	}
	serve, why := goodGuardWorld().guard().judge(t.Context())
	require.True(t, serve)
	require.Empty(t, why)
}

// What cannot be judged in one check keeps the verdict of the check before:
// a moment of an incomplete inventory, an unanswered read or a migration
// neither starts nor stops the connectors.
func TestWhatCannotBeJudgedKeepsTheLastVerdict(t *testing.T) {
	w := goodGuardWorld()
	g := w.guard()
	serve, _ := g.judge(t.Context())
	require.True(t, serve)

	w.verdict = appliance.Verdict{Why: appliance.WhyIncomplete}
	w.placeErr, w.accErr, w.lock, w.node = errors.New("no answer"), errors.New("no answer"), "migrate", "pve3"
	serve, _ = g.judge(t.Context())
	require.True(t, serve)

	w.placeErr, w.lock = nil, ""
	serve, why := g.judge(t.Context())
	require.False(t, serve)
	require.Contains(t, why, "runs on pve3")
	w.verdict, w.node = appliance.Verdict{Why: appliance.WhyIncomplete}, "pve2"
	w.accErr = errors.New("no answer")
	serve, _ = g.judge(t.Context())
	require.True(t, serve, "back home, with nothing else decided against it")
}

// An agent container without a certificate and without a join token says
// what it needs, starts no connector and answers for its agent alone.
func TestAnAgentContainerWithoutATokenSaysWhatItNeeds(t *testing.T) {
	a := newApplianceWorld(t)
	inst, _, err := a.store.Install()
	require.NoError(t, err)
	inst.ID = "0123456789ab"
	inst.Appliance.Kind = store.ApplianceAgent
	require.NoError(t, a.store.SaveInstall(inst))

	d := a.start()
	require.Eventually(t, func() bool {
		v, err := d.client.AgentStatus(t.Context())
		return err == nil && strings.Contains(strings.Join(v.Problems, "\n"), "has no certificate and no join token")
	}, 10*time.Second, 5*time.Millisecond)

	st := d.state()
	require.Contains(t, st.Hold, "this container is the pco agent of node pve1")
	_, err = d.client.Apply(t.Context(), false, "")
	require.ErrorContains(t, err, "the pco agent of node pve1")
	require.Empty(t, a.sysd.units(), "no connector")
	findings, err := d.client.Doctor(t.Context())
	require.NoError(t, err)
	checks := map[string]string{}
	for _, f := range findings {
		checks[f.Check] = f.Detail
	}
	require.Contains(t, checks, "agent", "the agent container answers the doctor for its agent")
	require.Contains(t, checks["agent"], "no certificate and no join token")
	require.NotContains(t, checks, "state", "and not with the line of a container without state")
	require.NoError(t, d.stop())
}

// Verdicts that cannot be made are stood in for by the last ones for five
// minutes; after that the agent holds until a check is made again.
func TestAnAgentHoldsOnceNothingCouldBeJudgedForFiveMinutes(t *testing.T) {
	w := goodGuardWorld()
	w.at = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	g := w.guard()
	serve, _ := g.judge(t.Context())
	require.True(t, serve)

	w.placeErr = errors.New("no answer")
	w.at = w.at.Add(time.Minute)
	serve, _ = g.judge(t.Context())
	require.True(t, serve)
	w.at = w.at.Add(4*time.Minute + 59*time.Second)
	serve, _ = g.judge(t.Context())
	require.True(t, serve)
	w.at = w.at.Add(time.Second)
	serve, why := g.judge(t.Context())
	require.False(t, serve)
	require.Contains(t, why, "nothing proves this agent for 5m0s: where the container runs cannot be read: no answer")

	w.placeErr = nil
	serve, _ = g.judge(t.Context())
	require.True(t, serve, "a check that is made again ends it")

	w.verdict = appliance.Verdict{Pending: true, Why: appliance.WhyPendingNIC}
	serve, _ = g.judge(t.Context())
	require.True(t, serve, "a pending NIC keeps the verdict before")
}

// The loop of an agent container pings the watchdog of systemd as the
// engine's loop does: while a check runs that began less than the cycle
// deadline ago, or while the next one is due or was due less than the grace
// ago. pco.service has the same WatchdogSec in every kind of appliance.
func TestAnAgentContainerMakesProgressWhileItsChecksRunOnTime(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := &agentRun{now: func() time.Time { return now }}
	require.False(t, r.healthy(now), "before the loop begins, no check is due")

	r.progress.due(now)
	require.True(t, r.healthy(now))
	require.True(t, r.healthy(now.Add(14*time.Second)))
	require.False(t, r.healthy(now.Add(15*time.Second)), "a check that is 15 s late is no progress")

	r.progress.started(now)
	require.True(t, r.healthy(now.Add(74*time.Second)))
	require.False(t, r.healthy(now.Add(75*time.Second)), "a check that runs for 75 s hangs")

	r.progress.due(now.Add(agentCheckEvery))
	require.True(t, r.healthy(now.Add(agentCheckEvery+14*time.Second)))
	require.False(t, r.healthy(now.Add(agentCheckEvery+15*time.Second)))
}

// An agent container pings the watchdog of systemd from its start: a daemon
// that never pinged would be restarted by pco.service every WatchdogSec,
// and the connectors with it.
func TestAnAgentContainerPingsTheWatchdog(t *testing.T) {
	a := newApplianceWorld(t)
	inst, _, err := a.store.Install()
	require.NoError(t, err)
	inst.ID = "0123456789ab"
	inst.Appliance.Kind = store.ApplianceAgent
	require.NoError(t, a.store.SaveInstall(inst))

	d := a.start()
	require.Eventually(t, func() bool { return a.notify.pinged() > 0 }, 10*time.Second, 5*time.Millisecond,
		"the watchdog is pinged as the agent's loop begins")
	require.NoError(t, d.stop())
}

// agentFiles are the TLS files of an agent with a client certificate, so
// that a step reaches its checks.
func agentFiles(t *testing.T, now time.Time) agent.TLSFiles {
	t.Helper()
	ca, err := pki.NewCA("0123456789ab", now, rand.Reader)
	require.NoError(t, err)
	id := pki.Identity{Kind: pki.KindAppliance, InstallID: "0123456789ab", Node: "pve2", VMID: 9301}
	key, err := pki.NewKey(rand.Reader)
	require.NoError(t, err)
	csr, err := pki.NewCSR(key, id, rand.Reader)
	require.NoError(t, err)
	cert, err := ca.IssueClient(csr, id, now, rand.Reader)
	require.NoError(t, err)
	files := agent.TLSFiles{Dir: filepath.Join(t.TempDir(), "tls")}
	require.NoError(t, files.SaveClient(agent.Creds{Key: key, Cert: cert, CA: ca.Cert}))
	return files
}

// stopRecorder stands in for the agent in hold: it records what was left of
// the context the stop ran on.
type stopRecorder struct {
	halted, stopped bool
	live            bool
}

func (s *stopRecorder) Halt(ctx context.Context) error {
	s.halted, s.live = true, ctx.Err() == nil
	return nil
}

func (s *stopRecorder) StopConnectors(ctx context.Context) error {
	s.stopped = true
	return nil
}

// The checks of a step end at agentChecksDeadline, well inside what the
// watchdog allows a step: a slow Proxmox API cuts them short and keeps the
// last verdict, where checks that outlasted the watchdog would have the
// daemon, and its connectors, restarted.
func TestTheChecksOfAnAgentContainerEndAtTheirDeadline(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	files := agentFiles(t, now)

	w := goodGuardWorld()
	var deadline time.Time
	var bounded bool
	w.verdict = appliance.Verdict{Why: "no uptime"}
	g := w.guard()
	inner := g.verdict
	g.verdict = func(ctx context.Context) appliance.Verdict {
		deadline, bounded = ctx.Deadline()
		return inner(ctx)
	}
	r := &agentRun{now: time.Now, files: files, guard: g, flag: func(bool) error { return nil }, stopped: true}

	before := time.Now()
	r.step(t.Context())

	require.True(t, bounded, "the checks run on a context with a deadline")
	require.WithinDuration(t, before.Add(agentChecksDeadline), deadline, 5*time.Second)
	require.Less(t, agentChecksDeadline+agentHoldDeadline, 75*time.Second, "checks and hold end inside the time the watchdog allows a step")
}

// Checks that used all of their time, on a slow Proxmox API, do not starve
// the hold that follows them: the egress filter is emptied and the
// connectors are stopped on a context of their own, so the agent fails
// closed when nothing proves it.
func TestTheHoldOfAnAgentContainerHasItsOwnTime(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	files := agentFiles(t, now)
	w := goodGuardWorld()
	w.verdict = appliance.Verdict{Why: "no uptime"}
	g := w.guard()
	inner := g.verdict
	g.verdict = func(ctx context.Context) appliance.Verdict {
		<-ctx.Done()
		return inner(ctx)
	}
	stop := &stopRecorder{}
	r := &agentRun{now: time.Now, files: files, guard: g, stopper: stop, flag: func(bool) error { return nil },
		checksFor: 20 * time.Millisecond, holdFor: time.Second}

	r.step(t.Context())

	require.True(t, stop.halted, "the egress filter is emptied")
	require.True(t, stop.live, "on a context that is not over yet")
	require.True(t, stop.stopped, "and the connectors are stopped")
	require.True(t, r.stopped)
}

// The doctor of an agent container sees what its checks found: the identity
// as the guard last judged it, and the egress filter as the keeper last found
// it, instead of saying that neither has run.
func TestTheStateOfAnAgentContainerHasItsIdentityAndEgress(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	files := agentFiles(t, now)
	w := goodGuardWorld()
	w.at = now
	w.verdict = appliance.Verdict{Why: "no uptime"}
	notes := &agentNotes{}
	r := &agentRun{now: time.Now, files: files, guard: w.guard(), notes: notes, flag: func(bool) error { return nil }, stopped: true}
	require.Nil(t, notes.state("pve2", now).Identity, "nothing judged yet")

	r.step(t.Context())
	notes.noteEgress(engine.EgressCheck{Failed: "cannot list"})
	notes.noteEgress(engine.EgressCheck{View: engine.EgressView{State: engine.EgressOn}})
	notes.noteEgress(engine.EgressCheck{Failed: "cannot list"})

	st := notes.state("pve2", now)
	require.Equal(t, &engine.IdentityView{VMID: 9301, Node: "pve2", Why: "no uptime", CheckedAt: now}, st.Identity)
	require.Equal(t, engine.EgressOn, st.Egress.State, "a check that could not tell keeps what the last one found")
	w.verdict = appliance.Verdict{Copy: true, Why: "this container is a copy"}
	r.step(t.Context())
	require.Equal(t, &engine.IdentityView{VMID: 9301, Node: "pve2", Why: "this container is a copy", Copy: true, CheckedAt: now}, notes.state("pve2", now).Identity)
}
