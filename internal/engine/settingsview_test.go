package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// startedWith makes the engine compare the settings with those it was
// started with, as the daemon does for what it reads once.
func (e *env) startedWith(s store.Settings) {
	e.startOnly = func(now store.Settings) []string {
		var out []string
		if now.GateTag != s.GateTag {
			out = append(out, "gateTag")
		}
		if now.CloudflareBudget != s.CloudflareBudget {
			out = append(out, "cloudflareBudget")
		}
		return out
	}
	e.eng = e.newEngine()
	e.eng.d.StartOnlyFields = []string{"trustedCIDRs", "gateTag", "cloudflareBudget", "trustStatic"}
}

// saveSettings saves s at the revision the engine shows now.
func (e *env) saveSettings(change func(*store.Settings)) (SettingsView, []string, error) {
	e.t.Helper()
	v, err := e.eng.SettingsView()
	require.NoError(e.t, err)
	s := v.Settings
	change(&s)
	return e.eng.SaveSettings(WithActor(e.t.Context(), "alice@pve (ticket)"), v.Rev, s)
}

func TestSettingsAreReadAndWrittenWithTheirRevision(t *testing.T) {
	e := newEnv(t)
	v, err := e.eng.SettingsView()
	require.NoError(t, err)
	require.Zero(t, v.Rev)
	require.Equal(t, store.DefaultSettings(), v.Settings)
	require.Equal(t, []string{}, v.Notes)

	saved, restart, err := e.saveSettings(func(s *store.Settings) {
		s.MaxHostnamesPerGuest, s.ReverifyInterval, s.CloudflareBudget = 8, store.Duration(2*time.Minute), 900
	})
	require.NoError(t, err)
	require.Equal(t, 1, saved.Rev)
	require.Equal(t, []string{}, restart)
	require.Equal(t, 8, saved.Settings.MaxHostnamesPerGuest)

	v, err = e.eng.SettingsView()
	require.NoError(t, err)
	require.Equal(t, saved, v)
	data, err := json.Marshal(v)
	require.NoError(t, err)
	for _, part := range []string{`"rev":1`, `"maxHostnamesPerGuest":8`, `"reverifyInterval":"2m0s"`, `"cloudflareBudget":900`} {
		require.Contains(t, string(data), part)
	}
}

func TestSettingsOutOfTheirRangeAreRefusedWithTheirField(t *testing.T) {
	for _, tt := range []struct {
		field  string
		change func(*store.Settings)
	}{
		{"pollInterval", func(s *store.Settings) { s.PollInterval = store.Duration(4 * time.Second) }},
		{"grace", func(s *store.Settings) { s.Grace = store.Duration(29 * time.Second) }},
		{"reverifyInterval", func(s *store.Settings) { s.ReverifyInterval = store.Duration(9 * time.Second) }},
		{"reverifyInterval", func(s *store.Settings) { s.ReverifyInterval = store.Duration(5*time.Minute + time.Second) }},
		{"maxHostnamesPerGuest", func(s *store.Settings) { s.MaxHostnamesPerGuest = 0 }},
		{"cloudflareBudget", func(s *store.Settings) { s.CloudflareBudget = 99 }},
		{"cloudflareBudget", func(s *store.Settings) { s.CloudflareBudget = 1151 }},
		{"denyHosts[2]", func(s *store.Settings) { s.DenyHosts = []string{"a.example.com", "b.example.com", "not a host"} }},
		{"trustedCIDRs[0]", func(s *store.Settings) { s.TrustedCIDRs = []netip.Prefix{netip.MustParsePrefix("fd00::/8")} }},
		{"manualCIDRs[0]", func(s *store.Settings) { s.ManualCIDRs = []netip.Prefix{netip.MustParsePrefix("fd00::/8")} }},
	} {
		t.Run(tt.field, func(t *testing.T) {
			e := newEnv(t)

			_, _, err := e.saveSettings(tt.change)

			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			require.Equal(t, tt.field, fe.Field)
			require.ErrorIs(t, err, ErrInvalid)
			require.True(t, strings.HasPrefix(err.Error(), tt.field[:4]), "the message names the setting: %s", err)
			v, err := e.eng.SettingsView()
			require.NoError(t, err)
			require.Zero(t, v.Rev, "nothing was written")
		})
	}
}

func TestSettingsReadAtAnOlderRevisionAreRefused(t *testing.T) {
	e := newEnv(t)
	first, err := e.eng.SettingsView()
	require.NoError(t, err)
	_, _, err = e.saveSettings(func(s *store.Settings) { s.Grace = store.Duration(2 * time.Minute) })
	require.NoError(t, err)

	stale := first.Settings
	stale.PollInterval = store.Duration(time.Minute)
	_, _, err = e.eng.SaveSettings(t.Context(), first.Rev, stale)

	require.ErrorIs(t, err, ErrRefused)
	require.Contains(t, err.Error(), "read them again")
	v, err := e.eng.SettingsView()
	require.NoError(t, err)
	require.Equal(t, 1, v.Rev)
	require.Equal(t, store.Duration(2*time.Minute), v.Settings.Grace)
	require.Equal(t, store.Duration(10*time.Second), v.Settings.PollInterval)
}

// Leaving observe-only mode has one way, apply, which shows what it changes.
func TestTheSettingsDoNotLeaveObserveOnlyMode(t *testing.T) {
	e := newEnv(t)

	_, _, err := e.saveSettings(func(s *store.Settings) { s.ObserveOnly = false })

	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "observeOnly", fe.Field)
	require.Contains(t, err.Error(), "use apply")
	s, err := e.store.Settings()
	require.NoError(t, err)
	require.True(t, s.ObserveOnly)

	e.enforce()
	_, _, err = e.saveSettings(func(s *store.Settings) { s.ObserveOnly = true })
	require.NoError(t, err, "returning to observe-only mode is a setting")
}

func TestASaveSaysWhatNeedsARestart(t *testing.T) {
	e := newEnv(t)
	e.startedWith(store.DefaultSettings())

	_, restart, err := e.saveSettings(func(s *store.Settings) { s.CloudflareBudget = 600 })
	require.NoError(t, err)
	require.Equal(t, []string{"cloudflareBudget"}, restart)

	_, restart, err = e.saveSettings(func(s *store.Settings) { s.GateTag = "publish" })
	require.NoError(t, err)
	require.Equal(t, []string{"gateTag", "cloudflareBudget"}, restart, "everything that differs from the start")

	_, restart, err = e.saveSettings(func(s *store.Settings) {
		s.GateTag, s.CloudflareBudget, s.Grace = "cf-tunnel", 1000, store.Duration(time.Hour)
	})
	require.NoError(t, err)
	require.Equal(t, []string{}, restart)
}

func TestTheSettingsReadAtStartAreTheDaemons(t *testing.T) {
	e := newEnv(t)
	v, err := e.eng.SettingsView()
	require.NoError(t, err)
	require.Equal(t, []string{}, v.ReadAtStart, "an engine told of none has none")

	e.startedWith(store.DefaultSettings())
	v, err = e.eng.SettingsView()
	require.NoError(t, err)
	require.Equal(t, []string{"cloudflareBudget", "gateTag", "trustStatic", "trustedCIDRs"}, v.ReadAtStart)
}

func TestTheLimitsOfTheSettings(t *testing.T) {
	e := newEnv(t)
	v, err := e.eng.SettingsView()
	require.NoError(t, err)

	data, err := json.Marshal(v.Limits)
	require.NoError(t, err)
	require.JSONEq(t, `{"pollInterval":{"min":"5s"},"grace":{"min":"30s"},
		"reverifyInterval":{"min":"10s","max":"5m0s"},"maxHostnamesPerGuest":{"min":1},
		"cloudflareBudget":{"min":100,"max":1150}}`, string(data))
}

func TestSettingsRaisedToTheirMinimumAreNoted(t *testing.T) {
	e := newEnv(t)
	writeSettingsFile(t, e, `{"gateTag":"cf-tunnel","pollInterval":"1s","grace":"1m0s","admission":"tag","observeOnly":true,"identityMinimum":"port"}`)

	v, err := e.eng.SettingsView()
	require.NoError(t, err)
	require.Equal(t, store.Duration(5*time.Second), v.Settings.PollInterval)
	require.Len(t, v.Notes, 1)
	require.Contains(t, v.Notes[0], "pollInterval is 1s")
}

func writeSettingsFile(t *testing.T, e *env, data string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(e.paths.Cluster, "meta", "settings.json"),
		[]byte(`{"schemaVersion":1,"rev":3,"id":"settings","data":`+data+`}`), 0o600))
}

func TestASaveIsAnAdminEventThatNamesWhatChanged(t *testing.T) {
	e := newEnv(t)
	long := make([]string, 11)
	for i := range long {
		long[i] = fmt.Sprintf("h%d.example.com", i)
	}

	_, _, err := e.saveSettings(func(s *store.Settings) {
		s.PollInterval, s.AllowHosts, s.DenyHosts = store.Duration(30*time.Second), long, []string{"admin.example.com"}
	})
	require.NoError(t, err)

	events := e.eng.Events(time.Time{})
	require.Len(t, events, 1)
	ev := events[0]
	require.Equal(t, kindAdmin, ev.Kind)
	require.Equal(t, "alice@pve (ticket)", ev.Actor)
	require.Equal(t, "the settings are saved: allowHosts none -> 11 entries; denyHosts none -> [admin.example.com]; pollInterval 10s -> 30s", ev.Message)
	require.NotContains(t, ev.Message, "h3.example.com", "a long list is never written out")

	_, _, err = e.saveSettings(func(*store.Settings) {})
	require.NoError(t, err)
	events = e.eng.Events(time.Time{})
	require.Equal(t, "the settings are saved; nothing changed", events[len(events)-1].Message)
}

func TestARestartIsAskedOfTheDaemon(t *testing.T) {
	e := newEnv(t)
	err := e.eng.RequestRestart(t.Context())
	require.ErrorIs(t, err, ErrRefused, "an engine without a way to restart cannot")

	asked := 0
	e.eng.d.Restart = func() { asked++ }
	require.NoError(t, e.eng.RequestRestart(WithActor(t.Context(), "root (cli)")))
	require.Equal(t, 1, asked)
	events := e.eng.Events(time.Time{})
	require.Equal(t, "root (cli)", events[len(events)-1].Actor)
	require.Contains(t, events[len(events)-1].Message, "restart")
}

func TestAFieldErrorIsAnInvalidRequest(t *testing.T) {
	err := fmt.Errorf("saving: %w", &FieldError{Field: "hostname", Err: errors.New("hostname x: not one")})
	require.ErrorIs(t, err, ErrInvalid)
	require.Equal(t, "saving: hostname x: not one", err.Error())
}
