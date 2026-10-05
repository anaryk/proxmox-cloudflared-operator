package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// noStateEngine answers the API of an appliance whose volume holds no state
// yet, or none it can use (ruling 19): its state is the profile, the line that
// says what to do and when the volume was last looked at, and every action is
// refused with that line. It changes nothing anywhere.
type noStateEngine struct {
	boot     string
	interval time.Duration

	mu   sync.Mutex
	line string
	at   time.Time
}

func newNoStateEngine(line string, at time.Time, retry time.Duration) *noStateEngine {
	return &noStateEngine{boot: randomBoot(), interval: retry, line: line, at: at}
}

// look says what the last look at the volume found.
func (n *noStateEngine) look(line string, at time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.line, n.at = line, at
}

func (n *noStateEngine) said() (string, time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.line, n.at
}

// refusal is the error of every action: the line, which the API answers as a
// refusal.
type refusal struct{ line string }

func (r refusal) Error() string        { return r.line }
func (r refusal) Is(target error) bool { return target == engine.ErrRefused }

func (n *noStateEngine) refuse() error {
	line, _ := n.said()
	return refusal{line}
}

func (n *noStateEngine) State() engine.State {
	line, at := n.said()
	return engine.State{
		At: at, FinishedAt: at, Mode: engine.ModeObserve, WriterVerdict: engine.VerdictUnknown, Profile: store.ProfileAppliance,
		Routes: []engine.RouteView{}, Issues: []planner.Issue{}, Tunnels: []engine.TunnelView{}, Connectors: []connector.Status{},
		Credentials: []engine.CredentialView{}, Zones: []engine.ZoneView{}, Actions: []reconcile.Action{}, Conflicts: []reconcile.Conflict{},
		Lost: []string{}, Problems: []string{line}, Hold: line, Waiting: []engine.Waiting{}, Unapproved: []engine.UnapprovedGuest{},
		Segments: []engine.SegmentView{}, RogueConnectors: []engine.RogueConnector{},
	}
}

// randomBoot names this process to the clients of its API.
func randomBoot() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (n *noStateEngine) Boot() string { return n.boot }

func (n *noStateEngine) RunsAs() (profile, node string) { return store.ProfileAppliance, "" }

func (n *noStateEngine) QueryEvents(engine.EventQuery) ([]engine.Event, error) {
	return []engine.Event{}, nil
}

// Subscribe answers a stream that has nothing to tell until ctx ends.
func (n *noStateEngine) Subscribe(ctx context.Context, _ string, _ uint64) (<-chan engine.Notice, engine.Hello, error) {
	ch := make(chan engine.Notice)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, engine.Hello{Boot: n.boot, PollInterval: n.interval.String()}, nil
}

func (n *noStateEngine) Trigger() {}

func (n *noStateEngine) Apply(context.Context, bool, string) (engine.ApplyResult, error) {
	return engine.ApplyResult{}, n.refuse()
}

func (n *noStateEngine) Adopt(context.Context, string) error { return n.refuse() }

func (n *noStateEngine) RotateTunnel(context.Context, string) (engine.TunnelRotation, error) {
	return engine.TunnelRotation{}, n.refuse()
}

func (n *noStateEngine) AddCredential(context.Context, string, string) (engine.CredentialView, error) {
	return engine.CredentialView{}, n.refuse()
}

func (n *noStateEngine) Credentials() ([]engine.CredentialView, error) { return nil, n.refuse() }

func (n *noStateEngine) CheckCredential(context.Context, string, bool) (engine.CredentialView, error) {
	return engine.CredentialView{}, n.refuse()
}

func (n *noStateEngine) RemoveCredential(context.Context, string) error { return n.refuse() }

func (n *noStateEngine) Claims() ([]engine.ClaimView, error) { return nil, n.refuse() }

func (n *noStateEngine) ResolveClaim(context.Context, string, string) error { return n.refuse() }

func (n *noStateEngine) Approvals() ([]engine.ApprovalView, error) { return nil, n.refuse() }

func (n *noStateEngine) ApproveGuest(context.Context, string, string, []string, []netip.Addr) (engine.Approval, error) {
	return engine.Approval{}, n.refuse()
}

func (n *noStateEngine) RevokeGuest(context.Context, string) error { return n.refuse() }

func (n *noStateEngine) Segments() ([]engine.SegmentView, error) { return nil, n.refuse() }

func (n *noStateEngine) AcknowledgeSegment(context.Context, string, int) error { return n.refuse() }

func (n *noStateEngine) RevokeSegment(context.Context, string, int) error { return n.refuse() }

func (n *noStateEngine) Diagnose(context.Context, string) ([]doctor.Step, error) {
	return nil, n.refuse()
}

// Doctor finds the one thing there is to find.
func (n *noStateEngine) Doctor(context.Context) []doctor.Finding {
	line, _ := n.said()
	return []doctor.Finding{{Check: "state", Level: doctor.LevelFail, Detail: line}}
}

func (n *noStateEngine) PollInterval() time.Duration { return n.interval }

// Traffic is none: no connector runs without a state.
func (n *noStateEngine) Traffic() engine.TrafficView {
	return engine.TrafficView{Tunnels: []engine.TunnelTraffic{}, Routes: []engine.RouteTraffic{}, RoutesWhy: n.refuse().Error()}
}

func (n *noStateEngine) RouteSeries(string) (engine.RouteSeries, error) {
	return engine.RouteSeries{}, n.refuse()
}

func (n *noStateEngine) SettingsView() (engine.SettingsView, error) {
	return engine.SettingsView{}, n.refuse()
}

func (n *noStateEngine) SaveSettings(context.Context, int, store.Settings) (engine.SettingsView, []string, error) {
	return engine.SettingsView{}, nil, n.refuse()
}

func (n *noStateEngine) ManualRoutes() ([]engine.ManualRouteView, error) { return nil, n.refuse() }

func (n *noStateEngine) CreateManualRoute(context.Context, engine.ManualRouteView) (engine.ManualRouteView, error) {
	return engine.ManualRouteView{}, n.refuse()
}

func (n *noStateEngine) UpdateManualRoute(context.Context, string, int, engine.ManualRouteView) (engine.ManualRouteView, error) {
	return engine.ManualRouteView{}, n.refuse()
}

func (n *noStateEngine) DeleteManualRoute(context.Context, string, int) error { return n.refuse() }

func (n *noStateEngine) Guests() ([]engine.GuestListView, error) { return nil, n.refuse() }

func (n *noStateEngine) Annotation(model.GuestRef) (engine.AnnotationView, error) {
	return engine.AnnotationView{}, n.refuse()
}

func (n *noStateEngine) RequestRestart(context.Context) error { return n.refuse() }
