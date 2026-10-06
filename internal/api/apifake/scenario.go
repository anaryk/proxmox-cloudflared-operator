package apifake

import (
	"bytes"
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// builtin holds the scenarios of the package, one directory each.
//
//go:embed testdata/scenarios
var builtin embed.FS

const scenariosDir = "testdata/scenarios"

// The files of a scenario directory. Only state.json is needed, and not even
// that by a scenario that is generated or like another.
const (
	fileScenario  = "scenario.json"
	fileState     = "state.json"
	fileEvents    = "events.json"
	fileTraffic   = "traffic.json"
	fileSettings  = "settings.json"
	fileManual    = "manual_routes.json"
	fileGuests    = "guests.json"
	fileNotes     = "notes.json"
	fileClaims    = "claims.json"
	fileApprovals = "approvals.json"
	fileReport    = "report.json"
)

// files is a scenario as its directory has it, each file in the type the
// daemon's API answers with.
type files struct {
	About string `json:"about"`
	// Generate names the generator that makes the scenario from populated,
	// and Like a scenario of the package whose files it shares.
	Generate string `json:"generate,omitempty"`
	Like     string `json:"like,omitempty"`
	// Now is the time the scenario is as of: its times move with the clock
	// from there. Without it the end of the last cycle of the state is.
	Now time.Time `json:"now,omitzero"`

	State     engine.State             `json:"-"`
	Events    []engine.Event           `json:"-"`
	Traffic   *engine.TrafficView      `json:"-"`
	Settings  *engine.SettingsView     `json:"-"`
	Manual    []engine.ManualRouteView `json:"-"`
	Guests    []engine.GuestListView   `json:"-"`
	Notes     map[string]string        `json:"-"` // the Notes of a guest, by owner
	Claims    []engine.ClaimView       `json:"-"`
	Approvals []engine.ApprovalView    `json:"-"`
	// Report is what the check of a credential added to the scenario finds.
	Report *credentials.Report `json:"-"`
}

// Scenarios returns the names of the scenarios of the package.
func Scenarios() []string {
	entries, err := builtin.ReadDir(scenariosDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// About says in one line what a scenario of the package shows.
func About(name string) (string, error) {
	f, err := builtinFiles(name)
	if err != nil {
		return "", err
	}
	return f.About, nil
}

func builtinFiles(name string) (files, error) {
	if !slices.Contains(Scenarios(), name) {
		return files{}, fmt.Errorf("there is no scenario %q; there are %s", name, strings.Join(Scenarios(), ", "))
	}
	sub, err := fs.Sub(builtin, scenariosDir+"/"+name)
	if err != nil {
		return files{}, err
	}
	return readFiles(sub)
}

// readFiles reads a scenario directory. A scenario that is like another reads
// the files of that one, and a generated one is made from populated.
func readFiles(dir fs.FS) (files, error) {
	var f files
	if err := readJSON(dir, fileScenario, &f); err != nil {
		return files{}, err
	}
	switch {
	case f.Like != "" && f.Generate != "":
		return files{}, errors.New(fileScenario + ": a scenario is either like another or generated")
	case f.Like != "":
		like, err := builtinFiles(f.Like)
		if err != nil {
			return files{}, err
		}
		like.About, like.Like = f.About, f.Like
		return like, nil
	case f.Generate != "":
		base, err := builtinFiles("populated")
		if err != nil {
			return files{}, err
		}
		made, err := generate(f.Generate, base)
		if err != nil {
			return files{}, err
		}
		made.About, made.Generate = f.About, f.Generate
		return made, nil
	}
	if _, err := fs.Stat(dir, fileState); err != nil {
		return files{}, fmt.Errorf("the scenario has no %s: %w", fileState, err)
	}
	for _, r := range []struct {
		name string
		v    any
	}{
		{fileState, &f.State}, {fileEvents, &f.Events}, {fileManual, &f.Manual}, {fileGuests, &f.Guests},
		{fileNotes, &f.Notes}, {fileClaims, &f.Claims}, {fileApprovals, &f.Approvals},
	} {
		if err := readJSON(dir, r.name, r.v); err != nil {
			return files{}, err
		}
	}
	for _, r := range []struct {
		name string
		set  func(fs.FS, string) error
	}{
		{fileTraffic, func(d fs.FS, n string) error { return readOptional(d, n, &f.Traffic) }},
		{fileSettings, func(d fs.FS, n string) error { return readOptional(d, n, &f.Settings) }},
		{fileReport, func(d fs.FS, n string) error { return readOptional(d, n, &f.Report) }},
	} {
		if err := r.set(dir, r.name); err != nil {
			return files{}, err
		}
	}
	return f, nil
}

// readJSON decodes a file of the scenario strictly, as the daemon's clients
// must be able to: a key the type has no field for is an error. A file that
// is not there leaves v as it is.
func readJSON(dir fs.FS, name string, v any) error {
	data, err := fs.ReadFile(dir, name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// readOptional decodes a file into a new value, or leaves *p nil without one.
func readOptional[T any](dir fs.FS, name string, p **T) error {
	if _, err := fs.Stat(dir, name); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	v := new(T)
	if err := readJSON(dir, name, v); err != nil {
		return err
	}
	*p = v
	return nil
}

// moment is the time a scenario is as of: its now, or else the end of the
// last cycle of its state; zero for a scenario without a time.
func (f files) moment() time.Time {
	if !f.Now.IsZero() {
		return f.Now
	}
	return f.State.FinishedAt
}

// shift moves every time of the scenario, and every time a text of it names
// in full, by as much as makes its moment now, in whole seconds, as the texts
// name them. A clock time a text names, as "checking again at 12:10", stays.
func (f *files) shift(now time.Time) error {
	at := f.moment()
	if at.IsZero() {
		return nil
	}
	d := now.Sub(at).Truncate(time.Second)
	var err error
	if f.State, err = shifted(f.State, d); err != nil {
		return err
	}
	if f.Events, err = shifted(f.Events, d); err != nil {
		return err
	}
	if f.Traffic, err = shifted(f.Traffic, d); err != nil {
		return err
	}
	if f.Claims, err = shifted(f.Claims, d); err != nil {
		return err
	}
	if f.Approvals, err = shifted(f.Approvals, d); err != nil {
		return err
	}
	if f.Report, err = shifted(f.Report, d); err != nil {
		return err
	}
	f.Now = at.Add(d)
	return nil
}

// timeInText is a time as a text names it.
var timeInText = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

// shifted is v with every time in it moved by d: every value of its JSON that
// is a time, and every time a text of it names.
func shifted[T any](v T, d time.Duration) (T, error) {
	var out T
	data, err := json.Marshal(v)
	if err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return out, err
	}
	if data, err = json.Marshal(shiftValue(doc, d)); err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	return out, err
}

func shiftValue(v any, d time.Duration) any {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			v[k] = shiftValue(x, d)
		}
	case []any:
		for i, x := range v {
			v[i] = shiftValue(x, d)
		}
	case string:
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil && timeInText.MatchString(v) {
			return t.Add(d).UTC().Format(time.RFC3339Nano)
		}
		return timeInText.ReplaceAllStringFunc(v, func(s string) string {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return s
			}
			return t.Add(d).UTC().Format(time.RFC3339)
		})
	}
	return v
}

// sortState puts the lists of a state in the order the daemon gives them,
// and makes every list that is missing an empty one.
func sortState(st *engine.State) {
	sortRoutes(st.Routes)
	slices.SortStableFunc(st.Tunnels, func(a, b engine.TunnelView) int {
		return cmp.Or(strings.Compare(a.AccountID, b.AccountID), strings.Compare(a.ID, b.ID))
	})
	slices.SortStableFunc(st.Connectors, func(a, b connector.Status) int { return strings.Compare(a.TunnelID, b.TunnelID) })
	slices.SortStableFunc(st.Credentials, func(a, b engine.CredentialView) int { return strings.Compare(a.ID, b.ID) })
	slices.SortStableFunc(st.Zones, func(a, b engine.ZoneView) int { return strings.Compare(a.Name, b.Name) })
	st.Problems = slices.Compact(slices.Sorted(slices.Values(st.Problems)))
	st.Lost = slices.Compact(slices.Sorted(slices.Values(st.Lost)))
	st.Routes, st.Issues, st.Tunnels = nonNil(st.Routes), nonNil(st.Issues), nonNil(st.Tunnels)
	st.Connectors, st.Credentials, st.Zones = nonNil(st.Connectors), nonNil(st.Credentials), nonNil(st.Zones)
	st.Actions, st.Conflicts, st.Lost = nonNil(st.Actions), nonNil(st.Conflicts), nonNil(st.Lost)
	st.Problems, st.Waiting, st.Unapproved = nonNil(st.Problems), nonNil(st.Waiting), nonNil(st.Unapproved)
	st.Segments, st.RogueConnectors = nonNil(st.Segments), nonNil(st.RogueConnectors)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// sortRoutes puts routes in the order of a state, by hostname and then by
// owner, and returns them.
func sortRoutes(routes []engine.RouteView) []engine.RouteView {
	slices.SortStableFunc(routes, func(a, b engine.RouteView) int {
		return cmp.Or(strings.Compare(a.Hostname, b.Hostname), model.CompareOwners(a.Owner, b.Owner))
	})
	return routes
}
