package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
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
}

func (w *guardWorld) guard() *agentGuard {
	return &agentGuard{
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
	require.NoError(t, d.stop())
}
