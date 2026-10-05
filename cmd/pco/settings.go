package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// maxSettingsFile is the largest settings file apply reads, as the web UI
// takes for an import.
const maxSettingsFile = 1 << 20

func (a *app) settingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "Show the settings and save them",
	}
	cmd.AddCommand(a.settingsShowCmd(), a.settingsApplyCmd())
	return cmd
}

func (a *app) settingsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the settings with their revision",
		Long: "Show the settings, the revision they are at and, for each one that has them, the range the\n" +
			"daemon accepts and whether it is read only when the daemon starts.\n\n" + jsonHelp + " That is\n" +
			"the file pco settings apply takes: save it, edit the settings in it, and apply it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().SettingsRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			v, err := a.client().Settings(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			return renderSettings(cmd.OutOrStdout(), v)
		},
	}
}

// renderSettings writes every setting, in the order of store.Settings, with
// its range and whether it is read at start, as the daemon says.
func renderSettings(w io.Writer, v engine.SettingsView) error {
	s := &screen{w: w}
	s.printf("Settings at revision %d:\n", v.Rev)
	t := s.table()
	values := reflect.ValueOf(v.Settings)
	for _, f := range reflect.VisibleFields(values.Type()) {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		var notes []string
		if l, ok := v.Limits[name]; ok {
			notes = append(notes, limitText(l))
		}
		if slices.Contains(v.ReadAtStart, name) {
			notes = append(notes, "read at start")
		}
		value := dash(settingText(values.FieldByIndex(f.Index).Interface()))
		if len(notes) > 0 {
			value += " (" + strings.Join(notes, "; ") + ")"
		}
		t.row("  "+name, value)
	}
	t.flush()
	if len(v.Notes) > 0 {
		s.println("")
		for _, n := range v.Notes {
			s.printf("%s\n", n)
		}
	}
	return s.done()
}

// settingText writes the value of a setting as its JSON says it, without the
// quotes of a string and the brackets of a list.
func settingText(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var decoded any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return string(raw)
	}
	switch d := decoded.(type) {
	case nil:
		return ""
	case string:
		return d
	case []any:
		parts := make([]string, len(d))
		for i, x := range d {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		keys := slices.Sorted(maps.Keys(d))
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = fmt.Sprintf("%s=%v", k, d[k])
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(decoded)
}

// limitText writes a range: "at least 5s", "10s to 5m0s".
func limitText(l engine.Limit) string {
	switch {
	case l.Min != nil && l.Max != nil:
		return fmt.Sprintf("%v to %v", l.Min, l.Max)
	case l.Max != nil:
		return fmt.Sprintf("at most %v", l.Max)
	}
	return fmt.Sprintf("at least %v", l.Min)
}

func (a *app) settingsApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply <file>",
		Short: "Save the settings of a file, at the revision they were read at",
		Long: "Save the settings of a file: what pco settings show --json printed, with the settings in it\n" +
			"edited. Its revision is sent with them, and the daemon refuses the settings when someone\n" +
			"saved others since: show them again and edit those. A setting out of its range is refused\n" +
			"with its name. Leaving observe-only mode is no setting: that is pco apply. The settings read\n" +
			"only at start take effect once pco is restarted (systemctl restart pco).\n\n" +
			"With --json the answer of the daemon is printed as it sent it, re-indented, with control and\n" +
			"bidirectional characters escaped.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rev, settings, err := readSettingsFile(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			raw, err := a.client().SaveSettings(ctx, rev, settings)
			if err != nil {
				return a.explain(ctx, err)
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var saved struct {
				engine.SettingsView
				RestartNeeded []string `json:"restartNeeded"`
			}
			if err := json.Unmarshal(raw, &saved); err != nil {
				return couldNotAsk{fmt.Errorf("decoding the answer of the daemon: %w", err)}
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Saved the settings; they are at revision %d.\n", saved.Rev)
			if len(saved.RestartNeeded) > 0 {
				s.printf("%s differ from what pco was started with and are read only at start: "+
					"restart pco (systemctl restart pco) for them to take effect.\n", strings.Join(saved.RestartNeeded, ", "))
			}
			return s.done()
		},
	}
}

// readSettingsFile reads what pco settings show --json printed: the revision
// and the settings, which are passed on as they are, for the daemon to read
// as strictly as its own. A key the answer has no field for is refused.
func readSettingsFile(path string) (int, json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxSettingsFile+1))
	if err != nil {
		return 0, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(data) > maxSettingsFile {
		return 0, nil, fmt.Errorf("%s is larger than %d bytes: it is no settings file", path, maxSettingsFile)
	}
	var file struct {
		Rev           *int            `json:"rev"`
		Settings      json.RawMessage `json:"settings"`
		ReadAtStart   json.RawMessage `json:"readAtStart"`
		Limits        json.RawMessage `json:"limits"`
		Notes         json.RawMessage `json:"notes"`
		RestartNeeded json.RawMessage `json:"restartNeeded"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return 0, nil, fmt.Errorf("%s is not what pco settings show --json prints: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return 0, nil, fmt.Errorf("%s has more after the settings: it is not what pco settings show --json prints", path)
	}
	switch {
	case file.Rev == nil:
		return 0, nil, fmt.Errorf("%s has no rev: save what pco settings show --json prints and edit its settings", path)
	case len(file.Settings) == 0 || string(file.Settings) == "null":
		return 0, nil, fmt.Errorf("%s has no settings: save what pco settings show --json prints and edit its settings", path)
	}
	return *file.Rev, file.Settings, nil
}
