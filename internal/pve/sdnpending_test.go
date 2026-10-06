package pve

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClusterOptionsGiveTheRegisteredTags(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       []string
	}{
		{"several", `{"data":{"keyboard":"en-us","registered-tags":"cf-tunnel;cf-tunnel-managed;Prod"}}`, []string{"cf-tunnel", "cf-tunnel-managed", "Prod"}},
		{"none", `{"data":{"keyboard":"en-us"}}`, nil},
		{"empty", `{"data":{"registered-tags":""}}`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := newTestClient(t, map[string]reply{"/cluster/options": {http.StatusOK, tt.body}})

			got, err := c.ClusterOptions(context.Background())

			require.NoError(t, err)
			require.Equal(t, ClusterOptions{RegisteredTags: tt.want}, got)
			require.Equal(t, "/api2/json/cluster/options", rec.requests()[0].uri)
		})
	}

	c, _ := newTestClient(t, map[string]reply{"/cluster/options": {http.StatusOK, `{"data":[]}`}})
	_, err := c.ClusterOptions(context.Background())
	require.Error(t, err)
}

// What pvesh prints is the data without the envelope of the API.
func TestDecodeClusterOptions(t *testing.T) {
	got, err := DecodeClusterOptions([]byte(`{"registered-tags":"a;b"}`))
	require.NoError(t, err)
	require.Equal(t, ClusterOptions{RegisteredTags: []string{"a", "b"}}, got)

	_, err = DecodeClusterOptions([]byte(`"a;b"`))
	require.Error(t, err)
}

// The pending view lists every object with the values it will have once
// applied: a value that is pending wins over the applied one, and one pending
// as "deleted" is gone.
func TestSDNPendingReadsWhatIsNotApplied(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/cluster/sdn/zones?pending=1":                okReply(t, "sdn_zones_pending.json"),
		"/cluster/sdn/vnets?pending=1":                okReply(t, "sdn_vnets_pending.json"),
		"/cluster/sdn/vnets/pcotv1/subnets?pending=1": okReply(t, "sdn_subnets_pending.json"),
		"/cluster/sdn/vnets/pconet/subnets?pending=1": {http.StatusOK, `{"data":[]}`},
		"/cluster/sdn/vnets/blue/subnets?pending=1":   {http.StatusOK, `{"data":[]}`},
	})

	got, err := c.SDNPending(context.Background())

	require.NoError(t, err)
	require.Equal(t, SDN{
		Zones: []SDNZone{
			{Name: "pcotest", Type: "simple"},
			{Name: "pco", Type: "simple", State: SDNNew},
			{Name: "tenants", Type: "vlan", State: SDNDeleted},
		},
		Vnets: []SDNVnet{
			{Name: "pcotv1", Zone: "pcotest"},
			{Name: "pconet", Zone: "pco", IsolatePorts: true, State: SDNNew},
			{Name: "blue", Zone: "pcotest", VLANAware: true, State: SDNChanged},
		},
		Subnets: []SDNSubnet{
			{Vnet: "pcotv1", CIDR: "10.92.1.0/24"},
			{Vnet: "pcotv1", CIDR: "10.92.2.0/24", State: SDNNew},
		},
	}, got)
	require.Len(t, rec.requests(), 5)
	require.Equal(t, []string{"zone pco (new)", "zone tenants (deleted)", "vnet pconet (new)", "vnet blue (changed)",
		"subnet 10.92.2.0/24 of vnet pcotv1 (new)"}, got.Pending())
}

func TestSDNVnetsAreTheConfiguredOnes(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/cluster/sdn/vnets": {http.StatusOK,
		`{"data":[{"vnet":"pconet","zone":"pco","type":"vnet","isolate-ports":1},{"vnet":"pcotv1","zone":"pcotest","type":"vnet"}]}`}})

	got, err := c.SDNVnets(context.Background())

	require.NoError(t, err)
	require.Equal(t, []SDNVnet{{Name: "pconet", Zone: "pco", IsolatePorts: true}, {Name: "pcotv1", Zone: "pcotest"}}, got)
	require.Len(t, rec.requests(), 1)
}

func TestSDNDecodersRefuseBadAnswers(t *testing.T) {
	for _, tt := range []struct {
		name   string
		decode func([]byte) error
		body   string
	}{
		{"a zone without a name", func(b []byte) error { _, err := DecodeSDNZones(b); return err }, `[{"type":"simple"}]`},
		{"a zone name leaving its segment", func(b []byte) error { _, err := DecodeSDNZones(b); return err }, `[{"zone":"../x","type":"simple"}]`},
		{"a vnet without a zone", func(b []byte) error { _, err := DecodeSDNVnets(b); return err }, `[{"vnet":"a"}]`},
		{"a vnet whose zone is pending as deleted", func(b []byte) error { _, err := DecodeSDNVnets(b); return err },
			`[{"vnet":"a","zone":"z","state":"changed","pending":{"zone":"deleted"}}]`},
		{"an unknown state", func(b []byte) error { _, err := DecodeSDNVnets(b); return err }, `[{"vnet":"a","zone":"z","state":"odd"}]`},
		{"a subnet without its cidr", func(b []byte) error { _, err := DecodeSDNSubnets(b); return err }, `[{"vnet":"a"}]`},
		{"not a list", func(b []byte) error { _, err := DecodeSDNZones(b); return err }, `{"zone":"a"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, tt.decode([]byte(tt.body)))
		})
	}
}

// lockServer answers the lock calls of the SDN and remembers them with their
// parameters, from the body or from the query.
func lockServer(t *testing.T, status int, body string) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	c := startServer(t, testConfig(""), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		form, err := url.ParseQuery(string(raw))
		require.NoError(t, err)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" query="+r.URL.RawQuery+" body="+form.Encode())
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestSDNLockTakesTheLockWithPendingChanges(t *testing.T) {
	c, seen := lockServer(t, http.StatusOK, `{"data":"7a3c1f0e-2b4d-4e6f-8a9b-0c1d2e3f4a5b"}`)

	token, err := c.SDNLock(t.Context())

	require.NoError(t, err)
	require.Equal(t, "7a3c1f0e-2b4d-4e6f-8a9b-0c1d2e3f4a5b", token)
	require.Equal(t, []string{"POST /api2/json/cluster/sdn/lock query= body=allow-pending=1"}, seen())
}

func TestSDNUnlockReleasesItsToken(t *testing.T) {
	c, seen := lockServer(t, http.StatusOK, `{"data":null}`)

	require.NoError(t, c.SDNUnlock(t.Context(), "7a3c1f0e"))

	require.Equal(t, []string{"DELETE /api2/json/cluster/sdn/lock query=lock-token=7a3c1f0e body="}, seen())
}

func TestSDNLockRefusals(t *testing.T) {
	c, _ := lockServer(t, http.StatusForbidden, `{"data":null,"message":"Permission check failed (/sdn, SDN.Allocate)"}`)
	_, err := c.SDNLock(t.Context())
	require.True(t, IsForbidden(err), "%v", err)
	require.Error(t, c.SDNUnlock(t.Context(), "x"))

	empty, _ := lockServer(t, http.StatusOK, `{"data":""}`)
	_, err = empty.SDNLock(t.Context())
	require.ErrorContains(t, err, "no lock token")

	require.ErrorContains(t, c.SDNUnlock(t.Context(), ""), "no lock token")
}
