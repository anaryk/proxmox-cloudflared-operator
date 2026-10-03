package credentials

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

func leftOut(s scope) Exclusion {
	return Exclusion{Zone: s.name, ZoneID: s.id, Reason: "no DNS read", Detail: "grant Zone > DNS > Edit on " + s.name}
}

// The common token for pco lists every zone and edits the DNS of a few.
func TestAZoneWhoseDNSCannotBeReadIsLeftOut(t *testing.T) {
	for _, deep := range []bool{false, true} {
		f := newFake()
		f.AddZone("zone2", "example.org", "acct1")
		f.Deny("dns.read", "zone2")

		got := newChecker().Run(t.Context(), f, deep)

		want := []Check{
			ok(CapToken, scope{}),
			ok(CapAccounts, scope{}),
			ok(CapZones, scope{}),
			ok(CapDNSRead, exampleCom),
			ok(CapTunnelRead, acme),
		}
		if deep {
			want = []Check{
				ok(CapToken, scope{}),
				ok(CapAccounts, scope{}),
				ok(CapZones, scope{}),
				ok(CapDNSRead, exampleCom),
				ok(CapDNSWrite, exampleCom),
				ok(CapTunnelRead, acme),
				ok(CapTunnelWrite, acme),
			}
		}
		require.Equal(t, want, got.Checks, "deep %v", deep)
		require.Equal(t, []Exclusion{leftOut(exampleOrg)}, got.Excluded)
		require.True(t, got.Usable, "deep %v", deep)
		require.False(t, got.Unanswered())
		require.Len(t, got.Zones, 2, "the zone is still one the token sees")
		require.Empty(t, callsTo(f, "CreateRecord zone2"), "no write is tried where nothing can be read")
	}
}

func TestTheAccountOfZonesLeftOutIsNotProbed(t *testing.T) {
	f := newFake()
	f.AddAccount("acct2", "Beta")
	f.AddZone("zone2", "example.org", "acct2")
	f.Deny("dns.read", "zone2")
	f.Deny("tunnel.read", "acct2")

	got := newChecker().Run(t.Context(), f, true)

	require.True(t, got.Usable, "%v", got.Checks)
	require.Empty(t, failures(got))
	require.Empty(t, callsTo(f, "FindTunnel acct2", "CreateTunnel acct2"))
	for _, check := range got.Checks {
		require.NotEqual(t, "Beta", check.Scope)
	}
}

func TestATokenThatCanReadTheDNSOfNoZoneIsNotUsable(t *testing.T) {
	f := newFake()
	f.AddZone("zone2", "example.org", "acct1")
	f.Deny("dns.read")

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, []Check{
		ok(CapToken, scope{}),
		ok(CapAccounts, scope{}),
		ok(CapZones, scope{}),
		failed(CapDNSRead, scope{}, "token can read the DNS of no zone it lists; grant Zone > DNS > Edit on the zones to manage"),
	}, got.Checks)
	require.Equal(t, []Exclusion{leftOut(exampleCom), leftOut(exampleOrg)}, got.Excluded, "sorted by zone")
	require.False(t, got.Usable)
	require.False(t, got.Unanswered(), "a refusal is an answer")
	require.Empty(t, callsTo(f, "FindTunnel", "CreateRecord", "CreateTunnel"))
}

// Only a permission error leaves a zone out: a read Cloudflare did not answer
// says nothing of the token.
func TestAZoneWhoseReadWasNotAnsweredIsNotLeftOut(t *testing.T) {
	for _, injected := range []error{
		&cfapi.Error{Status: http.StatusServiceUnavailable, Message: "unavailable"},
		&cfapi.Error{Status: http.StatusTooManyRequests, Message: "rate limited"},
	} {
		t.Run(injected.Error(), func(t *testing.T) {
			f := newFake()
			f.AddZone("zone2", "example.org", "acct1")
			f.Deny("dns.read", "zone2")
			// example.com is read first.
			f.FailNext("dns.read", 1, injected)

			got := newChecker().Run(t.Context(), f, false)

			check := find(t, got, CapDNSRead, exampleCom)
			require.False(t, check.OK)
			require.True(t, check.Unanswered)
			require.Equal(t, []Exclusion{leftOut(exampleOrg)}, got.Excluded)
			require.Equal(t, []Check{check}, failures(got), "no zone is said to be unreadable")
			require.False(t, got.Usable)
			require.True(t, got.Unanswered())
		})
	}
}

func TestAZoneLeftOutIsNoFailure(t *testing.T) {
	f := newFake()
	f.AddZone("zone2", "example.org", "acct1")
	f.Deny("dns.read", "zone2")
	f.Deny("dns.write", "zone1")

	got := newChecker().Run(t.Context(), f, true)

	require.Equal(t, []Check{failed(CapDNSWrite, exampleCom, "grant Zone > DNS > Edit on example.com")}, failures(got),
		"a zone that is served still needs every check")
	require.Equal(t, []Exclusion{leftOut(exampleOrg)}, got.Excluded)
	require.False(t, got.Usable)
}

func TestLeftOutNamesTheZonesByReason(t *testing.T) {
	tests := []struct {
		name     string
		excluded []Exclusion
		want     string
	}{
		{"none", nil, ""},
		{"one", []Exclusion{leftOut(exampleCom)}, "example.com left out: no DNS read"},
		{"two", []Exclusion{leftOut(exampleCom), leftOut(exampleOrg)}, "example.com, example.org left out: no DNS read"},
		{"two reasons", []Exclusion{leftOut(exampleCom), {Zone: "example.net", Reason: "other"}, leftOut(exampleOrg)},
			"example.com, example.org left out: no DNS read; example.net left out: other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Report{Excluded: tt.excluded}.LeftOut())
		})
	}
}

func TestTheZonesLeftOutAreInTheJSON(t *testing.T) {
	raw, err := json.Marshal(Report{Excluded: []Exclusion{leftOut(exampleOrg)}})
	require.NoError(t, err)

	var got struct{ Excluded json.RawMessage }
	require.NoError(t, json.Unmarshal(raw, &got))
	require.JSONEq(t, `[{"zone":"example.org","zoneId":"zone2","reason":"no DNS read","detail":"grant Zone > DNS > Edit on example.org"}]`,
		string(got.Excluded))
}
