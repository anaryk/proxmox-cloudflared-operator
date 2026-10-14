package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

// t0 is the time of the tests; the zone shows in every time the commands print.
var (
	t0       = time.Date(2026, 10, 1, 12, 0, 0, 123_000_000, time.UTC)
	testZone = time.FixedZone("CEST", 2*60*60)
)

const cliVersion = "0.2.0"

// testEnv is a machine where it is 2026-10-01 12:00 UTC, in a zone two hours
// ahead, and where stdin is no terminal.
func testEnv() env {
	e := defaultEnv()
	e.now = func() time.Time { return t0 }
	e.loc = testZone
	e.version = cliVersion
	e.stdinTerminal = func(io.Reader) (int, bool) { return 0, false }
	e.readPassword = func(int) ([]byte, error) { panic("no terminal in this test") }
	e.getenv = func(string) string { return "" }
	e.profileFile = "/nonexistent/pco/profile"
	e.webDir = "/nonexistent/pco/web"
	e.net0File = "/nonexistent/pco/net0"
	return e
}

// fakeEngine is the engine behind a daemon of a test: it answers from its
// fields and remembers what it was asked.
type fakeEngine struct {
	mu     sync.Mutex
	state  engine.State
	events []engine.Event
	since  time.Time // what the last request for the events asked for
	kinds  []string  // and of which kinds

	addView, checkView engine.CredentialView
	addErr, checkErr   error
	applyErr, adoptErr error
	removeErr          error
	applied            *engine.ApplyResult // what Apply answers instead of what the state offers

	claims                            []engine.ClaimView
	approvals                         []engine.ApprovalView
	segments                          []engine.SegmentView
	steps                             []doctor.Step
	findings                          []doctor.Finding
	resolveErr, guestErr, diagnoseErr error
	mode                              string // the admission mode an approval answers with; "approve" when empty

	settings      engine.SettingsView
	saved         store.Settings // what the last save of the settings was sent
	restartNeeded []string       // what a save of the settings answers needs a restart
	manual        []engine.ManualRouteView
	guests        []engine.GuestListView
	configErr     error // what a write of the settings or of a manual route fails with

	// The networks: those attached, by id with their service addresses and
	// the routes served through them, the candidates, what a probe answers
	// and what an attach or a detach fails with.
	networks   []engine.NetworkDetail
	candidates []engine.CandidateNetwork
	probe      engine.ProbeResult
	scoped     engine.ProbeResult // what a probe of a scope answers
	networkErr error

	// The cluster: what the agent of the node shows of itself, and what an
	// action on the cluster or a node fails with.
	agent      agentproto.AgentView
	clusterErr error

	calls []string
}

func (f *fakeEngine) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeEngine) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeEngine) State() engine.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeEngine) RunsAs() (profile, node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Profile, f.state.Node
}

func (f *fakeEngine) QueryEvents(q engine.EventQuery) ([]engine.Event, error) {
	f.record("events")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since, f.kinds = q.Since, q.Kind
	return f.events, nil
}

func (f *fakeEngine) Boot() string { return "9f2c4e1a0b7d3c55" }

// Subscribe answers with a stream that has nothing to say until ctx ends.
func (f *fakeEngine) Subscribe(ctx context.Context, _ string, _ uint64) (<-chan engine.Notice, engine.Hello, error) {
	ch := make(chan engine.Notice)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, engine.Hello{Boot: f.Boot(), PollInterval: "10s"}, nil
}

func (f *fakeEngine) lastSince() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since
}

func (f *fakeEngine) lastKinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kinds
}

// Credentials answers with the credentials of the state.
func (f *fakeEngine) Credentials() ([]engine.CredentialView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Credentials, nil
}

func (f *fakeEngine) Trigger() { f.record("sync") }

// Apply answers as the engine does: a confirmation with another offer than
// that of the state is refused, and one with it accepts what the state shows.
func (f *fakeEngine) Apply(_ context.Context, confirmDeletes bool, offer string) (engine.ApplyResult, error) {
	call := "apply confirmDeletes=" + boolText(confirmDeletes)
	if offer != "" {
		call += " offer=" + offer
	}
	f.record(call)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.applyErr != nil:
		return engine.ApplyResult{}, f.applyErr
	case f.applied != nil:
		return *f.applied, nil
	case confirmDeletes && offer != f.state.Offer:
		return engine.ApplyResult{}, errOfferChanged
	}
	res := engine.ApplyResult{LeftObserveOnly: f.state.Mode == "observe", Accepted: []engine.Waiting{}}
	if confirmDeletes {
		res.Accepted = append(res.Accepted, f.state.Waiting...)
	}
	return res, nil
}

// errOfferChanged is how the engine refuses a confirmation of what no longer
// waits.
var errOfferChanged = fmt.Errorf("%w: what waits for a confirmation changed since it was shown; look again and repeat", engine.ErrRefused)

func (f *fakeEngine) Adopt(_ context.Context, name string) error {
	f.record("adopt " + name)
	return f.adoptErr
}

// RotateTunnel is root's alone, which no test is: the tests of pco tunnel
// rotate answer from a daemon of their own.
func (f *fakeEngine) RotateTunnel(_ context.Context, account string) (engine.TunnelRotation, error) {
	f.record("rotate " + account)
	return engine.TunnelRotation{}, fmt.Errorf("%w: not in this test", engine.ErrRefused)
}

// Promote is root's alone too: the tests of pco promote answer from a
// daemon of their own.
func (f *fakeEngine) Promote(_ context.Context, req engine.PromoteRequest) (engine.PromoteResult, error) {
	f.record(fmt.Sprintf("promote %t", req.Confirm))
	return engine.PromoteResult{}, fmt.Errorf("%w: not in this test", engine.ErrRefused)
}

func (f *fakeEngine) AddCredential(_ context.Context, label, token string) (engine.CredentialView, error) {
	f.record("add " + label + " " + token)
	return f.addView, f.addErr
}

func (f *fakeEngine) CheckCredential(_ context.Context, id string, deep bool) (engine.CredentialView, error) {
	f.record("check " + id + " deep=" + boolText(deep))
	return f.checkView, f.checkErr
}

func (f *fakeEngine) RemoveCredential(_ context.Context, id string) error {
	f.record("remove " + id)
	return f.removeErr
}

func (f *fakeEngine) Claims() ([]engine.ClaimView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, nil
}

func (f *fakeEngine) ResolveClaim(_ context.Context, hostname, owner string) error {
	f.record("resolve " + hostname + " " + owner)
	return f.resolveErr
}

func (f *fakeEngine) Approvals() ([]engine.ApprovalView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approvals, nil
}

func (f *fakeEngine) ApproveGuest(_ context.Context, owner, identity string, macs []string, addrs []netip.Addr) (engine.Approval, error) {
	call := "approve " + owner
	if identity != "" {
		call += " identity=" + identity
	}
	if len(macs) > 0 {
		call += fmt.Sprintf(" macs=%v", macs)
	}
	if len(addrs) > 0 {
		call += fmt.Sprintf(" addresses=%v", addrs)
	}
	f.record(call)
	if f.guestErr != nil {
		return engine.Approval{}, f.guestErr
	}
	mode := f.mode
	if mode == "" {
		mode = "approve"
	}
	return engine.Approval{Owner: owner, Identity: "uuid:" + strings.TrimPrefix(owner, "qemu/"), Mode: mode}, nil
}

func (f *fakeEngine) RevokeGuest(_ context.Context, owner string) error {
	f.record("revoke " + owner)
	return f.guestErr
}

func (f *fakeEngine) Segments() ([]engine.SegmentView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.segments, nil
}

func (f *fakeEngine) AcknowledgeSegment(_ context.Context, bridge string, vlan int) error {
	f.record(fmt.Sprintf("acknowledge %s:%d", bridge, vlan))
	return nil
}

func (f *fakeEngine) RevokeSegment(_ context.Context, bridge string, vlan int) error {
	f.record(fmt.Sprintf("revoke segment %s:%d", bridge, vlan))
	return nil
}

func (f *fakeEngine) Diagnose(_ context.Context, hostname string) ([]doctor.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps, f.diagnoseErr
}

func (f *fakeEngine) Doctor(ctx context.Context) []doctor.Finding {
	if doctor.OptionsOf(ctx).Cluster {
		f.record("doctor --cluster")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.findings
}

func (f *fakeEngine) SettingsView() (engine.SettingsView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings, nil
}

// SaveSettings answers with the settings at the next revision.
func (f *fakeEngine) SaveSettings(_ context.Context, rev int, s store.Settings) (engine.SettingsView, []string, error) {
	f.record(fmt.Sprintf("save settings rev=%d gateTag=%s cloudflareBudget=%d", rev, s.GateTag, s.CloudflareBudget))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = s
	if f.configErr != nil {
		return engine.SettingsView{}, nil, f.configErr
	}
	v := f.settings
	v.Rev, v.Settings = rev+1, s
	return v, f.restartNeeded, nil
}

func (f *fakeEngine) ManualRoutes() ([]engine.ManualRouteView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manual, nil
}

// CreateManualRoute makes the route at revision 1, under an id of its own
// when it has none.
func (f *fakeEngine) CreateManualRoute(_ context.Context, v engine.ManualRouteView) (engine.ManualRouteView, error) {
	data, _ := json.Marshal(v)
	f.record("add manual " + string(data))
	if f.configErr != nil {
		return engine.ManualRouteView{}, f.configErr
	}
	if v.ID == "" {
		v.ID = "1a2b3c4d"
	}
	v.Rev = 1
	return v, nil
}

func (f *fakeEngine) UpdateManualRoute(_ context.Context, id string, rev int, v engine.ManualRouteView) (engine.ManualRouteView, error) {
	f.record(fmt.Sprintf("update manual %s rev=%d", id, rev))
	v.ID, v.Rev = id, rev+1
	return v, f.configErr
}

func (f *fakeEngine) DeleteManualRoute(_ context.Context, id string, rev int) error {
	f.record(fmt.Sprintf("remove manual %s rev=%d", id, rev))
	return f.configErr
}

func (f *fakeEngine) Guests() ([]engine.GuestListView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.guests, nil
}

func (f *fakeEngine) Annotation(ref model.GuestRef) (engine.AnnotationView, error) {
	return engine.AnnotationView{}, fmt.Errorf("%w: %s is not in this test", engine.ErrNotFound, ref)
}

func (f *fakeEngine) RequestRestart(context.Context) error {
	f.record("restart")
	return nil
}

func (f *fakeEngine) PollInterval() time.Duration { return 10 * time.Second }

func (f *fakeEngine) Networks() ([]engine.NetworkView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []engine.NetworkView{}
	for _, n := range f.networks {
		out = append(out, n.NetworkView)
	}
	return out, nil
}

func (f *fakeEngine) Network(id string) (engine.NetworkDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.networks {
		if n.ID == id {
			return n, nil
		}
	}
	return engine.NetworkDetail{}, fmt.Errorf("%w: network %s is not attached", engine.ErrNotFound, id)
}

func (f *fakeEngine) CandidateNetworks(context.Context) ([]engine.CandidateNetwork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.candidates, nil
}

// ProbeNetwork answers with the probe of the test, for the bridge, the
// prefix and the address asked for where the request names them.
func (f *fakeEngine) ProbeNetwork(_ context.Context, r engine.ProbeRequest) (engine.ProbeResult, error) {
	call := fmt.Sprintf("probe %s:%d prefix=%v address=%v", r.Bridge, r.VLAN, r.Prefix, r.Address)
	if r.Scope != "" || r.Node != "" || len(r.Legs) > 0 {
		call += fmt.Sprintf(" scope=%s node=%s legs=%v", r.Scope, r.Node, r.Legs)
	}
	f.record(call)
	f.mu.Lock()
	defer f.mu.Unlock()
	res := f.probe
	if r.Scope != "" {
		res = f.scoped
		res.Scope = r.Scope
	}
	res.Bridge, res.VLAN = r.Bridge, r.VLAN
	if r.Prefix.IsValid() {
		res.Prefix = r.Prefix
	}
	if r.Address.IsValid() {
		res.Address = r.Address
	}
	return res, nil
}

func (f *fakeEngine) Attach(_ context.Context, r engine.AttachRequest) (engine.NetworkView, error) {
	call := fmt.Sprintf("attach %s:%d prefix=%v address=%v confirmUplink=%t overlapOK=%t",
		r.Bridge, r.VLAN, r.Prefix, r.Address, r.ConfirmUplink, r.OverlapOK)
	if r.Scope != "" || r.Node != "" || len(r.Legs) > 0 {
		call += fmt.Sprintf(" scope=%s node=%s legs=%v", r.Scope, r.Node, r.Legs)
	}
	f.record(call)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.networkErr != nil {
		return engine.NetworkView{}, f.networkErr
	}
	if len(r.Legs) > 0 {
		v := engine.NetworkView{Network: store.Network{
			ID: store.SegmentID(r.Bridge, r.VLAN), Kind: store.NetworkAttach, Bridge: r.Bridge, VLAN: r.VLAN, Prefix: r.Prefix,
			Legs: r.Legs, LegMAC: "02:50:43:4f:00:01", Mark: 1, Scope: r.Scope, CreatedAt: t0, Rev: 1,
		}}
		for _, node := range slices.Sorted(maps.Keys(r.Legs)) {
			v.Legs = append(v.Legs, engine.LegView{Node: node, Addr: r.Legs[node], Problem: "the gateway has not reported the leg yet"})
		}
		return v, nil
	}
	return engine.NetworkView{
		Network: store.Network{
			ID: store.SegmentID(r.Bridge, r.VLAN), Kind: store.NetworkAttach, Bridge: r.Bridge, VLAN: r.VLAN, Prefix: r.Prefix,
			Legs: map[string]netip.Addr{"pve1": r.Address}, LegMAC: "02:50:43:4f:00:01", LegMACs: map[string]string{"pve1": "02:50:43:4f:00:01"}, Mark: 1, OverlapOK: r.OverlapOK, CreatedAt: t0, Rev: 1,
		},
		Legs: []engine.LegView{{Node: "pve1", Addr: r.Address, Problem: "the gateway has not reported the leg yet"}},
	}, nil
}

func (f *fakeEngine) Detach(_ context.Context, id string, rev int64, served []string) error {
	f.record(fmt.Sprintf("detach %s rev=%d served=%v", id, rev, served))
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networkErr
}

// Managed answers with the managed network of the state.
func (f *fakeEngine) Managed() (engine.ManagedView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Managed, nil
}

func (f *fakeEngine) ApproveManaged(_ context.Context, owner, identity string, accept bool) (store.Consent, error) {
	call := "approve managed " + owner
	if identity != "" {
		call += " identity=" + identity
	}
	if accept {
		call += " accept-firewall-impact"
	}
	f.record(call)
	if f.guestErr != nil {
		return store.Consent{}, f.guestErr
	}
	return store.Consent{Owner: owner, Identity: "uuid:" + strings.TrimPrefix(strings.TrimPrefix(owner, "qemu/"), "lxc/"), At: t0, AcceptFirewallImpact: accept}, nil
}

func (f *fakeEngine) RevokeManaged(_ context.Context, owner string) error {
	f.record("revoke managed " + owner)
	return f.guestErr
}

func (f *fakeEngine) EnableManaged(context.Context) error {
	f.record("enable managed")
	return f.networkErr
}

func (f *fakeEngine) Traffic() engine.TrafficView { return engine.TrafficView{} }

// The cluster: answered from the fields of the fake, and recorded.
func (f *fakeEngine) ClusterStatus() engine.ClusterStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := engine.ClusterStatus{Lease: f.state.Lease, Nodes: f.state.Nodes}
	if out.Nodes == nil {
		out.Nodes = []engine.NodeView{}
	}
	if f.state.Cluster != nil {
		out.Cluster = *f.state.Cluster
	}
	return out
}

func (f *fakeEngine) Nodes() []engine.NodeView {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state.Nodes == nil {
		return []engine.NodeView{}
	}
	return f.state.Nodes
}

func (f *fakeEngine) Handoff(_ context.Context, to string) (engine.HandoffResult, error) {
	f.record("handoff " + to)
	if f.clusterErr != nil {
		return engine.HandoffResult{}, f.clusterErr
	}
	return engine.HandoffResult{From: "pve1", To: to, Until: t0.Add(30 * time.Second)}, nil
}

func (f *fakeEngine) NewJoinToken(_ context.Context, node string) (engine.JoinTokenView, error) {
	f.record("join-token " + node)
	if f.clusterErr != nil {
		return engine.JoinTokenView{}, f.clusterErr
	}
	return engine.JoinTokenView{ID: "tok1", Node: node, Expires: t0.Add(15 * time.Minute), Token: "pco1.c2VjcmV0LXRva2Vu"}, nil
}

func (f *fakeEngine) FinishUpgrade(context.Context) (engine.FinishUpgradeResult, error) {
	f.record("finish-upgrade")
	if f.clusterErr != nil {
		return engine.FinishUpgradeResult{}, f.clusterErr
	}
	return engine.FinishUpgradeResult{From: "0.9.0", Schema: 1, Backup: "/var/lib/pco/backup/1-20261001T120000Z"}, nil
}

func (f *fakeEngine) Unblock(_ context.Context, addr netip.Addr) error {
	f.record("unblock " + addr.String())
	return f.clusterErr
}

func (f *fakeEngine) UninstallCluster(_ context.Context, force bool) (engine.UninstallResult, error) {
	f.record("uninstall-cluster force=" + boolText(force))
	if f.clusterErr != nil {
		return engine.UninstallResult{}, f.clusterErr
	}
	return engine.UninstallResult{Clean: []string{}, Left: []string{"pve2: pco uninstall --yes --keep-cloudflare removes what pco keeps on the node"}}, nil
}

func (f *fakeEngine) nodeNamed(name string) engine.NodeView {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.state.Nodes {
		if n.Name == name {
			return n
		}
	}
	return engine.NodeView{Name: name, State: "active", Kind: "host"}
}

func (f *fakeEngine) AdmitNode(_ context.Context, name, fingerprint string, renew bool) (engine.NodeView, error) {
	f.record("admit " + name + " fingerprint=" + fingerprint + " renew=" + boolText(renew))
	if f.clusterErr != nil {
		return engine.NodeView{}, f.clusterErr
	}
	return f.nodeNamed(name), nil
}

func (f *fakeEngine) DrainNode(_ context.Context, name string, force bool) (engine.NodeView, error) {
	f.record("drain " + name + " force=" + boolText(force))
	if f.clusterErr != nil {
		return engine.NodeView{}, f.clusterErr
	}
	v := f.nodeNamed(name)
	v.State, v.Drained = "draining", "admin"
	return v, nil
}

func (f *fakeEngine) UndrainNode(_ context.Context, name string) (engine.NodeView, error) {
	f.record("undrain " + name)
	if f.clusterErr != nil {
		return engine.NodeView{}, f.clusterErr
	}
	v := f.nodeNamed(name)
	v.State, v.Drained = "active", ""
	return v, nil
}

func (f *fakeEngine) ForgetNode(_ context.Context, name string, compromised bool) (engine.ForgetResult, error) {
	f.record("forget " + name + " compromised=" + boolText(compromised))
	if f.clusterErr != nil {
		return engine.ForgetResult{}, f.clusterErr
	}
	res := engine.ForgetResult{Node: name, Compromised: compromised, Done: []string{
		"the entry of the node is removed: its agent is refused from its next request",
		"its connections to this controller are closed",
		"its pinned tunnels are deleted in the next cycle",
	}}
	if compromised {
		res.Done = append(res.Done, "the rotation of every secret of the install began; pco cluster status follows it, and a controller that takes over finishes it")
		res.Pending = []string{"credentials", "proxmox", "tunnels", "rollout", "ca", "previous"}
		res.NotRotated = []string{"the corosync authkey (/etc/corosync/authkey): a node that keeps it can join the cluster again; pvecm updatecerts after a new key"}
	} else {
		res.Done = append(res.Done, "the secret of tunnel pco-abc123 in account 0123456789abcdef0123456789abcdef is rotated: the run token the node holds stops working, and the remaining replicas restart with the new one")
	}
	return res, nil
}

func (f *fakeEngine) SetNode(_ context.Context, name string, p engine.NodePatch) (engine.NodeView, error) {
	eligible := "unset"
	if p.ControllerEligible != nil {
		eligible = boolText(*p.ControllerEligible)
	}
	f.record("set " + name + " controllerEligible=" + eligible)
	if f.clusterErr != nil {
		return engine.NodeView{}, f.clusterErr
	}
	return f.nodeNamed(name), nil
}

func (f *fakeEngine) AgentView() (agentproto.AgentView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clusterErr != nil {
		return agentproto.AgentView{}, f.clusterErr
	}
	return f.agent, nil
}

func (f *fakeEngine) ResetAgentEpoch(context.Context) error {
	f.record("reset-epoch")
	return f.clusterErr
}

func (f *fakeEngine) RouteSeries(string) (engine.RouteSeries, error) {
	return engine.RouteSeries{}, engine.ErrNotFound
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// serveFake runs the API of the daemon over the engine on a socket of its own
// until the test ends, and returns the path of the socket.
func serveFake(t *testing.T, e *fakeEngine) string {
	t.Helper()
	socket := filepath.Join(testutil.ShortDir(t), "pco", "pco.sock")
	srv := api.New(e, "1.2.3", []uint32{uint32(os.Getuid())}, zerolog.Nop())
	// The commands are tested as root runs them, whoever runs the tests.
	srv.SetRootUID(uint32(os.Getuid()))
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	srv.OnListening(func() { close(ready) })
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, socket, os.Getgid()) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("the API ended before it listened: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return socket
}

// serveRaw runs a daemon that answers every request from replies, by path, with
// a status and a body; a path it has no reply for is an unknown route.
func serveRaw(t *testing.T, replies map[string]rawReply) string {
	t.Helper()
	socket := filepath.Join(testutil.ShortDir(t), "pco.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply, ok := replies[r.Method+" "+r.URL.Path]
		if !ok {
			reply = rawReply{status: http.StatusNotFound, body: `{"error":"no such route","code":"no_route"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return socket
}

type rawReply struct {
	status int
	body   string
}

// result is what a command did.
type result struct {
	out    string // standard output
	errOut string // standard error
	err    error  // what Execute returned, which main turns into the exit code
}

// runner runs the commands of pco against a socket.
type runner struct {
	t      *testing.T
	socket string
	env    env
}

func newRunner(t *testing.T, socket string) *runner {
	return &runner{t: t, socket: socket, env: testEnv()}
}

// run runs pco with args, reading stdin from in.
func (r *runner) run(in string, args ...string) result {
	r.t.Helper()
	return r.runReader(strings.NewReader(in), args...)
}

// runReader runs pco with args, reading stdin from in.
func (r *runner) runReader(in io.Reader, args ...string) result {
	r.t.Helper()
	cmd := newRootCmdWith(r.env)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(in)
	cmd.SetArgs(append([]string{"--socket", r.socket}, args...))
	err := cmd.ExecuteContext(r.t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

// tty makes stdin a terminal, which the commands that ask a question need.
func (r *runner) tty() *runner {
	r.env.stdinTerminal = func(io.Reader) (int, bool) { return 0, true }
	return r
}

// unreadable is a stdin that fails the test when it is read.
type unreadable struct{ t *testing.T }

func (u unreadable) Read([]byte) (int, error) {
	u.t.Helper()
	u.t.Fatal("stdin was read")
	return 0, io.EOF
}

// requireGolden compares got with a file of testdata; run the tests with
// -update to write them.
func requireGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got, "the output changed; run the test with -update when that is intended")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
