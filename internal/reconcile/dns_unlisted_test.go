package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// failingLookups fails every lookup of the records of one name.
type failingLookups struct{ cfapi.API }

func (f failingLookups) Records(ctx context.Context, zoneID string, filter cfapi.RecordFilter) ([]cfapi.Record, error) {
	if filter.Name != "" {
		return nil, errors.New("503 Service Unavailable")
	}
	return f.API.Records(ctx, zoneID, filter)
}

func TestDNSUnlistedNamesTheZonesItCouldNotRead(t *testing.T) {
	cases := []struct {
		name string
		api  func() cfapi.API
		want []string
	}{
		{name: "every zone read", api: func() cfapi.API { return newDNSFake() }},
		{name: "the listing of a zone fails", want: []string{zone1.Name}, api: func() cfapi.API {
			f := newDNSFake()
			f.FailNext("dns.read", 1, errors.New("503 Service Unavailable"))
			return f
		}},
		{name: "the lookup of a name fails", want: []string{zone1.Name}, api: func() cfapi.API {
			return failingLookups{newDNSFake()}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []Mode{Observe, Enforce} {
				r := newDNS(tc.api(), &memStore{}, t0)

				res := r.Run(context.Background(), dnsIn("app.example.com"), mode)

				require.True(t, res.Looked)
				require.Equal(t, tc.want, res.Unlisted)
			}
		})
	}
}

func TestDNSUnlistedNamesAZoneWithoutAClient(t *testing.T) {
	in := dnsIn("app.example.com")
	in.Zones = append(in.Zones, ZoneRef{ID: "zone3", Name: "other.org", CredentialID: "cred9"})

	res := newDNS(newDNSFake(), &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Equal(t, []string{"other.org"}, res.Unlisted)
}
