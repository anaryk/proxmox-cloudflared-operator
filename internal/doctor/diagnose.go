package doctor

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Step is one link of a route's chain.
type Step struct {
	Name   string `json:"name"` // "route", "zone", "dns", "ingress", "connector", "identity", "path", "tcp", "http"
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	// Skipped says that an earlier step failed; Level stays "warn" and
	// Detail "skipped", as clients that came before it read them.
	Skipped bool `json:"skipped,omitempty"`
}

// DiagnoseRoute walks the chain of the route that holds a hostname in st:
// the route and its state, its zone, its DNS record, its ingress rule in the
// tunnel's verified configuration, the connector, the identity of its target,
// the path to it, directly or through the leg of a network of the gateway,
// the port, and last an HTTP(S) request to that target as the tunnel would
// make it. The first step that fails leaves the steps after it skipped, but
// for the connector's, whose failure is a finding of its own.
//
// The request goes to nothing but the address and port of the route's rule,
// which for a target behind a leg is its service address, and only when the
// state shows the target verified for the route's owner and its port
// answering; the Host header, the name TLS asks for and the verification are
// those of the rule. It follows no redirect, ends after diagnoseTimeout and
// reads at most maxBodyRead bytes, none of which are returned: only the
// status, a hint and the class of a TLS failure are.
//
// httpc only lends the certificate authorities it trusts; nil trusts those of
// the system. An unknown hostname is engine.ErrNotFound.
func DiagnoseRoute(ctx context.Context, st engine.State, name string, httpc *http.Client) ([]Step, error) {
	return diagnoseRoute(ctx, st, name, httpc, nil)
}

// pinsRead are the pins of the gateway as the diagnosis read them: what a
// path through a leg is checked against.
type pinsRead struct {
	pins []store.Pin
	err  error
}

// readPins reads the pins of the gateway where env can tell them; nil where
// it cannot, which leaves them out of the diagnosis.
func readPins(ctx context.Context, env Env) *pinsRead {
	net, ok := env.(NetworkEnv)
	if !ok {
		return nil
	}
	pins, _, err := net.Pins(ctx)
	return &pinsRead{pins: pins, err: err}
}

// diagnoseRoute is DiagnoseRoute with the pins of the gateway, when read.
func diagnoseRoute(ctx context.Context, st engine.State, name string, httpc *http.Client, pins *pinsRead) ([]Step, error) {
	host, err := hostname.Normalize(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrInvalid, err)
	}
	rt, found := HolderOf(st, host)
	if !found {
		return nil, fmt.Errorf("%w: the last cycle has no route for %s", engine.ErrNotFound, host)
	}
	d := &diagnosis{st: st, rt: rt, host: host, pins: pins}
	if i := slices.IndexFunc(st.Tunnels, func(t engine.TunnelView) bool { return rt.Account != "" && t.AccountID == rt.Account }); i >= 0 {
		d.tunnel = &st.Tunnels[i]
	}
	// A connector that fails skips nothing: the steps after it ask the
	// target from the node, which needs no connector.
	links := []struct {
		name  string
		check func() Step
		alone bool
	}{
		{"route", d.route, false}, {"zone", d.zone, false}, {"dns", d.dns, false}, {"ingress", d.ingress, false},
		{"connector", d.connector, true}, {"identity", d.identity, false}, {"path", d.path, false}, {"tcp", d.tcp, false},
		{"http", func() Step { return d.http(ctx, httpc) }, false},
	}
	steps := make([]Step, 0, len(links))
	failed := false
	for _, l := range links {
		if failed {
			steps = append(steps, Step{Name: l.name, Level: LevelWarn, Detail: "skipped", Skipped: true})
			continue
		}
		s := l.check()
		s.Name = l.name
		failed = s.Level == LevelFail && !l.alone
		steps = append(steps, s)
	}
	return steps, nil
}

// HolderOf returns the route of host that did not lose it to another owner,
// or, failing that, any route of host: the route DiagnoseRoute walks.
func HolderOf(st engine.State, host string) (engine.RouteView, bool) {
	var fallback *engine.RouteView
	for i, r := range st.Routes {
		if !strings.EqualFold(r.Hostname, host) {
			continue
		}
		if r.State != planner.StateConflict {
			return r, true
		}
		if fallback == nil {
			fallback = &st.Routes[i]
		}
	}
	if fallback == nil {
		return engine.RouteView{}, false
	}
	return *fallback, true
}

type diagnosis struct {
	st     engine.State
	rt     engine.RouteView
	host   string
	tunnel *engine.TunnelView // of the route's account, when the state has it
	pins   *pinsRead          // nil when they were not read
}

func passed(detail string) Step { return Step{Level: LevelOK, Detail: detail} }
func warned(detail string) Step { return Step{Level: LevelWarn, Detail: detail} }
func failed(detail string) Step { return Step{Level: LevelFail, Detail: detail} }

func (d *diagnosis) route() Step {
	who := engine.OwnerName(d.rt.Owner, d.rt.Guest)
	switch d.rt.State {
	case planner.StateConflict:
		return failed(d.rt.Reason)
	case planner.StateHeld:
		return failed(fmt.Sprintf("%s holds it, but nobody serves it: %s", who, d.rt.Reason))
	case planner.StateRejected:
		return failed(fmt.Sprintf("%s holds it, but it is not published: %s", who, d.rt.Reason))
	}
	detail := fmt.Sprintf("%s holds it; state %s", who, d.rt.State)
	if d.rt.Reason != "" {
		detail += ": " + d.rt.Reason
	}
	return passed(detail)
}

func (d *diagnosis) zone() Step {
	switch {
	case d.rt.State == planner.StateNoZone, d.rt.State == engine.RouteFrozen:
		return failed(cmp.Or(d.rt.Reason, "its zone cannot be served"))
	case d.rt.Zone == "":
		return failed("it is in no zone pco serves")
	}
	return passed("zone " + d.rt.Zone + " is active")
}

func (d *diagnosis) same(name string) bool {
	return strings.EqualFold(strings.TrimSuffix(name, "."), d.host)
}

// unchecked says why the last cycle did not check the route's tunnel, and is
// empty when it did. What the state shows of the records, the rules and the
// connectors is then not what is there now.
func (d *diagnosis) unchecked() string {
	if d.tunnel != nil && d.tunnel.Unchecked {
		return cmp.Or(d.tunnel.Held, notChecked)
	}
	return ""
}

func (d *diagnosis) dns() Step {
	if why := d.unchecked(); why != "" {
		return warned(why)
	}
	if i := slices.IndexFunc(d.st.Conflicts, func(c reconcile.Conflict) bool { return d.same(c.Name) }); i >= 0 {
		c := d.st.Conflicts[i]
		return failed(fmt.Sprintf("a record of someone else holds the name in zone %s: %s %s; pco adopt %s replaces it", c.Zone, c.Type, c.Content, d.host))
	}
	if slices.ContainsFunc(d.st.Lost, d.same) {
		return failed(fmt.Sprintf("its record points at the tunnel but lost the marker of this install; pco adopt %s takes it back", d.host))
	}
	for _, a := range d.st.Actions {
		if (a.Kind == reconcile.CreateRecord || a.Kind == reconcile.UpdateRecord) && !a.Applied && d.same(a.Target) {
			return failed(fmt.Sprintf("%s is not applied: %s", a.Kind, cmp.Or(a.Held, "it waits for the next run")))
		}
	}
	if d.rt.Service == "" && d.rt.State != planner.StateWithdrawn {
		return warned("no record is published for it while its target is not served")
	}
	return passed("its record points at the tunnel")
}

func (d *diagnosis) ingress() Step {
	r, t := d.rt.Rule, d.tunnel
	switch {
	case r == nil || d.rt.Account == "":
		return failed("the plan has no rule for it")
	case d.unchecked() != "":
		return warned(d.unchecked())
	case t == nil || !t.Exists && !t.Unknown:
		return failed(fmt.Sprintf("the tunnel of account %s does not exist yet", d.rt.Account))
	case t.Unknown:
		return failed(fmt.Sprintf("the state of the tunnel of account %s is not known", d.rt.Account))
	case t.Held != "":
		return failed(fmt.Sprintf("tunnel %s in account %s is left as it is: %s", t.Name, t.AccountID, t.Held))
	case !t.Verified:
		return failed(fmt.Sprintf("the configuration of tunnel %s is not verified: the last write was held or failed (pco plan shows why)", t.Name))
	case r.Service == planner.BlockedService && d.targetIsTheCause():
		return warned(fmt.Sprintf("tunnel %s answers 503 for it while its target is not served", t.Name))
	case r.Service == planner.BlockedService:
		return failed(fmt.Sprintf("tunnel %s answers 503 for it", t.Name))
	}
	return passed(fmt.Sprintf("tunnel %s sends it to %s (configuration version %d)", t.Name, r.Service, t.Version))
}

// targetIsTheCause reports whether the route is not served because of its
// target, which the identity and tcp steps tell more of.
func (d *diagnosis) targetIsTheCause() bool {
	return d.rt.State == planner.StateUnreachable || d.rt.State == planner.StateWithdrawn
}

func (d *diagnosis) connector() Step {
	if why := d.unchecked(); why != "" {
		return warned(why)
	}
	t := d.tunnel
	if t == nil || t.ID == "" {
		return failed("the tunnel has no id yet")
	}
	i := slices.IndexFunc(d.st.Connectors, func(c connector.Status) bool { return c.TunnelID == t.ID })
	switch {
	case i < 0:
		return failed(fmt.Sprintf("no connector runs for tunnel %s", t.Name))
	case !d.st.Connectors[i].Active:
		return failed(fmt.Sprintf("the connector of tunnel %s is not running", t.Name))
	case !d.st.Connectors[i].Ready:
		return failed(fmt.Sprintf("the connector of tunnel %s is not connected to Cloudflare", t.Name))
	}
	return passed(d.st.Connectors[i].Text())
}

func (d *diagnosis) identity() Step {
	switch {
	case d.rt.State == planner.StateWithdrawn:
		return failed(cmp.Or(d.rt.Reason, "the identity check failed"))
	case d.rt.Service == "":
		return failed(cmp.Or(d.rt.Reason, "no verified address") + tried(d.rt.Candidates))
	}
	_, ap, err := parseService(d.rt.Service)
	if err != nil {
		return failed(fmt.Sprintf("the target %s is no address and port", d.rt.Service))
	}
	level := ""
	if d.rt.Level != "" {
		level = " at identity level " + d.rt.Level
	}
	// What a cycle that did not check the tunnel shows is what an earlier
	// one verified.
	verified, step := "verified", passed
	if d.unchecked() != "" {
		verified, step = "verified in an earlier cycle", warned
	}
	addr := d.behind(ap.Addr())
	if c, ok := d.candidate(addr); ok {
		return step(fmt.Sprintf("%s is the address of %s, %s%s (%s)", addr, d.rt.Owner, verified, level, c.Source))
	}
	return step(fmt.Sprintf("%s is the address %s for %s%s", addr, verified, d.rt.Owner, level))
}

// behind is the address of the target the connector reaches at dialed: the
// guest's own address behind the leg when dialed is the service address of
// the route's path, and dialed otherwise.
func (d *diagnosis) behind(dialed netip.Addr) netip.Addr {
	if p := d.rt.Path; p != nil && p.Service.IsValid() && p.Service == dialed && p.Addr.IsValid() {
		return p.Addr
	}
	return dialed
}

// gatewayFailed is the start of the problem line of a cycle whose gateway
// could not be programmed.
const gatewayFailed = "programming the gateway: "

// path says how the node reaches the target: directly, or through the leg of
// a network of the gateway, which forwards the service address the connector
// dials to the guest's own address.
func (d *diagnosis) path() Step {
	p := d.rt.Path
	if p == nil || p.Via == "" || p.Via == engine.ViaDirect {
		return passed("direct")
	}
	via := strings.Replace(p.Via, ":", " ", 1)
	switch {
	case !p.Service.IsValid():
		return failed(via + ": no service address")
	case p.Leg != engine.LegUp:
		return failed(fmt.Sprintf("%s: the leg is not up: %s", via, cmp.Or(p.Leg, "the gateway has not said how it is")))
	case slices.ContainsFunc(d.st.Problems, func(l string) bool { return strings.HasPrefix(l, gatewayFailed) }):
		return failed(via + ": the gateway could not be programmed; pco status says why")
	}
	done := fmt.Sprintf("%s: leg up, service %s -> %s programmed", via, p.Service, p.Addr)
	if d.pins == nil {
		return passed(done)
	}
	if d.pins.err != nil {
		return warned(done + "; the pins of the gateway could not be read: " + d.pins.err.Error())
	}
	network := strings.TrimPrefix(p.Via, engine.ViaAttach)
	if p.Via == engine.ViaManaged {
		network = gateway.ManagedBridge
	}
	i := slices.IndexFunc(d.pins.pins, func(pin store.Pin) bool { return pin.Network == network && pin.Addr == p.Addr })
	switch {
	case i < 0:
		return warned(fmt.Sprintf("%s, but no pin holds %s to %s: another guest that answers for the address may take its requests; the next cycle pins it",
			done, p.Addr, p.MAC))
	case !strings.EqualFold(d.pins.pins[i].MAC, p.MAC):
		return failed(fmt.Sprintf("%s: %s is pinned to %s, and the last proof found it at %s; the next cycle pins it again", via, p.Addr, d.pins.pins[i].MAC, p.MAC))
	}
	return passed(fmt.Sprintf("%s, %s pinned to %s", done, p.Addr, p.MAC))
}

// tried lists the candidates resolution tried, and how each fared.
func tried(cands []resolve.CandidateResult) string {
	if len(cands) == 0 {
		return ""
	}
	parts := make([]string, len(cands))
	for i, c := range cands {
		parts[i] = fmt.Sprintf("%s (%s): %s", c.Addr, c.Source, cmp.Or(c.Reason, outcome(c.OK)))
	}
	return "; tried " + strings.Join(parts, ", ")
}

func outcome(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}

func (d *diagnosis) candidate(addr netip.Addr) (resolve.CandidateResult, bool) {
	i := slices.IndexFunc(d.rt.Candidates, func(c resolve.CandidateResult) bool { return c.Addr == addr })
	if i < 0 {
		return resolve.CandidateResult{}, false
	}
	return d.rt.Candidates[i], true
}

// notAsked is what the tcp and http steps say of a target from a cycle that
// did not check Cloudflare.
const notAsked = "not asked: the target is from an earlier cycle"

func (d *diagnosis) tcp() Step {
	if d.unchecked() != "" {
		return warned(notAsked)
	}
	_, ap, err := parseService(d.rt.Service)
	if err != nil {
		return failed(fmt.Sprintf("the target %s is no address and port", d.rt.Service))
	}
	addr := d.behind(ap.Addr())
	c, ok := d.candidate(addr)
	switch {
	case ok && c.OK && addr != ap.Addr():
		return passed(fmt.Sprintf("%s answers (%s through the leg)", ap, netip.AddrPortFrom(addr, ap.Port())))
	case ok && c.OK:
		return passed(ap.String() + " answers")
	case ok:
		return failed(cmp.Or(c.Reason, d.rt.Reason, "the port does not answer"))
	}
	return failed(cmp.Or(d.rt.Reason, "the last cycle did not try "+ap.String()))
}
