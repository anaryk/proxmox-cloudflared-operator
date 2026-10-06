package apifake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/annotation"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// SettingsView returns the settings with their revision.
func (e *Engine) SettingsView() (engine.SettingsView, error) {
	if err := e.begin(context.Background(), "SettingsView", noArgs); err != nil {
		return engine.SettingsView{}, err
	}
	s, rev, notes, err := e.store.LoadSettingsRev()
	if err != nil {
		return engine.SettingsView{}, err
	}
	return e.settingsView(s, rev, notes), nil
}

func (e *Engine) settingsView(s store.Settings, rev int, notes []string) engine.SettingsView {
	limits := map[string]engine.Limit{}
	for name, l := range store.Limits() {
		limits[name] = engine.Limit{Min: l.Min, Max: l.Max}
	}
	return engine.SettingsView{
		Rev: rev, Settings: s, ReadAtStart: nonNil(slices.Clone(e.readAtStart)), Limits: limits,
		Notes: nonNil(slices.Concat(e.settingNotes, notes)),
	}
}

// SaveSettings saves settings read at revision rev, as the store of the
// daemon checks them. The mode and the admission of the state follow them.
func (e *Engine) SaveSettings(ctx context.Context, rev int, s store.Settings) (engine.SettingsView, []string, error) {
	if err := e.begin(ctx, "SaveSettings", struct {
		Rev      int            `json:"rev"`
		Settings store.Settings `json:"settings"`
	}{rev, s}); err != nil {
		return engine.SettingsView{}, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	old, have, _, err := e.store.LoadSettingsRev()
	switch {
	case err != nil:
		return engine.SettingsView{}, nil, err
	case have != rev:
		return engine.SettingsView{}, nil, staleSettings(rev, fmt.Sprintf(" and are at revision %d now", have))
	case old.ObserveOnly && !s.ObserveOnly:
		return engine.SettingsView{}, nil, &engine.FieldError{Field: "observeOnly", Err: errors.New("observeOnly: leaving observe-only mode is not a setting, " +
			"so that what it changes is shown first; use apply (pco apply)")}
	}
	_, err = e.store.SaveSettingsIf(rev, s)
	var invalid *store.FieldError
	switch {
	case errors.As(err, &invalid):
		return engine.SettingsView{}, nil, &engine.FieldError{Field: invalid.Field, Err: invalid.Err}
	case errors.Is(err, store.ErrRevision):
		return engine.SettingsView{}, nil, staleSettings(rev, "")
	case err != nil:
		return engine.SettingsView{}, nil, err
	}
	saved, now, notes, err := e.store.LoadSettingsRev()
	if err != nil {
		return engine.SettingsView{}, nil, err
	}
	restart := slices.DeleteFunc(changedFields(e.started, saved), func(name string) bool { return !slices.Contains(e.readAtStart, name) })
	changes := changedFields(old, saved)
	msg := "the settings are saved; nothing changed"
	if len(changes) > 0 {
		msg = "the settings are saved: " + strings.Join(changes, ", ") + " changed"
	}
	if len(restart) > 0 {
		msg += fmt.Sprintf("; %s take effect once pco is restarted", strings.Join(restart, ", "))
	}
	e.adminEvent(ctx, "", msg)
	e.state.Mode, e.state.Admission = modeOf(saved), saved.Admission
	e.changed()
	return e.settingsView(saved, now, notes), restart, nil
}

func staleSettings(rev int, now string) error {
	return fmt.Errorf("%w: the settings changed since they were read at revision %d%s; read them again", engine.ErrRefused, rev, now)
}

func modeOf(s store.Settings) string {
	if s.ObserveOnly {
		return engine.ModeObserve
	}
	return engine.ModeEnforce
}

// changedFields names the settings in which b differs from a, by their JSON
// names in order.
func changedFields(a, b store.Settings) []string {
	before, after := settingsFields(a), settingsFields(b)
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	out := []string{}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if string(before[name]) != string(after[name]) {
			out = append(out, name)
		}
	}
	return out
}

func settingsFields(s store.Settings) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	data, err := json.Marshal(s)
	if err == nil {
		err = json.Unmarshal(data, &out)
	}
	if err != nil {
		return map[string]json.RawMessage{}
	}
	return out
}

// ManualRoutes returns the manual routes by id.
func (e *Engine) ManualRoutes() ([]engine.ManualRouteView, error) {
	if err := e.begin(context.Background(), "ManualRoutes", noArgs); err != nil {
		return nil, err
	}
	routes, err := e.store.ManualRoutesRev()
	if err != nil {
		return nil, err
	}
	out := make([]engine.ManualRouteView, 0, len(routes))
	for _, r := range routes {
		out = append(out, manualView(r.Route, r.Rev))
	}
	slices.SortFunc(out, func(a, b engine.ManualRouteView) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// CreateManualRoute makes a manual route, under an id of its own when v has
// none.
func (e *Engine) CreateManualRoute(ctx context.Context, v engine.ManualRouteView) (engine.ManualRouteView, error) {
	if err := e.begin(ctx, "CreateManualRoute", v); err != nil {
		return engine.ManualRouteView{}, err
	}
	if v.ID == "" {
		v.ID = newBoot()[:8]
	}
	return e.writeManual(ctx, v, 0, "made")
}

// UpdateManualRoute replaces the manual route id, read at revision rev.
func (e *Engine) UpdateManualRoute(ctx context.Context, id string, rev int, v engine.ManualRouteView) (engine.ManualRouteView, error) {
	if err := e.begin(ctx, "UpdateManualRoute", struct {
		ID    string                 `json:"id"`
		Rev   int                    `json:"rev"`
		Route engine.ManualRouteView `json:"route"`
	}{id, rev, v}); err != nil {
		return engine.ManualRouteView{}, err
	}
	if v.ID != "" && v.ID != id {
		return engine.ManualRouteView{}, &engine.FieldError{Field: "id", Err: fmt.Errorf("id %q: the id of a manual route does not change; make a new one", v.ID)}
	}
	v.ID = id
	if rev < 1 {
		return engine.ManualRouteView{}, &engine.FieldError{Field: "rev", Err: fmt.Errorf("rev %d: give the revision the route was read at", rev)}
	}
	return e.writeManual(ctx, v, rev, "changed")
}

func (e *Engine) writeManual(ctx context.Context, v engine.ManualRouteView, rev int, done string) (engine.ManualRouteView, error) {
	s, err := e.store.Settings()
	if err != nil {
		return engine.ManualRouteView{}, err
	}
	r, err := manualRoute(v, s)
	if err != nil {
		return engine.ManualRouteView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	old, found, err := e.storedManual(r.ManualID)
	switch {
	case err != nil:
		return engine.ManualRouteView{}, err
	case rev == 0 && found:
		return engine.ManualRouteView{}, fmt.Errorf("%w: the id %s is taken by %s; choose another one", engine.ErrRefused, r.Owner(), old.Route.Hostname)
	case rev > 0 && !found:
		return engine.ManualRouteView{}, fmt.Errorf("%w: there is no manual route %s", engine.ErrNotFound, r.Owner())
	}
	now, err := e.store.SaveManualRouteIf(rev, r)
	switch {
	case errors.Is(err, store.ErrRevision):
		return engine.ManualRouteView{}, staleManual(r.ManualID, rev)
	case err != nil:
		return engine.ManualRouteView{}, err
	}
	e.manualEvent(ctx, r, done)
	return manualView(r, now), nil
}

// DeleteManualRoute removes the manual route id, read at revision rev.
func (e *Engine) DeleteManualRoute(ctx context.Context, id string, rev int) error {
	if err := e.begin(ctx, "DeleteManualRoute", struct {
		ID  string `json:"id"`
		Rev int    `json:"rev"`
	}{id, rev}); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	old, found, err := e.storedManual(id)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: there is no manual route %s", engine.ErrNotFound, model.ManualPrefix+id)
	}
	if err := e.store.DeleteManualRouteIf(id, rev); errors.Is(err, store.ErrRevision) {
		return staleManual(id, rev)
	} else if err != nil {
		return err
	}
	e.manualEvent(ctx, old.Route, "removed")
	return nil
}

func (e *Engine) storedManual(id string) (store.ManualRoute, bool, error) {
	routes, err := e.store.ManualRoutesRev()
	if err != nil {
		return store.ManualRoute{}, false, err
	}
	i := slices.IndexFunc(routes, func(r store.ManualRoute) bool { return r.Route.ManualID == id })
	if i < 0 {
		return store.ManualRoute{}, false, nil
	}
	return routes[i], true, nil
}

func staleManual(id string, rev int) error {
	return fmt.Errorf("%w: the manual route %s changed since it was read at revision %d; read it again",
		engine.ErrRefused, model.ManualPrefix+id, rev)
}

// manualEvent records an action on a manual route. The caller holds mu.
func (e *Engine) manualEvent(ctx context.Context, r model.Route, done string) {
	host := r.Target.Addr.String()
	if r.Guest != nil {
		host = r.Guest.String()
	}
	e.emit(engine.Event{
		Level: "info", Kind: "admin", Subject: r.Owner(), Route: r.Hostname, Actor: engine.ActorOf(ctx),
		Message: fmt.Sprintf("manual route %s is %s: %s -> %s://%s:%d", r.Owner(), done, r.Hostname, r.Target.Scheme, host, r.Target.Port),
	})
}

// manualRoute checks a manual route as the daemon does and returns it as the
// store keeps it. Where an address may point is checked against the
// manualCIDRs of s; a fake has no node addresses to check it against.
func manualRoute(v engine.ManualRouteView, s store.Settings) (model.Route, error) {
	field := func(name, format string, args ...any) error {
		return &engine.FieldError{Field: name, Err: fmt.Errorf(format, args...)}
	}
	if !validID(v.ID) {
		return model.Route{}, field("id", "id %q: want 1 to 32 of a-z, 0-9 and -", v.ID)
	}
	host, err := hostname.Normalize(v.Hostname)
	if err != nil {
		return model.Route{}, &engine.FieldError{Field: "hostname", Err: err}
	}
	r := model.Route{Hostname: host, Source: model.SourceManual, ManualID: v.ID,
		Target: model.Target{Scheme: model.Scheme(v.Target.Scheme), Port: v.Target.Port}}
	switch r.Target.Scheme {
	case model.SchemeHTTP, model.SchemeHTTPS:
	default:
		return model.Route{}, field("target.scheme", "target.scheme %q: want http or https", v.Target.Scheme)
	}
	if v.Target.Port == 0 {
		return model.Route{}, field("target.port", "target.port: want a port from 1 to 65535")
	}
	switch v.Target.Kind {
	case engine.TargetGuest:
		ref, err := model.ParseGuestRef(v.Target.Guest)
		if err != nil {
			return model.Route{}, &engine.FieldError{Field: "target.guest", Err: err}
		}
		if v.Target.Addr.IsValid() {
			return model.Route{}, field("target.addr", "target.addr: a route to a guest goes to the address proven for it, and takes none")
		}
		if v.Options.AllowNode {
			return model.Route{}, field("options.allowNode", "allowNode: only a route to an address may point at a node")
		}
		r.Guest = &ref
	case engine.TargetAddress:
		if v.Target.Guest != "" {
			return model.Route{}, field("target.guest", "target.guest: a route to an address names no guest")
		}
		if !v.Target.Addr.Is4() {
			return model.Route{}, field("target.addr", "target.addr %v: want an IPv4 address", v.Target.Addr)
		}
		if !slices.ContainsFunc(s.ManualCIDRs, func(p netip.Prefix) bool { return p.Contains(v.Target.Addr) }) {
			return model.Route{}, field("target.addr", "target.addr %s: not inside the manualCIDRs of the settings (%s)",
				v.Target.Addr, prefixesText(s.ManualCIDRs))
		}
		r.Target.Addr = v.Target.Addr
	default:
		return model.Route{}, field("target.kind", "target.kind %q: want guest or address", v.Target.Kind)
	}
	if r.Options, err = annotation.CheckOptions(r.Target, v.Options); err != nil {
		var oe *annotation.OptionError
		if errors.As(err, &oe) {
			return model.Route{}, field("options."+oe.Option, "options.%s", oe)
		}
		return model.Route{}, fmt.Errorf("%w: %w", engine.ErrInvalid, err)
	}
	return r, nil
}

func validID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	return !strings.ContainsFunc(id, func(c rune) bool { return (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' })
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

func manualView(r model.Route, rev int) engine.ManualRouteView {
	v := engine.ManualRouteView{
		ID: r.ManualID, Rev: rev, Hostname: r.Hostname, Options: r.Options,
		Target: engine.ManualTarget{Kind: engine.TargetAddress, Scheme: string(r.Target.Scheme), Addr: r.Target.Addr, Port: r.Target.Port},
	}
	if r.Guest != nil {
		v.Target.Kind, v.Target.Guest = engine.TargetGuest, r.Guest.String()
	}
	return v
}

// Guests returns the guest list of the scenario.
func (e *Engine) Guests() ([]engine.GuestListView, error) {
	if err := e.begin(context.Background(), "Guests", noArgs); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.guests), nil
}

// Annotation returns the route block of the Notes of a guest of the guest
// list, as the daemon cuts it out.
func (e *Engine) Annotation(ref model.GuestRef) (engine.AnnotationView, error) {
	if err := e.begin(context.Background(), "Annotation", ref); err != nil {
		return engine.AnnotationView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	notes, ok := e.notes[ref.String()]
	if !ok && !slices.ContainsFunc(e.guests, func(g engine.GuestListView) bool { return g.Ref == ref.String() }) {
		return engine.AnnotationView{}, fmt.Errorf("%w: %s is not in the last listing of Proxmox", engine.ErrNotFound, ref)
	}
	block, line := annotation.Block(notes)
	if r := []rune(block); len(r) > maxAnnotation {
		block = string(r[:maxAnnotation])
	}
	issues := []planner.Issue{}
	for _, is := range e.state.Issues {
		if is.Guest == ref {
			issues = append(issues, is)
		}
	}
	return engine.AnnotationView{Ref: ref.String(), Block: block, StartLine: line, Issues: issues}, nil
}
