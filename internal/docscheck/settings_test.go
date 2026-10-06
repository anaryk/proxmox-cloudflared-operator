package docscheck

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A setting that has no row in docs/settings.md fails here: its page is the
// only place that says what a setting accepts and when a change takes effect.
func TestEverySettingHasARow(t *testing.T) {
	rows := spans(page(t, "settings.md"))
	typ := reflect.TypeFor[store.Settings]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		require.Contains(t, rows, name,
			"docs/settings.md has no row for the setting %q: add one to the table of its group", name)
	}
}

// The other direction: a row of a group of settings whose setting is gone.
func TestNoRowOfASettingTheCodeLost(t *testing.T) {
	have := make(map[string]bool)
	typ := reflect.TypeFor[store.Settings]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		have[name] = true
	}
	text := page(t, "settings.md")
	for _, group := range []string{"Publishing", "Timing", "Identity", "Cloudflare"} {
		for _, row := range spans(section(t, text, group)) {
			require.True(t, have[row],
				"docs/settings.md has a row %q under %q, and store.Settings has no such setting: take the row out", row, group)
		}
	}
}
