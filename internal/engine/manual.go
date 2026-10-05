package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/annotation"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The kinds of ManualTarget.Kind.
const (
	TargetGuest   = "guest"
	TargetAddress = "address"
)

// manualID is what the id of a manual route may be. A route made without one
// gets eight random hex digits.
var manualID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ManualTarget is where a manual route sends traffic: a guest, whose address
// is proven as that of a route in its Notes, or an address.
type ManualTarget struct {
	Kind   string     `json:"kind"` // "guest" or "address"
	Guest  string     `json:"guest,omitempty"`
	Scheme string     `json:"scheme"`
	Addr   netip.Addr `json:"addr,omitzero"`
	Port   uint16     `json:"port"`
}

// ManualRouteView is a route an admin made, with the revision of its file.
// It owns its hostname as manual/<id>.
type ManualRouteView struct {
	ID       string             `json:"id"`
	Rev      int                `json:"rev"`
	Hostname string             `json:"hostname"`
	Target   ManualTarget       `json:"target"`
	Options  model.RouteOptions `json:"options"`
}

// ManualRoutes returns the manual routes by id.
func (e *Engine) ManualRoutes() ([]ManualRouteView, error) {
	routes, err := e.d.Store.ManualRoutesRev()
	if err != nil {
		return nil, fmt.Errorf("reading the manual routes: %w", err)
	}
	out := make([]ManualRouteView, 0, len(routes))
	for _, r := range routes {
		out = append(out, manualView(r.Route, r.Rev))
	}
	slices.SortFunc(out, func(a, b ManualRouteView) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func manualView(r model.Route, rev int) ManualRouteView {
	v := ManualRouteView{
		ID: r.ManualID, Rev: rev, Hostname: r.Hostname, Options: r.Options,
		Target: ManualTarget{Kind: TargetAddress, Scheme: string(r.Target.Scheme), Addr: r.Target.Addr, Port: r.Target.Port},
	}
	if r.Guest != nil {
		v.Target.Kind, v.Target.Guest = TargetGuest, r.Guest.String()
	}
	return v
}

// CreateManualRoute makes a manual route of v, under v.ID or, when it has
// none, an id of its own. An id that is taken is refused with ErrRefused.
func (e *Engine) CreateManualRoute(ctx context.Context, v ManualRouteView) (ManualRouteView, error) {
	if v.ID == "" {
		v.ID = randomHex(4)()
	}
	return e.writeManual(ctx, v, 0, "made")
}

// UpdateManualRoute replaces the manual route id, which was read at revision
// rev, with v; a route that has another revision now is refused with
// ErrRefused. The id of a route does not change.
func (e *Engine) UpdateManualRoute(ctx context.Context, id string, rev int, v ManualRouteView) (ManualRouteView, error) {
	if v.ID != "" && v.ID != id {
		return ManualRouteView{}, fieldError("id", "id %q: the id of a manual route does not change; make a new one", v.ID)
	}
	v.ID = id
	if rev < 1 {
		return ManualRouteView{}, fieldError("rev", "rev %d: give the revision the route was read at", rev)
	}
	return e.writeManual(ctx, v, rev, "changed")
}

// writeManual checks v and writes it at revision rev, 0 for a new route.
func (e *Engine) writeManual(ctx context.Context, v ManualRouteView, rev int, done string) (ManualRouteView, error) {
	r, err := manualRoute(v)
	if err != nil {
		return ManualRouteView{}, err
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return ManualRouteView{}, err
	}
	defer e.release()

	if err := e.checkManualAddr(r); err != nil {
		return ManualRouteView{}, err
	}
	old, found, err := e.storedManual(r.ManualID)
	switch {
	case err != nil:
		return ManualRouteView{}, err
	case rev == 0 && found:
		return ManualRouteView{}, fmt.Errorf("%w: the id %s is taken by %s; choose another one", ErrRefused, r.Owner(), old.Route.Hostname)
	case rev > 0 && !found:
		return ManualRouteView{}, fmt.Errorf("%w: there is no manual route %s", ErrNotFound, r.Owner())
	case rev > 0 && old.Rev != rev:
		return ManualRouteView{}, staleManual(r.ManualID, rev)
	}
	now, err := e.d.Store.SaveManualRouteIf(rev, r)
	switch {
	case errors.Is(err, store.ErrRevision):
		return ManualRouteView{}, staleManual(r.ManualID, rev)
	case err != nil:
		return ManualRouteView{}, fmt.Errorf("saving the manual route: %w", err)
	}
	e.manualEvent(ctx, r, done)
	e.Trigger()
	return manualView(r, now), nil
}

// DeleteManualRoute removes the manual route id, which was read at revision
// rev; a route that has another revision now is refused with ErrRefused.
func (e *Engine) DeleteManualRoute(ctx context.Context, id string, rev int) error {
	if err := e.acquireAdmin(ctx); err != nil {
		return err
	}
	defer e.release()

	old, found, err := e.storedManual(id)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: there is no manual route %s", ErrNotFound, model.ManualPrefix+id)
	case old.Rev != rev:
		return staleManual(id, rev)
	}
	err = e.d.Store.DeleteManualRouteIf(id, rev)
	switch {
	case errors.Is(err, store.ErrRevision):
		return staleManual(id, rev)
	case err != nil:
		return fmt.Errorf("removing the manual route: %w", err)
	}
	e.manualEvent(ctx, old.Route, "removed")
	e.Trigger()
	return nil
}

// storedManual returns the manual route id as it is stored, if there is one.
func (e *Engine) storedManual(id string) (store.ManualRoute, bool, error) {
	routes, err := e.d.Store.ManualRoutesRev()
	if err != nil {
		return store.ManualRoute{}, false, fmt.Errorf("reading the manual routes: %w", err)
	}
	i := slices.IndexFunc(routes, func(r store.ManualRoute) bool { return r.Route.ManualID == id })
	if i < 0 {
		return store.ManualRoute{}, false, nil
	}
	return routes[i], true, nil
}

func staleManual(id string, rev int) error {
	return fmt.Errorf("%w: the manual route %s changed since it was read at revision %d; read it again",
		ErrRefused, model.ManualPrefix+id, rev)
}

// manualEvent records an admin action on a manual route.
func (e *Engine) manualEvent(ctx context.Context, r model.Route, done string) {
	e.events.add(Event{
		At: e.d.Now(), Level: levelInfo, Kind: kindAdmin, Subject: r.Owner(), Route: r.Hostname, Actor: ActorOf(ctx),
		Message: fmt.Sprintf("manual route %s is %s: %s -> %s", r.Owner(), done, r.Hostname, targetText(r)),
	})
}

// targetText writes the target of a manual route: http://10.0.5.20:9000, or
// https://qemu/101:8443 for a guest.
func targetText(r model.Route) string {
	host := r.Target.Addr.String()
	if r.Guest != nil {
		host = r.Guest.String()
	}
	return fmt.Sprintf("%s://%s:%d", r.Target.Scheme, host, r.Target.Port)
}

// manualRoute checks what a view says of a route by itself and returns the
// route it describes, with its hostname and options in normal form.
func manualRoute(v ManualRouteView) (model.Route, error) {
	if !manualID.MatchString(v.ID) {
		return model.Route{}, fieldError("id", "id %q: want 1 to 32 of a-z, 0-9 and -", v.ID)
	}
	host, err := hostname.Normalize(v.Hostname)
	if err != nil {
		return model.Route{}, &FieldError{Field: "hostname", Err: err}
	}
	r := model.Route{Hostname: host, Source: model.SourceManual, ManualID: v.ID}
	if r.Target, r.Guest, err = manualTarget(v.Target); err != nil {
		return model.Route{}, err
	}
	if v.Options.AllowNode && r.Guest != nil {
		return model.Route{}, fieldError("options.allowNode", "allowNode: only a route to an address may point at a node")
	}
	if r.Options, err = annotation.CheckOptions(r.Target, v.Options); err != nil {
		var oe *annotation.OptionError
		if errors.As(err, &oe) {
			return model.Route{}, fieldError("options."+oe.Option, "options.%s", oe)
		}
		return model.Route{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return r, nil
}

func manualTarget(t ManualTarget) (model.Target, *model.GuestRef, error) {
	out := model.Target{Scheme: model.Scheme(t.Scheme), Port: t.Port}
	switch out.Scheme {
	case model.SchemeHTTP, model.SchemeHTTPS:
	default:
		return out, nil, fieldError("target.scheme", "target.scheme %q: want http or https", t.Scheme)
	}
	if t.Port == 0 {
		return out, nil, fieldError("target.port", "target.port: want a port from 1 to 65535")
	}
	switch t.Kind {
	case TargetGuest:
		ref, err := model.ParseGuestRef(t.Guest)
		if err != nil {
			return out, nil, &FieldError{Field: "target.guest", Err: err}
		}
		if t.Addr.IsValid() {
			return out, nil, fieldError("target.addr", "target.addr: a route to a guest goes to the address proven for it, and takes none")
		}
		return out, &ref, nil
	case TargetAddress:
		if t.Guest != "" {
			return out, nil, fieldError("target.guest", "target.guest: a route to an address names no guest")
		}
		if !t.Addr.Is4() {
			return out, nil, fieldError("target.addr", "target.addr %v: want an IPv4 address", t.Addr)
		}
		out.Addr = t.Addr
		return out, nil, nil
	}
	return out, nil, fieldError("target.kind", "target.kind %q: want guest or address", t.Kind)
}

// checkManualAddr checks the address of a route to one against the trusted
// prefixes of the settings and, unless the route has allowNode, against the
// addresses of the nodes. The caller holds the cycle lock.
func (e *Engine) checkManualAddr(r model.Route) error {
	addr := r.Target.Addr
	if !addr.IsValid() {
		return nil
	}
	s, err := e.d.Store.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	if !slices.ContainsFunc(s.TrustedCIDRs, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return fieldError("target.addr", "target.addr %s: not inside the trusted prefixes of the settings (trustedCIDRs: %s)",
			addr, prefixesText(s.TrustedCIDRs))
	}
	if r.Options.AllowNode {
		return nil
	}
	nodes, err := e.knownNodeAddrs()
	if err != nil {
		return err
	}
	if slices.Contains(nodes, addr) {
		return fieldError("target.addr", "target.addr %s: an address of a node; a route to a service of the node needs allowNode", addr)
	}
	return nil
}

// knownNodeAddrs are the addresses of the nodes the cycles have seen, or
// those saved when no cycle has run yet. The caller holds the cycle lock.
func (e *Engine) knownNodeAddrs() ([]netip.Addr, error) {
	if e.addrs.loaded {
		return e.addrs.list, nil
	}
	addrs, err := e.d.Store.NodeAddrs()
	if err != nil {
		return nil, fmt.Errorf("reading the saved node addresses: %w", err)
	}
	return addrs, nil
}

func prefixesText(ps []netip.Prefix) string {
	if len(ps) == 0 {
		return "none"
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.String()
	}
	return strings.Join(out, ", ")
}
