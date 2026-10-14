package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

var testNodeView = engine.NodeView{Name: "pve2", State: "active", Kind: "host", AgentAddress: netip.MustParseAddr("10.9.0.2"), Covered: true}

func (f *fakeEngine) ClusterStatus() engine.ClusterStatus {
	f.record(context.Background(), "cluster")
	return engine.ClusterStatus{Lease: &engine.LeaseView{Holder: "pve1", Generation: 3, Valid: true}, Cluster: engine.ClusterView{Controller: "pve1", Members: []string{"pve1", "pve2"}, Quorate: true}, Nodes: []engine.NodeView{testNodeView}}
}

func (f *fakeEngine) Nodes() []engine.NodeView {
	f.record(context.Background(), "nodes")
	return []engine.NodeView{testNodeView}
}

func (f *fakeEngine) Handoff(ctx context.Context, to string) (engine.HandoffResult, error) {
	f.record(ctx, "handoff:"+to)
	if err := f.failure(); err != nil {
		return engine.HandoffResult{}, err
	}
	return engine.HandoffResult{From: "pve1", To: to, Until: time.Date(2026, 10, 1, 12, 0, 30, 0, time.UTC)}, nil
}

func (f *fakeEngine) NewJoinToken(ctx context.Context, node string) (engine.JoinTokenView, error) {
	f.record(ctx, "join-token:"+node)
	if err := f.failure(); err != nil {
		return engine.JoinTokenView{}, err
	}
	return engine.JoinTokenView{ID: "tok1", Node: node, Expires: time.Date(2026, 10, 1, 12, 15, 0, 0, time.UTC), Token: "pco1.c2VjcmV0"}, nil
}

func (f *fakeEngine) FinishUpgrade(ctx context.Context) (engine.FinishUpgradeResult, error) {
	f.record(ctx, "finish-upgrade")
	if err := f.failure(); err != nil {
		return engine.FinishUpgradeResult{}, err
	}
	return engine.FinishUpgradeResult{From: "0.9.0", Schema: 1, Backup: "/var/lib/pco/backup/1-20261001T120000Z"}, nil
}

func (f *fakeEngine) Unblock(ctx context.Context, addr netip.Addr) error {
	f.record(ctx, "unblock:"+addr.String())
	return f.failure()
}

func (f *fakeEngine) UninstallCluster(ctx context.Context, force bool) (engine.UninstallResult, error) {
	f.record(ctx, fmt.Sprintf("uninstall-cluster:%t", force))
	if err := f.failure(); err != nil {
		return engine.UninstallResult{}, err
	}
	return engine.UninstallResult{Clean: []string{}, Left: []string{"pve2: pco uninstall --yes --keep-cloudflare removes what pco keeps on the node"}}, nil
}

func (f *fakeEngine) AdmitNode(ctx context.Context, name, fingerprint string, renew bool) (engine.NodeView, error) {
	f.record(ctx, fmt.Sprintf("admit:%s:%s:%t", name, fingerprint, renew))
	if err := f.failure(); err != nil {
		return engine.NodeView{}, err
	}
	v := testNodeView
	v.Name = name
	return v, nil
}

func (f *fakeEngine) DrainNode(ctx context.Context, name string, force bool) (engine.NodeView, error) {
	f.record(ctx, fmt.Sprintf("drain:%s:%t", name, force))
	if err := f.failure(); err != nil {
		return engine.NodeView{}, err
	}
	v := testNodeView
	v.Name, v.State, v.Drained = name, "draining", "admin"
	return v, nil
}

func (f *fakeEngine) UndrainNode(ctx context.Context, name string) (engine.NodeView, error) {
	f.record(ctx, "undrain:"+name)
	if err := f.failure(); err != nil {
		return engine.NodeView{}, err
	}
	v := testNodeView
	v.Name = name
	return v, nil
}

func (f *fakeEngine) ForgetNode(ctx context.Context, name string, compromised bool) (engine.ForgetResult, error) {
	f.record(ctx, fmt.Sprintf("forget:%s:%t", name, compromised))
	if err := f.failure(); err != nil {
		return engine.ForgetResult{}, err
	}
	return engine.ForgetResult{Node: name, Compromised: compromised, Done: []string{"the entry of the node is removed"}}, nil
}

func (f *fakeEngine) SetNode(ctx context.Context, name string, p engine.NodePatch) (engine.NodeView, error) {
	eligible := "unset"
	if p.ControllerEligible != nil {
		eligible = fmt.Sprint(*p.ControllerEligible)
	}
	f.record(ctx, fmt.Sprintf("set:%s:%s", name, eligible))
	if err := f.failure(); err != nil {
		return engine.NodeView{}, err
	}
	v := testNodeView
	v.Name = name
	return v, nil
}

func (f *fakeEngine) AgentView() (agentproto.AgentView, error) {
	f.record(context.Background(), "agent")
	if err := f.failure(); err != nil {
		return agentproto.AgentView{}, err
	}
	return agentproto.AgentView{Node: "pve1", Kind: pki.KindHost, Controller: "pve1 (this node)", Connectors: 1}, nil
}

func (f *fakeEngine) ResetAgentEpoch(ctx context.Context) error {
	f.record(ctx, "reset-epoch")
	return f.failure()
}

// Every route of the cluster reaches the engine with what the body says,
// and answers with what the engine said.
func TestTheClusterRoutesReachTheEngine(t *testing.T) {
	for _, tt := range []struct {
		method, target, body string
		status               int
		call, contains       string
	}{
		{http.MethodGet, "/v1/cluster", "", http.StatusOK, "cluster", `"controller":"pve1"`},
		{http.MethodGet, "/v1/nodes", "", http.StatusOK, "nodes", `"name":"pve2"`},
		{http.MethodPost, "/v1/cluster/handoff", `{"to":"pve2"}`, http.StatusOK, "handoff:pve2", `"to":"pve2"`},
		{http.MethodPost, "/v1/cluster/handoff", `{}`, http.StatusOK, "handoff:", `"from":"pve1"`},
		{http.MethodPost, "/v1/cluster/join-tokens", `{"node":"app1"}`, http.StatusCreated, "join-token:app1", `"token":"pco1.c2VjcmV0"`},
		{http.MethodPost, "/v1/cluster/finish-upgrade", `{}`, http.StatusOK, "finish-upgrade", `"backup":"/var/lib/pco/backup/1-20261001T120000Z"`},
		{http.MethodPost, "/v1/cluster/unblock", `{"address":"10.0.0.7"}`, http.StatusOK, "unblock:10.0.0.7", `{}`},
		{http.MethodPost, "/v1/nodes/pve2/admit", `{"fingerprint":"9f86d081","renew":true}`, http.StatusOK, "admit:pve2:9f86d081:true", `"name":"pve2"`},
		{http.MethodPost, "/v1/nodes/pve2/admit", ``, http.StatusOK, "admit:pve2::false", `"name":"pve2"`},
		{http.MethodPost, "/v1/nodes/pve2/drain", `{"force":true}`, http.StatusOK, "drain:pve2:true", `"drained":"admin"`},
		{http.MethodPost, "/v1/nodes/pve2/undrain", ``, http.StatusOK, "undrain:pve2", `"name":"pve2"`},
		{http.MethodPost, "/v1/nodes/pve2/forget", `{"compromised":true}`, http.StatusOK, "forget:pve2:true", `"compromised":true`},
		{http.MethodPut, "/v1/nodes/pve2", `{"controllerEligible":false}`, http.StatusOK, "set:pve2:false", `"name":"pve2"`},
		{http.MethodGet, "/v1/local/agent", "", http.StatusOK, "agent", `"controller":"pve1 (this node)"`},
	} {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			f := &fakeEngine{}
			s := newServer(f)
			rec := do(s, tt.method, tt.target, tt.body)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tt.contains)
			require.Contains(t, f.called(), tt.call)
		})
	}
}

// A refusal of the engine is passed on with its code; a body that is not
// the route's is refused before the engine is asked.
func TestTheClusterRoutesPassRefusalsOn(t *testing.T) {
	f := &fakeEngine{err: fmt.Errorf("%w: node pve3 is contested", engine.ErrRefused)}
	s := newServer(f)
	rec := do(s, http.MethodPost, "/v1/nodes/pve3/admit", `{"fingerprint":"9f86d081"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "refused", parseError(t, rec).Code)

	f = &fakeEngine{}
	s = newServer(f)
	rec = do(s, http.MethodPost, "/v1/cluster/unblock", `{"address":"not-an-address"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "address must be an IP address", parseError(t, rec).Message)
	rec = do(s, http.MethodPost, "/v1/nodes/pve2/drain", `{"force":"yes"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = do(s, http.MethodPut, "/v1/nodes/pve2", ``)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, f.called(), "the engine was asked nothing")
}

// Uninstalling the cluster and resetting the agent's epoch are root's alone.
func TestUninstallClusterAndResetEpochAreRootsAlone(t *testing.T) {
	f := &fakeEngine{}
	s := New(f, "1.2.3", []uint32{0, testUID}, zerolog.Nop())
	s.checkPeers = true
	for _, target := range []string{"/v1/cluster/uninstall", "/v1/local/agent/reset-epoch"} {
		rec := do(s, http.MethodPost, target, `{}`)
		require.Equal(t, http.StatusForbidden, rec.Code, target)
		require.Equal(t, "forbidden", parseError(t, rec).Code)
	}
	require.Empty(t, f.called())

	rec := send(s, requestFrom(0, http.MethodPost, "/v1/cluster/uninstall", `{"force":true}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"left":["pve2: pco uninstall`)
	rec = send(s, requestFrom(0, http.MethodPost, "/v1/local/agent/reset-epoch", ``))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"uninstall-cluster:true", "reset-epoch"}, f.called())
}
