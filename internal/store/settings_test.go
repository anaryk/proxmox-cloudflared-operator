package store

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultSettings(t *testing.T) {
	d := DefaultSettings()
	require.Equal(t, "cf-tunnel", d.GateTag)
	require.Equal(t, Duration(10*time.Second), d.PollInterval)
	require.Equal(t, Duration(60*time.Second), d.Grace)
	require.Equal(t, "tag", d.Admission)
	require.True(t, d.ObserveOnly)
	require.False(t, d.TrustStatic)
	require.Empty(t, d.AllowHosts)
	require.Empty(t, d.DenyHosts)
	require.Empty(t, d.TrustedCIDRs)
	require.Empty(t, d.ManualCIDRs)
	require.Empty(t, d.ZonePins)
	require.Equal(t, "port", d.IdentityMinimum)
	require.Equal(t, 32, d.MaxHostnamesPerGuest)
	require.Equal(t, Duration(time.Minute), d.ReverifyInterval)
	require.Equal(t, 1000, d.CloudflareBudget)
}

func TestSettingsDefaultsWhenMissing(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, DefaultSettings(), got)
	require.Empty(t, stored(t, p.Cluster), "reading the defaults writes nothing")
}

func customSettings() Settings {
	return Settings{
		GateTag:      "web-publish",
		AllowHosts:   []string{"*.example.com", "shop.cz"},
		DenyHosts:    []string{"admin.example.com"},
		PollInterval: Duration(30 * time.Second),
		Grace:        Duration(5 * time.Minute),
		TrustStatic:  true,
		TrustedCIDRs: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")},
		ManualCIDRs:  []netip.Prefix{netip.MustParsePrefix("10.30.0.0/24")},
		Admission:    "approve",
		ZonePins:     map[string]string{"example.com": "cred-1"},
		ObserveOnly:  false,
		// Guests on other nodes are served.
		IdentityMinimum:      "observed",
		MaxHostnamesPerGuest: 100,
		ReverifyInterval:     Duration(2 * time.Minute),
		CloudflareBudget:     800,
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	want := customSettings()
	require.NoError(t, s.SaveSettings(want))

	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, []string{"meta/settings.json"}, stored(t, p.Cluster))

	data := readRaw(t, filepath.Join(p.Cluster, "meta", "settings.json")).Data
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Equal(t, "30s", wire["pollInterval"])
	require.Equal(t, "5m0s", wire["grace"])
	require.Equal(t, []any{"10.20.0.0/16"}, wire["trustedCIDRs"])
	require.Equal(t, []any{"10.30.0.0/24"}, wire["manualCIDRs"])
	require.Equal(t, "observed", wire["identityMinimum"])
	require.Equal(t, "2m0s", wire["reverifyInterval"])
	require.InDelta(t, 800, wire["cloudflareBudget"], 0)
}

func TestSettingsAcceptEveryIdentityMinimum(t *testing.T) {
	for _, level := range []string{"observed", "filtered", "port"} {
		t.Run(level, func(t *testing.T) {
			s, _ := openStore(t)
			in := customSettings()
			in.IdentityMinimum = level
			require.NoError(t, s.SaveSettings(in))
			got, err := s.Settings()
			require.NoError(t, err)
			require.Equal(t, level, got.IdentityMinimum)
		})
	}
}

func TestSaveSettingsOfUnchangedSettingsWritesNothing(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "meta", "settings.json")
	require.NoError(t, s.SaveSettings(DefaultSettings()))
	require.NoError(t, s.SaveSettings(DefaultSettings()))
	require.EqualValues(t, 1, revOf(t, path))
	changed := DefaultSettings()
	changed.ObserveOnly = false
	require.NoError(t, s.SaveSettings(changed))
	require.EqualValues(t, 2, revOf(t, path))
}

func TestSaveSettingsRefusesInvalidSettings(t *testing.T) {
	tests := []struct {
		name  string
		field string
		edit  func(*Settings)
	}{
		{"empty gate tag", "gateTag", func(s *Settings) { s.GateTag = "" }},
		{"upper case gate tag", "gateTag", func(s *Settings) { s.GateTag = "CF-Tunnel" }},
		{"gate tag with a space", "gateTag", func(s *Settings) { s.GateTag = "cf tunnel" }},
		{"gate tag with a semicolon", "gateTag", func(s *Settings) { s.GateTag = "a;b" }},
		{"gate tag starting with a hyphen", "gateTag", func(s *Settings) { s.GateTag = "-a" }},
		{"gate tag starting with a dot", "gateTag", func(s *Settings) { s.GateTag = ".a" }},
		{"gate tag over 64 characters", "gateTag", func(s *Settings) { s.GateTag = strings.Repeat("a", 65) }},
		{"bad allow pattern", "allowHosts[1]", func(s *Settings) { s.AllowHosts = []string{"*.example.com", "not a host"} }},
		{"single label allow pattern", "allowHosts[0]", func(s *Settings) { s.AllowHosts = []string{"localhost"} }},
		{"bad deny pattern", "denyHosts[0]", func(s *Settings) { s.DenyHosts = []string{"a..b"} }},
		{"empty deny pattern", "denyHosts[0]", func(s *Settings) { s.DenyHosts = []string{""} }},
		{"zero poll interval", "pollInterval", func(s *Settings) { s.PollInterval = 0 }},
		{"negative poll interval", "pollInterval", func(s *Settings) { s.PollInterval = Duration(-time.Second) }},
		{"poll interval below five seconds", "pollInterval 4.999s: at least 5s", func(s *Settings) { s.PollInterval = Duration(4999 * time.Millisecond) }},
		{"zero grace", "grace", func(s *Settings) { s.Grace = 0 }},
		{"negative grace", "grace", func(s *Settings) { s.Grace = Duration(-time.Minute) }},
		{"grace below thirty seconds", "grace 29s: at least 30s", func(s *Settings) { s.Grace = Duration(29 * time.Second) }},
		{"grace that turns the guard off", "grace 60ms: at least 30s", func(s *Settings) { s.Grace = Duration(60 * time.Millisecond) }},
		{"empty admission", "admission", func(s *Settings) { s.Admission = "" }},
		{"unknown admission", "admission", func(s *Settings) { s.Admission = "open" }},
		{"upper case admission", "admission", func(s *Settings) { s.Admission = "Tag" }},
		{"bad zone pin key", "zonePins", func(s *Settings) { s.ZonePins = map[string]string{"not a zone": "c"} }},
		{"empty zone pin key", "zonePins", func(s *Settings) { s.ZonePins = map[string]string{"": "c"} }},
		{"empty zone pin value", "zonePins", func(s *Settings) { s.ZonePins = map[string]string{"example.com": ""} }},
		{"zone pin keys that collide", "zonePins", func(s *Settings) {
			s.ZonePins = map[string]string{"Example.com": "a", "example.com": "b"}
		}},
		{"ipv6 trusted prefix", "trustedCIDRs[0]", func(s *Settings) { s.TrustedCIDRs = []netip.Prefix{netip.MustParsePrefix("fd00::/8")} }},
		{"mapped trusted prefix", "trustedCIDRs[0]", func(s *Settings) {
			s.TrustedCIDRs = []netip.Prefix{netip.MustParsePrefix("::ffff:10.0.0.0/104")}
		}},
		{"zero trusted prefix", "trustedCIDRs[0]", func(s *Settings) { s.TrustedCIDRs = []netip.Prefix{{}} }},
		{"ipv6 manual prefix", "manualCIDRs[0]", func(s *Settings) { s.ManualCIDRs = []netip.Prefix{netip.MustParsePrefix("fd00::/8")} }},
		{"mapped manual prefix", "manualCIDRs[0]", func(s *Settings) {
			s.ManualCIDRs = []netip.Prefix{netip.MustParsePrefix("::ffff:10.0.0.0/104")}
		}},
		{"zero manual prefix", "manualCIDRs[0]", func(s *Settings) { s.ManualCIDRs = []netip.Prefix{{}} }},
		{"empty identity minimum", "identityMinimum", func(s *Settings) { s.IdentityMinimum = "" }},
		{"unknown identity minimum", "identityMinimum", func(s *Settings) { s.IdentityMinimum = "strict" }},
		{"upper case identity minimum", "identityMinimum", func(s *Settings) { s.IdentityMinimum = "Port" }},
		{"manual as identity minimum", "identityMinimum", func(s *Settings) { s.IdentityMinimum = "manual" }},
		{"no hostname for a guest", "maxHostnamesPerGuest 0: at least 1", func(s *Settings) { s.MaxHostnamesPerGuest = 0 }},
		{"a negative cap", "maxHostnamesPerGuest -1: at least 1", func(s *Settings) { s.MaxHostnamesPerGuest = -1 }},
		{"zero re-check interval", "reverifyInterval 0s: at least 10s", func(s *Settings) { s.ReverifyInterval = 0 }},
		{"re-check interval below ten seconds", "reverifyInterval 9s: at least 10s", func(s *Settings) { s.ReverifyInterval = Duration(9 * time.Second) }},
		{"re-check interval over the age of a proof", "reverifyInterval 5m1s: at most 5m0s", func(s *Settings) {
			s.ReverifyInterval = Duration(5*time.Minute + time.Second)
		}},
		{"no budget", "cloudflareBudget 0: from 100 to 1150", func(s *Settings) { s.CloudflareBudget = 0 }},
		{"a budget below 100", "cloudflareBudget 99: from 100 to 1150", func(s *Settings) { s.CloudflareBudget = 99 }},
		{"a budget that leaves the dashboard nothing", "cloudflareBudget 1151: from 100 to 1150", func(s *Settings) { s.CloudflareBudget = 1151 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, p := openStore(t)
			bad := customSettings()
			tc.edit(&bad)

			err := s.SaveSettings(bad)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.field)
			require.Empty(t, stored(t, p.Cluster), "nothing is written")
		})
	}
}

func TestSaveSettingsKeepsTheOldSettingsWhenRefusing(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveSettings(customSettings()))
	bad := customSettings()
	bad.Admission = "open"
	require.Error(t, s.SaveSettings(bad))

	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, customSettings(), got)
}

func TestSaveSettingsNormalisesWhatItStores(t *testing.T) {
	s, _ := openStore(t)
	in := customSettings()
	in.AllowHosts = []string{"*.Example.COM.", "Shop.cz"}
	in.DenyHosts = []string{"ADMIN.example.com"}
	in.ZonePins = map[string]string{"Example.COM": "cred-1"}
	require.NoError(t, s.SaveSettings(in))
	require.Equal(t, []string{"*.Example.COM.", "Shop.cz"}, in.AllowHosts, "the caller's settings are not changed")
	require.Equal(t, map[string]string{"Example.COM": "cred-1"}, in.ZonePins)

	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, []string{"*.example.com", "shop.cz"}, got.AllowHosts)
	require.Equal(t, []string{"admin.example.com"}, got.DenyHosts)
	require.Equal(t, map[string]string{"example.com": "cred-1"}, got.ZonePins)
}

func TestSettingsPatternsAcceptWhatHostnamesDoNot(t *testing.T) {
	s, _ := openStore(t)
	in := customSettings()
	in.AllowHosts = []string{"*", "*.com"}
	require.NoError(t, s.SaveSettings(in))
}

func TestTheMinimumsThemselvesAreAccepted(t *testing.T) {
	s, _ := openStore(t)
	in := customSettings()
	in.PollInterval, in.Grace = Duration(5*time.Second), Duration(30*time.Second)

	require.NoError(t, s.SaveSettings(in))

	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, Duration(5*time.Second), got.PollInterval)
	require.Equal(t, Duration(30*time.Second), got.Grace)
}

// A build that raised a minimum meets settings written under the old one: it
// loads them with the value raised and says so, as a file that cannot be loaded
// would hold the whole daemon with no command to repair it.
func TestSettingsOnDiskBelowTheMinimumsAreRaisedAndNoted(t *testing.T) {
	file := func(p Paths) string { return filepath.Join(p.Cluster, "meta", "settings.json") }
	tests := []struct {
		name  string
		data  string
		poll  time.Duration
		grace time.Duration
		notes func(file string) []string
	}{
		{"poll interval", `{"gateTag":"cf-tunnel","pollInterval":"1s","grace":"1m","admission":"tag","observeOnly":true}`,
			5 * time.Second, time.Minute, func(f string) []string {
				return []string{"settings: pollInterval is 1s in " + f + ", below the minimum of 5s; 5s is used until it is raised there"}
			}},
		{"grace", `{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"60ms","admission":"tag","observeOnly":true}`,
			10 * time.Second, 30 * time.Second, func(f string) []string {
				return []string{"settings: grace is 60ms in " + f + ", below the minimum of 30s; 30s is used until it is raised there"}
			}},
		{"zero grace", `{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"0s","admission":"tag","observeOnly":true}`,
			10 * time.Second, 30 * time.Second, func(f string) []string {
				return []string{"settings: grace is 0s in " + f + ", below the minimum of 30s; 30s is used until it is raised there"}
			}},
		{"negative poll interval", `{"gateTag":"cf-tunnel","pollInterval":"-3s","grace":"1m","admission":"tag","observeOnly":true}`,
			5 * time.Second, time.Minute, func(f string) []string {
				return []string{"settings: pollInterval is -3s in " + f + ", below the minimum of 5s; 5s is used until it is raised there"}
			}},
		{"both", `{"gateTag":"cf-tunnel","pollInterval":"1s","grace":"10s","admission":"tag","observeOnly":true}`,
			5 * time.Second, 30 * time.Second, func(f string) []string {
				return []string{
					"settings: pollInterval is 1s in " + f + ", below the minimum of 5s; 5s is used until it is raised there",
					"settings: grace is 10s in " + f + ", below the minimum of 30s; 30s is used until it is raised there",
				}
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, p := openStore(t)
			raw := envelopeJSON("settings", tc.data)
			writeFile(t, file(p), raw)

			got, notes, err := s.LoadSettings()

			require.NoError(t, err)
			require.Equal(t, tc.notes(file(p)), notes)
			require.Equal(t, Duration(tc.poll), got.PollInterval)
			require.Equal(t, Duration(tc.grace), got.Grace)
			require.Equal(t, "cf-tunnel", got.GateTag, "nothing else changes")
			require.True(t, got.ObserveOnly)

			plain, err := s.Settings()
			require.NoError(t, err)
			require.Equal(t, got, plain, "Settings says the same without the notes")

			onDisk, err := os.ReadFile(file(p))
			require.NoError(t, err)
			require.Equal(t, raw, string(onDisk), "a load writes nothing")
		})
	}
}

// A cap of hostnames below one, written by hand, is raised as the durations
// are: a file no command can repair would hold the daemon.
func TestACapOfHostnamesOnDiskBelowOneIsRaisedAndNoted(t *testing.T) {
	file := filepath.Join("meta", "settings.json")
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, file), envelopeJSON("settings",
		`{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true,"maxHostnamesPerGuest":0}`))

	got, notes, err := s.LoadSettings()

	require.NoError(t, err)
	require.Equal(t, 1, got.MaxHostnamesPerGuest)
	require.Equal(t, []string{"settings: maxHostnamesPerGuest is 0 in " + filepath.Join(p.Cluster, file) +
		", below the minimum of 1; 1 is used until it is raised there"}, notes)
}

func TestACapOfHostnamesIsKeptAndALeftOutOneIsTheDefault(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "meta", "settings.json")
	writeFile(t, path, envelopeJSON("settings",
		`{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true,"maxHostnamesPerGuest":5}`))
	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, 5, got.MaxHostnamesPerGuest)

	writeFile(t, path, envelopeJSON("settings", `{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true}`))
	got, err = s.Settings()
	require.NoError(t, err)
	require.Equal(t, 32, got.MaxHostnamesPerGuest)

	writeFile(t, path, envelopeJSON("settings",
		`{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true,"maxHostnamePerGuest":5}`))
	_, err = s.Settings()
	require.ErrorContains(t, err, "maxHostnamePerGuest", "a misspelt key is refused, as any unknown one")
}

func TestSettingsAtTheirMinimumsAndAboveNeedNoNote(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"),
		envelopeJSON("settings", `{"gateTag":"cf-tunnel","pollInterval":"5s","grace":"30s","admission":"tag","observeOnly":true}`))

	got, notes, err := s.LoadSettings()

	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, Duration(5*time.Second), got.PollInterval)
	require.Equal(t, Duration(30*time.Second), got.Grace)
}

func TestLoadSettingsOfNothingStoredIsTheDefaultsWithoutNotes(t *testing.T) {
	s, _ := openStore(t)

	got, notes, err := s.LoadSettings()

	require.NoError(t, err)
	require.Empty(t, notes)
	require.Equal(t, DefaultSettings(), got)
}

func TestRaisingAValueDoesNotExcuseAnotherInvalidOne(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"),
		envelopeJSON("settings", `{"gateTag":"","pollInterval":"1s","grace":"1m","admission":"tag","observeOnly":true}`))

	got, notes, err := s.LoadSettings()

	require.ErrorContains(t, err, "stored settings are invalid: gateTag")
	require.Equal(t, Settings{}, got)
	require.Empty(t, notes)
}

func TestInvalidSettingsOnDiskAreAnErrorNotTheDefaults(t *testing.T) {
	tests := map[string]string{
		"empty gate tag":    `{"gateTag":"","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true}`,
		"unknown admission": `{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"open","observeOnly":true}`,
		"bad duration":      `{"gateTag":"cf-tunnel","pollInterval":"ten","grace":"1m","admission":"tag","observeOnly":true}`,
		"numeric duration":  `{"gateTag":"cf-tunnel","pollInterval":10,"grace":"1m","admission":"tag","observeOnly":true}`,
		"bad pattern":       `{"gateTag":"cf-tunnel","allowHosts":["a b"],"pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true}`,
		"bad prefix":        `{"gateTag":"cf-tunnel","trustedCIDRs":["nope"],"pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true}`,
		"ipv6 manual":       `{"gateTag":"cf-tunnel","manualCIDRs":["fd00::/8"],"pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true}`,
		"ipv6 prefix":       `{"gateTag":"cf-tunnel","trustedCIDRs":["fd00::/8"],"pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true}`,
		"unknown minimum":   `{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true,"identityMinimum":"none"}`,
		"empty minimum":     `{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true,"identityMinimum":""}`,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings", data))
			got, err := s.Settings()
			require.Error(t, err)
			require.Equal(t, Settings{}, got)
		})
	}

	t.Run("unreadable file", func(t *testing.T) {
		s, p := openStore(t)
		writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), `{"schemaVersion":1,`)
		_, err := s.Settings()
		require.Error(t, err)
		require.Contains(t, err.Error(), "settings.json")
	})
}

func TestSettingsFillsInTheFieldsAFileLeavesOut(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"),
		envelopeJSON("settings", `{"gateTag":"web","allowHosts":["Shop.CZ"]}`))

	got, err := s.Settings()
	require.NoError(t, err)
	want := DefaultSettings()
	want.GateTag = "web"
	want.AllowHosts = []string{"shop.cz"}
	require.Equal(t, want, got)
	require.Equal(t, "port", got.IdentityMinimum, "settings of an older version ask for the default")
}

func TestDurationJSON(t *testing.T) {
	type wrap struct {
		D Duration `json:"d"`
	}
	tests := []struct {
		d    time.Duration
		text string
	}{
		{10 * time.Second, `"10s"`},
		{time.Minute, `"1m0s"`},
		{90 * time.Minute, `"1h30m0s"`},
		{1500 * time.Millisecond, `"1.5s"`},
		{0, `"0s"`},
	}
	for _, tc := range tests {
		b, err := json.Marshal(wrap{D: Duration(tc.d)})
		require.NoError(t, err)
		require.JSONEq(t, `{"d":`+tc.text+`}`, string(b))

		var back wrap
		require.NoError(t, json.Unmarshal(b, &back))
		require.Equal(t, Duration(tc.d), back.D)
	}

	var w wrap
	require.NoError(t, json.Unmarshal([]byte(`{"d":"2h45m"}`), &w))
	require.Equal(t, Duration(2*time.Hour+45*time.Minute), w.D)

	for _, bad := range []string{`{"d":"ten"}`, `{"d":""}`, `{"d":10}`, `{"d":true}`, `{"d":"10"}`, `{"d":{}}`} {
		require.Error(t, json.Unmarshal([]byte(bad), &w), bad)
	}
}

func TestATextThatIsNoDurationIsADurationError(t *testing.T) {
	var d Duration
	for _, text := range []string{"ten", "", "10", "1 m"} {
		var de *DurationError
		require.ErrorAs(t, d.UnmarshalText([]byte(text)), &de, text)
	}
	var w struct {
		D Duration `json:"d"`
	}
	var de *DurationError
	require.ErrorAs(t, json.Unmarshal([]byte(`{"d":"ten"}`), &w), &de, "also when it is read from JSON")
	require.ErrorContains(t, de, "invalid duration", "the message is that of time.ParseDuration")

	require.NoError(t, d.UnmarshalText([]byte("1m30s")))
	require.Equal(t, Duration(90*time.Second), d)
}

func TestSettingsRefuseAnUnknownKey(t *testing.T) {
	const rest = `"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m","admission":"tag","observeOnly":true`
	for name, tc := range map[string]struct{ data, key string }{
		"misspelt deny list":  {`{` + rest + `,"denyhost":["admin.example.com"]}`, "denyhost"},
		"misspelt allow list": {`{` + rest + `,"allowhost":["shop.cz"]}`, "allowhost"},
		"invented key":        {`{` + rest + `,"somethingElse":1}`, "somethingElse"},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings", tc.data))
			got, err := s.Settings()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.key)
			require.Equal(t, Settings{}, got)
		})
	}
}

func TestSettingsStillReadWhatSaveSettingsWrites(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveSettings(customSettings()))
	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, customSettings(), got)
}
