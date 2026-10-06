package store

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestSettingsAreReadWithTheirRevision(t *testing.T) {
	s, p := openStore(t)
	v, rev, notes, err := s.LoadSettingsRev()
	require.NoError(t, err)
	require.Equal(t, DefaultSettings(), v)
	require.Zero(t, rev, "nothing stored is revision 0")
	require.Empty(t, notes)

	rev, err = s.SaveSettingsIf(0, customSettings())
	require.NoError(t, err)
	require.Equal(t, 1, rev)
	v, rev, _, err = s.LoadSettingsRev()
	require.NoError(t, err)
	require.Equal(t, customSettings(), v)
	require.Equal(t, 1, rev)
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Cluster, "meta", "settings.json")))

	changed := customSettings()
	changed.CloudflareBudget = 900
	rev, err = s.SaveSettingsIf(1, changed)
	require.NoError(t, err)
	require.Equal(t, 2, rev)

	rev, err = s.SaveSettingsIf(2, changed)
	require.NoError(t, err)
	require.Equal(t, 2, rev, "settings that did not change are not written again")
}

func TestSettingsReadAtAnotherRevisionAreNotSaved(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.SaveSettingsIf(0, customSettings())
	require.NoError(t, err)

	for _, rev := range []int{0, 2, 7} {
		changed := customSettings()
		changed.GateTag = "other"
		got, err := s.SaveSettingsIf(rev, changed)
		require.ErrorIs(t, err, ErrRevision, "revision %d", rev)
		require.Equal(t, 1, got, "the revision stored is told")
	}
	v, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, customSettings(), v)
}

func TestInvalidSettingsNameTheirField(t *testing.T) {
	for _, tt := range []struct {
		name, field string
		edit        func(*Settings)
	}{
		{"gate tag", "gateTag", func(s *Settings) { s.GateTag = "" }},
		{"the third deny pattern", "denyHosts[2]", func(s *Settings) { s.DenyHosts = []string{"a.example.com", "b.example.com", "a..b"} }},
		{"an allow pattern", "allowHosts[0]", func(s *Settings) { s.AllowHosts = []string{"not a host"} }},
		{"poll interval", "pollInterval", func(s *Settings) { s.PollInterval = Duration(time.Second) }},
		{"grace", "grace", func(s *Settings) { s.Grace = 0 }},
		{"hostnames per guest", "maxHostnamesPerGuest", func(s *Settings) { s.MaxHostnamesPerGuest = 0 }},
		{"reverify interval under", "reverifyInterval", func(s *Settings) { s.ReverifyInterval = Duration(9 * time.Second) }},
		{"reverify interval over", "reverifyInterval", func(s *Settings) { s.ReverifyInterval = Duration(6 * time.Minute) }},
		{"budget under", "cloudflareBudget", func(s *Settings) { s.CloudflareBudget = 99 }},
		{"budget over", "cloudflareBudget", func(s *Settings) { s.CloudflareBudget = 1151 }},
		{"admission", "admission", func(s *Settings) { s.Admission = "open" }},
		{"identity minimum", "identityMinimum", func(s *Settings) { s.IdentityMinimum = "manual" }},
		{"a trusted prefix", "trustedCIDRs[1]", func(s *Settings) {
			s.TrustedCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
		}},
		{"a manual prefix", "manualCIDRs[1]", func(s *Settings) {
			s.ManualCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
		}},
		{"a zone pin", `zonePins["example.com"]`, func(s *Settings) { s.ZonePins = map[string]string{"example.com": ""} }},
		{"a zone that is not one", `zonePins["not a zone"]`, func(s *Settings) { s.ZonePins = map[string]string{"not a zone": "c"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			bad := customSettings()
			tt.edit(&bad)

			_, err := s.SaveSettingsIf(0, bad)

			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			require.Equal(t, tt.field, fe.Field)
			require.False(t, errors.Is(err, ErrRevision))
			require.Empty(t, stored(t, p.Cluster))
			require.ErrorAs(t, s.SaveSettings(bad), &fe, "SaveSettings names it too")
		})
	}
}

func TestTheLimitsAreThoseTheSettingsAreCheckedAgainst(t *testing.T) {
	limits := Limits()
	require.Equal(t, map[string]Limit{
		"pollInterval":         {Min: Duration(5 * time.Second)},
		"grace":                {Min: Duration(30 * time.Second)},
		"reverifyInterval":     {Min: Duration(10 * time.Second), Max: Duration(5 * time.Minute)},
		"maxHostnamesPerGuest": {Min: 1},
		"cloudflareBudget":     {Min: 100, Max: 1150},
	}, limits)

	s, _ := openStore(t)
	at := customSettings()
	at.PollInterval, at.Grace = limits["pollInterval"].Min.(Duration), limits["grace"].Min.(Duration)
	at.ReverifyInterval, at.MaxHostnamesPerGuest = limits["reverifyInterval"].Max.(Duration), limits["maxHostnamesPerGuest"].Min.(int)
	at.CloudflareBudget = limits["cloudflareBudget"].Max.(int)
	require.NoError(t, s.SaveSettings(at), "every bound itself is accepted")
}

func addressRoute(id, host string) model.Route {
	return model.Route{
		Hostname: host,
		Target:   model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.0.5.20"), Port: 9000},
		Source:   model.SourceManual,
		ManualID: id,
	}
}

func TestManualRoutesAreWrittenAtTheirRevision(t *testing.T) {
	s, p := openStore(t)
	routes, err := s.ManualRoutesRev()
	require.NoError(t, err)
	require.NotNil(t, routes)
	require.Empty(t, routes)

	rev, err := s.SaveManualRouteIf(0, addressRoute("status", "Status.Example.com"))
	require.NoError(t, err)
	require.Equal(t, 1, rev)
	_, err = s.SaveManualRouteIf(0, addressRoute("status", "other.example.com"))
	require.ErrorIs(t, err, ErrRevision, "a route of that id is there")

	moved := addressRoute("status", "status.example.com")
	moved.Target.Port = 9001
	rev, err = s.SaveManualRouteIf(1, moved)
	require.NoError(t, err)
	require.Equal(t, 2, rev)
	_, err = s.SaveManualRouteIf(1, addressRoute("status", "status.example.com"))
	require.ErrorIs(t, err, ErrRevision)

	_, err = s.SaveManualRouteIf(0, addressRoute("wiki", "wiki.example.com"))
	require.NoError(t, err)
	routes, err = s.ManualRoutesRev()
	require.NoError(t, err)
	require.Equal(t, []ManualRoute{{Route: moved, Rev: 2}, {Route: addressRoute("wiki", "wiki.example.com"), Rev: 1}}, routes)

	require.ErrorIs(t, s.DeleteManualRouteIf("status", 1), ErrRevision)
	require.NoError(t, s.DeleteManualRouteIf("status", 2))
	require.ErrorIs(t, s.DeleteManualRouteIf("status", 2), ErrRevision, "it is gone")
	require.Equal(t, []string{"routes/wiki.json"}, stored(t, p.Cluster))
}

func TestAManualRouteThatIsNotValidIsNotWrittenAtAnyRevision(t *testing.T) {
	s, p := openStore(t)
	_, err := s.SaveManualRouteIf(0, addressRoute("", "status.example.com"))
	require.Error(t, err)
	_, err = s.SaveManualRouteIf(0, addressRoute("status", "not a host"))
	require.Error(t, err)
	require.Empty(t, stored(t, p.Cluster))
}
