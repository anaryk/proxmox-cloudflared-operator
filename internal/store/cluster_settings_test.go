package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// previousSettings are the settings as the previous release declares them.
// It decodes the file strictly, so a key it does not know refuses the file.
type previousSettings struct {
	GateTag              string            `json:"gateTag"`
	AllowHosts           []string          `json:"allowHosts,omitempty"`
	DenyHosts            []string          `json:"denyHosts,omitempty"`
	PollInterval         Duration          `json:"pollInterval"`
	Grace                Duration          `json:"grace"`
	TrustStatic          bool              `json:"trustStatic,omitempty"`
	TrustedCIDRs         []netip.Prefix    `json:"trustedCIDRs,omitempty"`
	ManualCIDRs          []netip.Prefix    `json:"manualCIDRs,omitempty"`
	Admission            string            `json:"admission"`
	ZonePins             map[string]string `json:"zonePins,omitempty"`
	ObserveOnly          bool              `json:"observeOnly"`
	IdentityMinimum      string            `json:"identityMinimum"`
	MaxHostnamesPerGuest int               `json:"maxHostnamesPerGuest"`
	ReverifyInterval     Duration          `json:"reverifyInterval"`
	CloudflareBudget     int               `json:"cloudflareBudget"`
	Managed              ManagedSettings   `json:"managed,omitzero"`
}

func defaultCluster() ClusterSettings {
	return ClusterSettings{FollowMaintenance: boolp(true), RotateOnForget: boolp(true), AdmissionMaxMoves: 10}
}

func TestTheClusterSettingsHaveTheirDefaults(t *testing.T) {
	d := DefaultSettings().Cluster
	require.Equal(t, defaultCluster(), d)
	require.False(t, d.AgentNetwork.IsValid(), "the management address")
	require.Empty(t, d.Replicas, "every Active node")
	require.True(t, d.FollowsMaintenance())
	require.True(t, d.RotatesOnForget())
	require.Equal(t, 10, d.MaxMoves())
	require.True(t, d.IsZero(), "defaults are what a file without the block means")

	var unset ClusterSettings
	require.True(t, unset.FollowsMaintenance())
	require.True(t, unset.RotatesOnForget())
	require.Equal(t, 10, unset.MaxMoves())
	require.True(t, unset.IsZero())

	off := ClusterSettings{FollowMaintenance: boolp(false)}
	require.False(t, off.FollowsMaintenance())
	require.False(t, off.IsZero())
}

// The previous release refuses a settings file with a key it does not know.
// A file this release writes while the cluster settings have their defaults
// is the file the previous release wrote.
func TestSettingsWithTheClusterDefaultsAreWhatThePreviousReleaseReads(t *testing.T) {
	s, p := openStore(t)
	for _, v := range []Settings{DefaultSettings(), customSettings()} {
		v.Cluster = ClusterSettings{}
		require.NoError(t, s.SaveSettings(v))
		data := readRaw(t, filepath.Join(p.Cluster, "meta", "settings.json")).Data
		require.NotContains(t, string(data), "cluster")
		var previous previousSettings
		require.NoError(t, decode(data, &previous, true))

		v.Cluster = defaultCluster()
		b, err := json.Marshal(v)
		require.NoError(t, err)
		require.NotContains(t, string(b), "cluster", "set to its defaults the block is left out")
		w, err := json.Marshal(previous)
		require.NoError(t, err)
		require.JSONEq(t, string(w), string(b))
	}
}

func TestAFileWithoutTheClusterSettingsLoadsWithTheirDefaults(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings",
		`{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m0s","admission":"tag","observeOnly":true,`+
			`"identityMinimum":"port","maxHostnamesPerGuest":32,"reverifyInterval":"1m0s","cloudflareBudget":1000}`))

	got, err := s.Settings()

	require.NoError(t, err)
	require.Equal(t, DefaultSettings(), got)
	require.Equal(t, defaultCluster(), got.Cluster)
}

func TestTheClusterSettingsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	want := customSettings()
	want.Cluster = ClusterSettings{
		AgentNetwork:      netip.MustParsePrefix("10.99.0.0/24"),
		Replicas:          []string{"pve-a", "pve-b"},
		FollowMaintenance: boolp(false),
		RotateOnForget:    boolp(true),
		AdmissionMaxMoves: 50,
	}
	require.NoError(t, s.SaveSettings(want))

	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, want, got)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(readRaw(t, filepath.Join(p.Cluster, "meta", "settings.json")).Data, &wire))
	require.Equal(t, map[string]any{
		"agentNetwork": "10.99.0.0/24", "replicas": []any{"pve-a", "pve-b"},
		"followMaintenance": false, "rotateOnForget": true, "admissionMaxMoves": float64(50),
	}, wire["cluster"])

	six := want
	six.Cluster = ClusterSettings{AgentNetwork: netip.MustParsePrefix("fd00:99::/64")}
	require.NoError(t, s.SaveSettings(six))
	got, err = s.Settings()
	require.NoError(t, err)
	require.Equal(t, ClusterSettings{
		AgentNetwork: netip.MustParsePrefix("fd00:99::/64"), FollowMaintenance: boolp(true), RotateOnForget: boolp(true), AdmissionMaxMoves: 10,
	}, got.Cluster, "an IPv6 prefix, and the defaults filled in")
}

func TestTheClusterSettingsRefuseWhatDoesNotFit(t *testing.T) {
	names := make([]string, MaxReplicas+1)
	for i := range names {
		names[i] = fmt.Sprintf("pve%d", i)
	}
	for _, tt := range []struct {
		name  string
		field string
		edit  func(*ClusterSettings)
	}{
		{"a prefix that is not its network", "cluster.agentNetwork", func(c *ClusterSettings) { c.AgentNetwork = netip.MustParsePrefix("10.99.0.1/24") }},
		{"a prefix that is not valid", "cluster.agentNetwork", func(c *ClusterSettings) { c.AgentNetwork = netip.PrefixFrom(netip.MustParseAddr("10.99.0.0"), 40) }},
		{"no move at a time", "cluster.admissionMaxMoves", func(c *ClusterSettings) { c.AdmissionMaxMoves = -1 }},
		{"1001 moves", "cluster.admissionMaxMoves", func(c *ClusterSettings) { c.AdmissionMaxMoves = 1001 }},
		{"more than 25 replicas", "cluster.replicas", func(c *ClusterSettings) { c.Replicas = names }},
		{"an empty node", "cluster.replicas[1]", func(c *ClusterSettings) { c.Replicas = []string{"pve1", ""} }},
		{"a node with a dot", "cluster.replicas[0]", func(c *ClusterSettings) { c.Replicas = []string{"pve.lan"} }},
		{"a node twice", "cluster.replicas[1]", func(c *ClusterSettings) { c.Replicas = []string{"pve1", "pve1"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			v := customSettings()
			tt.edit(&v.Cluster)

			err := s.SaveSettings(v)

			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			require.Equal(t, tt.field, fe.Field)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
	s, _ := openStore(t)
	v := customSettings()
	v.Cluster = ClusterSettings{Replicas: names[:MaxReplicas], AdmissionMaxMoves: 1000}
	require.NoError(t, s.SaveSettings(v), "25 replicas and 1000 moves are allowed")
	v.Cluster.AdmissionMaxMoves = 1
	require.NoError(t, s.SaveSettings(v))
}

// A key that is there is taken as written: zero or empty is refused, never
// read as the default.
func TestTheClusterSettingsInAFileAreTakenAsWritten(t *testing.T) {
	const base = `{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m0s","admission":"tag","observeOnly":true,` +
		`"identityMinimum":"port","maxHostnamesPerGuest":32,"reverifyInterval":"1m0s","cloudflareBudget":1000`
	for _, tt := range []struct {
		name, block, field string
	}{
		{"0 moves", `{"admissionMaxMoves":0}`, "cluster.admissionMaxMoves"},
		{"1001 moves", `{"admissionMaxMoves":1001}`, "cluster.admissionMaxMoves"},
		{"an empty prefix", `{"agentNetwork":""}`, "cluster.agentNetwork"},
		{"26 replicas", `{"replicas":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u","v","w","x","y","z"]}`, "cluster.replicas"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings", base+`,"cluster":`+tt.block+`}`))

			got, err := s.Settings()

			require.ErrorContains(t, err, "invalid: "+tt.field)
			require.Equal(t, Settings{}, got)

			var sent Settings
			err = json.Unmarshal([]byte(base+`,"cluster":`+tt.block+`}`), &sent)
			var fe *FieldError
			require.ErrorAs(t, err, &fe, "what a client sends is named by its field")
			require.Equal(t, tt.field, fe.Field)
		})
	}

	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings", base+`,"cluster":{"admissionMaxMoves":20,"replicas":[]}}`))
	got, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, ClusterSettings{FollowMaintenance: boolp(true), RotateOnForget: boolp(true), AdmissionMaxMoves: 20}, got.Cluster,
		"a key left out has its default, and an empty list is every node")

	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings", base+`,"cluster":null}`))
	got, err = s.Settings()
	require.NoError(t, err)
	require.Equal(t, defaultCluster(), got.Cluster, "null is the block left out")
}

func TestAnUnknownClusterSettingIsRefused(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings",
		`{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m0s","admission":"tag","observeOnly":true,"identityMinimum":"port",`+
			`"maxHostnamesPerGuest":32,"reverifyInterval":"1m0s","cloudflareBudget":1000,"cluster":{"replica":["pve1"]}}`))

	_, err := s.Settings()

	require.ErrorContains(t, err, `"replica"`)
}

func TestAClusterSettingOfTheWrongTypeIsNamedByItsPath(t *testing.T) {
	var v Settings
	err := json.Unmarshal([]byte(`{"cluster":{"admissionMaxMoves":"ten"}}`), &v)

	var typ *json.UnmarshalTypeError
	require.True(t, errors.As(err, &typ), "%v", err)
	require.Equal(t, "cluster.admissionMaxMoves", typ.Field)
}

func TestTheClusterSettingsAreSavedOnlyAsTheyChange(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "meta", "settings.json")
	v := DefaultSettings()
	require.NoError(t, s.SaveSettings(v))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	v.Cluster = ClusterSettings{AdmissionMaxMoves: 10, FollowMaintenance: boolp(true)}
	require.NoError(t, s.SaveSettings(v))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(before, after), "the defaults written out are still the defaults")
}
