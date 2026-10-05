package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// maxListed is the longest list an event writes out; of a longer one it
// gives the length.
const maxListed = 10

// FieldError is a request refused for one of its fields. Field is the path of
// that field in the JSON of the request, such as "denyHosts[2]" or
// "target.addr". errors.Is(err, ErrInvalid) holds for it.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Err.Error() }

func (e *FieldError) Unwrap() []error { return []error{ErrInvalid, e.Err} }

// fieldError is the FieldError of field, with the message format makes.
func fieldError(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Err: fmt.Errorf(format, args...)}
}

// SettingsView is the settings as stored, with their revision, and what the
// daemon says about them: the settings it reads only at its start, the ranges
// it accepts and the notes of settings it uses otherwise than stored.
type SettingsView struct {
	Rev         int              `json:"rev"`
	Settings    store.Settings   `json:"settings"`
	ReadAtStart []string         `json:"readAtStart"`
	Limits      map[string]Limit `json:"limits"`
	Notes       []string         `json:"notes"`
}

// Limit is the range the store accepts for a setting; a duration as Go
// writes it ("10s"), a number as JSON does.
type Limit struct {
	Min any `json:"min,omitempty"`
	Max any `json:"max,omitempty"`
}

// SettingsView returns the stored settings with their revision.
func (e *Engine) SettingsView() (SettingsView, error) {
	s, rev, notes, err := e.d.Store.LoadSettingsRev()
	if err != nil {
		return SettingsView{}, fmt.Errorf("reading the settings: %w", err)
	}
	return e.settingsView(s, rev, notes), nil
}

func (e *Engine) settingsView(s store.Settings, rev int, notes []string) SettingsView {
	limits := make(map[string]Limit)
	for name, l := range store.Limits() {
		limits[name] = Limit{Min: l.Min, Max: l.Max}
	}
	return SettingsView{
		Rev:         rev,
		Settings:    s,
		ReadAtStart: nonNil(slices.Sorted(slices.Values(e.d.StartOnlyFields))),
		Limits:      limits,
		Notes:       nonNil(notes),
	}
}

// SaveSettings writes s when the stored settings are still at revision rev,
// which they were read at; otherwise it refuses with ErrRefused. A setting the
// store does not accept is refused with a *FieldError. Leaving observe-only
// mode is refused too: that is Apply's, which says what it changes. It
// returns the settings as saved and the names of those that take effect
// only once the daemon is restarted.
func (e *Engine) SaveSettings(ctx context.Context, rev int, s store.Settings) (SettingsView, []string, error) {
	if err := e.acquireAdmin(ctx); err != nil {
		return SettingsView{}, nil, err
	}
	defer e.release()

	old, have, _, err := e.d.Store.LoadSettingsRev()
	switch {
	case err != nil:
		return SettingsView{}, nil, fmt.Errorf("reading the settings: %w", err)
	case have != rev:
		return SettingsView{}, nil, staleSettings(rev, have)
	case old.ObserveOnly && !s.ObserveOnly:
		return SettingsView{}, nil, fieldError("observeOnly", "observeOnly: leaving observe-only mode is not a setting, "+
			"so that what it changes is shown first; use apply (pco apply)")
	}
	_, err = e.d.Store.SaveSettingsIf(rev, s)
	var invalid *store.FieldError
	switch {
	case errors.As(err, &invalid):
		return SettingsView{}, nil, &FieldError{Field: invalid.Field, Err: invalid.Err}
	case errors.Is(err, store.ErrRevision):
		return SettingsView{}, nil, staleSettings(rev, -1)
	case err != nil:
		return SettingsView{}, nil, fmt.Errorf("saving the settings: %w", err)
	}
	saved, now, notes, err := e.d.Store.LoadSettingsRev()
	if err != nil {
		return SettingsView{}, nil, fmt.Errorf("reading the settings saved: %w", err)
	}
	restart := []string{}
	if e.d.StartOnly != nil {
		restart = nonNil(e.d.StartOnly(saved))
	}
	e.adminEvent(ctx, "", settingsSaved(old, saved, restart))
	e.Trigger()
	return e.settingsView(saved, now, notes), restart, nil
}

// staleSettings refuses settings read at revision rev; have is the revision
// stored, or -1 when it is not known.
func staleSettings(rev, have int) error {
	at := ""
	if have >= 0 {
		at = fmt.Sprintf(" and are at revision %d now", have)
	}
	return fmt.Errorf("%w: the settings changed since they were read at revision %d%s; read them again", ErrRefused, rev, at)
}

// settingsSaved is the message of the event of a save: the settings that
// changed, each with what it was and is.
func settingsSaved(old, saved store.Settings, restart []string) string {
	changes := changedSettings(old, saved)
	if len(changes) == 0 {
		return "the settings are saved; nothing changed"
	}
	msg := "the settings are saved: " + strings.Join(changes, "; ")
	if len(restart) > 0 {
		msg += fmt.Sprintf("; %s take effect once pco is restarted", strings.Join(restart, ", "))
	}
	return msg
}

// changedSettings names the settings in which b differs from a, by their
// JSON names in order, each as "name before -> after". A list or a map of
// more than maxListed entries is given by its length.
func changedSettings(a, b store.Settings) []string {
	before, after := settingsFields(a), settingsFields(b)
	names := slices.Collect(maps.Keys(before))
	for name := range after {
		if _, ok := before[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var out []string
	for _, name := range names {
		if bytes.Equal(before[name], after[name]) {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s -> %s", name, settingText(before[name]), settingText(after[name])))
	}
	return out
}

// settingsFields is the JSON of the settings by field. A field the JSON
// leaves out, as an empty list, is not there.
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

// settingText writes the JSON of a setting for an event: a string without
// quotes, a list as [a, b], a map as {k: v}, and none for nothing.
func settingText(raw json.RawMessage) string {
	if raw == nil {
		return "none"
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(raw)
	}
	return valueText(v)
}

func valueText(v any) string {
	switch v := v.(type) {
	case nil:
		return "none"
	case string:
		return v
	case []any:
		if len(v) > maxListed {
			return fmt.Sprintf("%d entries", len(v))
		}
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = valueText(x)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		if len(v) > maxListed {
			return fmt.Sprintf("%d entries", len(v))
		}
		parts := make([]string, 0, len(v))
		for _, k := range slices.Sorted(maps.Keys(v)) {
			parts = append(parts, k+": "+valueText(v[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// RequestRestart asks the daemon to stop once the cycle that runs is done;
// systemd starts it again. The connectors keep running.
func (e *Engine) RequestRestart(ctx context.Context) error {
	if e.d.Restart == nil {
		return fmt.Errorf("%w: this daemon cannot restart itself; run systemctl restart pco", ErrRefused)
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return err
	}
	defer e.release()
	e.adminEvent(ctx, "", "a restart of the daemon is asked for: it stops now, and systemd starts it again")
	e.d.Restart()
	return nil
}
