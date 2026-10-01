package cfapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// tunnelEndpoint fakes the listing of the tunnels of an account the way the
// API description has it: result_info holds page, per_page, count and
// total_count and no total_pages, the name filter matches more than the exact
// name, and total_count counts every tunnel of the account whatever the
// filters. filteredTotals makes total_count count the filtered result
// instead, the other reading of the description. maxPerPage, when set, is the
// largest page the server gives, whatever is asked.
type tunnelEndpoint struct {
	names          []string
	filteredTotals bool
	maxPerPage     int
}

func (e *tunnelEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, size := pageParams(r)
	if e.maxPerPage > 0 {
		size = min(size, e.maxPerPage)
	}
	var matched []string
	for _, n := range e.names {
		if name := q.Get("name"); name != "" && !strings.Contains(n, name) {
			continue
		}
		if prefix := q.Get("include_prefix"); prefix != "" && !strings.HasPrefix(n, prefix) {
			continue
		}
		matched = append(matched, n)
	}
	total := len(e.names)
	if e.filteredTotals {
		total = len(matched)
	}
	lo := min((page-1)*size, len(matched))
	hi := min(page*size, len(matched))
	reply(http.StatusOK, tunnelPage(matched[lo:hi], fmt.Sprintf(
		`{"page":%d,"per_page":%d,"count":%d,"total_count":%d}`, page, size, hi-lo, total)))(w, r)
}

// tunnelPage is a page of a tunnel listing that holds tunnels of the names.
func tunnelPage(names []string, info string) string {
	items := make([]string, len(names))
	for i, n := range names {
		items[i] = fmt.Sprintf(`{"id":"id-%s","name":%q,"status":"inactive","created_at":"2026-02-03T04:05:06Z","deleted_at":null}`, n, n)
	}
	return `{"success":true,"errors":[],"result":[` + strings.Join(items, ",") + `],"result_info":` + info + `}`
}

func named(prefix string, n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("%s%03d", prefix, i)
	}
	return names
}

func tunnelNames(tunnels []Tunnel) []string {
	var names []string
	for _, t := range tunnels {
		names = append(names, t.Name)
	}
	return names
}

// readings are the two readings of what total_count of a tunnel listing
// counts.
var readings = []struct {
	name     string
	filtered bool
}{
	{"total of every tunnel", false},
	{"total of the filtered result", true},
}

func TestTunnelListingsAmongManyOtherTunnels(t *testing.T) {
	for _, reading := range readings {
		t.Run(reading.name, func(t *testing.T) {
			names := append(named("other-", 120), "pco-abc", "pco-abc_probe_1", "pco-abc_probe_2")
			env := setup(t, (&tunnelEndpoint{names: names, filteredTotals: reading.filtered}).serve)

			got, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "pco-abc", got.Name)

			probes, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")
			require.NoError(t, err)
			require.Equal(t, []string{"pco-abc_probe_1", "pco-abc_probe_2"}, tunnelNames(probes))
			require.Len(t, env.requests(), 2, "one page each")
		})
	}
}

func TestTunnelListingReadsEveryPage(t *testing.T) {
	cases := []struct {
		name       string
		probes     int
		maxPerPage int
		requests   int
	}{
		{"three pages, the last one short", 120, 0, 3},
		{"two full pages and an empty one", 100, 0, 3},
		{"one full page and an empty one", 50, 0, 2},
		{"one short page", 49, 0, 1},
		{"nothing", 0, 0, 1},
		{"the server gives smaller pages than asked", 60, 25, 3},
	}
	for _, reading := range readings {
		for _, tc := range cases {
			t.Run(reading.name+"/"+tc.name, func(t *testing.T) {
				probes := named("pco-abc_probe_", tc.probes)
				e := &tunnelEndpoint{names: append(named("other-", 120), probes...), filteredTotals: reading.filtered, maxPerPage: tc.maxPerPage}
				env := setup(t, e.serve)

				got, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")

				require.NoError(t, err)
				require.Len(t, got, tc.probes)
				if tc.probes > 0 {
					require.Equal(t, probes, tunnelNames(got), "every one, in order")
				}
				require.Len(t, env.requests(), tc.requests)
			})
		}
	}
}

func TestTunnelListingThatNeverEndsHitsThePageCap(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		page, size := pageParams(r)
		reply(http.StatusOK, tunnelPage(named(fmt.Sprintf("pco-abc_probe_%d_", page), size), fmt.Sprintf(
			`{"page":%d,"per_page":%d,"count":%d,"total_count":5}`, page, size, size)))(w, r)
	})

	got, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")

	require.ErrorIs(t, err, errUnexpected)
	require.ErrorContains(t, err, "more than 1000 full pages")
	require.Nil(t, got)
	require.Len(t, env.requests(), 1000)
}

func TestTunnelListingChecksThatStillApply(t *testing.T) {
	full := func(page int) []string { return named(fmt.Sprintf("pco-abc_probe_%d_", page), 50) }
	cases := []struct {
		name  string
		page2 func() string
		is    error
		says  string
	}{
		{"a tunnel outside the prefix", func() string {
			return tunnelPage([]string{"pco-abc_probe_x", "pco-abc-node1"}, `{"page":2,"per_page":50,"count":2,"total_count":9}`)
		}, errUnexpected, "outside the requested prefix"},
		{"another page than asked", func() string {
			return tunnelPage(full(1), `{"page":1,"per_page":50,"count":50,"total_count":9}`)
		}, errUnexpected, "asked for page 2, got page 1"},
		{"the total changes", func() string {
			return tunnelPage([]string{"pco-abc_probe_x"}, `{"page":2,"per_page":50,"count":1,"total_count":10}`)
		}, errListingChanged, "total_count differs between pages"},
		{"the page size changes", func() string {
			return tunnelPage([]string{"pco-abc_probe_x"}, `{"page":2,"per_page":20,"count":1,"total_count":9}`)
		}, errListingChanged, "per_page differs between pages"},
		{"more items than the page size", func() string {
			return tunnelPage(named("pco-abc_probe_y", 51), `{"page":2,"per_page":50,"count":51,"total_count":9}`)
		}, errUnexpected, "51 items, more than the 50 of a page"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if page, _ := pageParams(r); page == 2 {
					reply(http.StatusOK, tc.page2())(w, r)
					return
				}
				reply(http.StatusOK, tunnelPage(full(1), `{"page":1,"per_page":50,"count":50,"total_count":9}`))(w, r)
			})

			got, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")

			require.ErrorIs(t, err, tc.is)
			require.ErrorContains(t, err, tc.says)
			require.Nil(t, got)
		})
	}
}

func TestFindTunnelCountsOnlyTheExactNameAcrossPages(t *testing.T) {
	// The name filter matches more than the name: the exact one may sit on
	// any page, among others that are skipped.
	for _, reading := range readings {
		t.Run(reading.name, func(t *testing.T) {
			names := append(named("pco-abc-x", 70), "pco-abc", "PCO-ABC")
			env := setup(t, (&tunnelEndpoint{names: names, filteredTotals: reading.filtered}).serve)

			got, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "id-pco-abc", got.ID)
			require.Len(t, env.requests(), 2)
		})
	}
}

// TestOtherListingsStayStrict pins that the listings of records, zones and
// accounts still hold the server to its totals.
func TestOtherListingsStayStrict(t *testing.T) {
	short := `{"success":true,"errors":[],"result":[%s],"result_info":{"page":1,"per_page":50,"count":1,"total_count":120}}`
	cases := []struct {
		name string
		item string
		call func(c *Client) error
	}{
		{"records", `{"id":"r1","type":"CNAME","name":"a.example.com","content":"x"}`, func(c *Client) error {
			_, err := c.Records(context.Background(), "z1", RecordFilter{})
			return err
		}},
		{"zones", `{"id":"z1","name":"example.com","status":"active","account":{"id":"a1"}}`, func(c *Client) error {
			_, err := c.Zones(context.Background())
			return err
		}},
		{"accounts", `{"id":"a1","name":"First"}`, func(c *Client) error {
			_, err := c.Accounts(context.Background())
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if page, _ := pageParams(r); page == 1 {
					reply(http.StatusOK, fmt.Sprintf(short, tc.item))(w, r)
					return
				}
				reply(http.StatusOK, `{"success":true,"errors":[],"result":[],"result_info":{"count":0,"total_count":120}}`)(w, r)
			})

			err := tc.call(env.c)

			require.ErrorIs(t, err, errListingChanged)
		})
	}
}
