package cfapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// listBody is the answer to a listing request; an empty info leaves
// result_info out.
func listBody(items []string, info string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	body := `{"success":true,"errors":[],"result":[` + strings.Join(quoted, ",") + `]`
	if info != "" {
		body += `,"result_info":` + info
	}
	return body + `}`
}

func pageParams(r *http.Request) (page, size int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	size, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
	return page, size
}

// pagedAnswer answers page n of a listing whose pages hold the given items,
// the way /zones does: with total_pages and total_count.
func pagedAnswer(pages [][]string, w http.ResponseWriter, r *http.Request) {
	page, size := pageParams(r)
	if page < 1 || page > len(pages) {
		reply(http.StatusNotFound, `{"success":false,"errors":[{"code":7003,"message":"no such page"}]}`)(w, r)
		return
	}
	total := 0
	for _, p := range pages {
		total += len(p)
	}
	reply(http.StatusOK, listBody(pages[page-1], fmt.Sprintf(
		`{"page":%d,"per_page":%d,"count":%d,"total_count":%d,"total_pages":%d}`,
		page, size, len(pages[page-1]), total, len(pages))))(w, r)
}

// countedAnswer serves all by page and per_page the way the tunnel listing
// does: result_info has count, page, per_page and total_count, and no
// total_pages. A serverSize above zero is the largest page the server gives,
// whatever is asked.
func countedAnswer(all []string, serverSize int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, size := pageParams(r)
		if serverSize > 0 {
			size = min(size, serverSize)
		}
		lo := min((page-1)*size, len(all))
		hi := min(page*size, len(all))
		reply(http.StatusOK, listBody(all[lo:hi], fmt.Sprintf(
			`{"count":%d,"page":%d,"per_page":%d,"total_count":%d}`, hi-lo, page, size, len(all))))(w, r)
	}
}

func numbered(n int) []string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf("item-%03d", i)
	}
	return items
}

// collect runs list and returns what the callback saw as strings.
func collect(t *testing.T, env *testEnv, path string, query url.Values) ([]string, error) {
	t.Helper()
	var got []string
	err := env.c.list(context.Background(), path, query, func(raw json.RawMessage) error {
		var s string
		require.NoError(t, json.Unmarshal(raw, &s))
		got = append(got, s)
		return nil
	})
	return got, err
}

func requestedURIs(env *testEnv) []string {
	var uris []string
	for _, r := range env.requests() {
		uris = append(uris, r.uri)
	}
	return uris
}

func TestListCollectsPagesInOrder(t *testing.T) {
	pages := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer(pages, w, r) })

	got, err := collect(t, env, "/zones/z1/dns_records", url.Values{"type": {"CNAME"}, "page": {"9"}})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c", "d", "e"}, got)

	for _, r := range env.requests() {
		require.Equal(t, http.MethodGet, r.method)
		require.Equal(t, "Bearer "+testToken, r.auth)
	}
	require.Equal(t, []string{
		"/client/v4/zones/z1/dns_records?page=1&per_page=50&type=CNAME",
		"/client/v4/zones/z1/dns_records?page=2&per_page=50&type=CNAME",
		"/client/v4/zones/z1/dns_records?page=3&per_page=50&type=CNAME",
	}, requestedURIs(env))
}

func TestListDoesNotChangeTheCallersQuery(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}, {"b"}}, w, r) })
	query := url.Values{"type": {"A"}}
	_, err := collect(t, env, "/zones", query)
	require.NoError(t, err)
	require.Equal(t, url.Values{"type": {"A"}}, query)
}

func TestListWithoutQuery(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}}, w, r) })
	got, err := collect(t, env, "/zones", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, got)
	require.Equal(t, []string{"/client/v4/zones?page=1&per_page=50"}, requestedURIs(env))
}

func TestListPageSize(t *testing.T) {
	t.Run("the callers per_page is the page size", func(t *testing.T) {
		env := setup(t, countedAnswer(numbered(5), 0))
		got, err := collect(t, env, "/accounts/a1/cfd_tunnel", url.Values{"per_page": {"2"}, "is_deleted": {"false"}})
		require.NoError(t, err)
		require.Equal(t, numbered(5), got)
		require.Equal(t, []string{
			"/client/v4/accounts/a1/cfd_tunnel?is_deleted=false&page=1&per_page=2",
			"/client/v4/accounts/a1/cfd_tunnel?is_deleted=false&page=2&per_page=2",
			"/client/v4/accounts/a1/cfd_tunnel?is_deleted=false&page=3&per_page=2",
		}, requestedURIs(env))
	})
	for _, bad := range []string{"0", "-1", "many", "1.5", " 2"} {
		t.Run("refuses per_page "+bad, func(t *testing.T) {
			env := setup(t, countedAnswer(numbered(5), 0))
			_, err := collect(t, env, "/zones", url.Values{"per_page": {bad}})
			require.ErrorContains(t, err, "per_page")
			require.Empty(t, env.requests())
		})
	}
}

func TestListFollowsTotalCountWhenThereIsNoTotalPages(t *testing.T) {
	tests := []struct {
		name       string
		items      int
		query      url.Values
		serverSize int
		wantPages  int
	}{
		{"default page size over three pages", 120, nil, 0, 3},
		{"small pages over three pages", 5, url.Values{"per_page": {"2"}}, 0, 3},
		{"exact multiple of the page size", 100, nil, 0, 2},
		{"one item over a page", 51, nil, 0, 2},
		{"exactly one page", 50, nil, 0, 1},
		{"fewer than a page", 3, nil, 0, 1},
		{"nothing", 0, nil, 0, 1},
		{"server gives smaller pages than asked", 60, nil, 25, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, countedAnswer(numbered(tt.items), tt.serverSize))
			got, err := collect(t, env, "/accounts/a1/cfd_tunnel", tt.query)
			require.NoError(t, err)
			require.Len(t, got, tt.items)
			if tt.items > 0 {
				require.Equal(t, numbered(tt.items), got, "in order")
			}
			require.Len(t, env.requests(), tt.wantPages)
		})
	}
}

func TestListWithoutPagingInformation(t *testing.T) {
	tests := []struct {
		name    string
		items   int
		info    string
		query   url.Values
		wantErr bool
	}{
		{"no result_info, a short page", 49, "", nil, false},
		{"no result_info, an empty page", 0, "", nil, false},
		{"no result_info, a full page", 50, "", nil, true},
		{"no result_info, a full page of the callers size", 2, "", url.Values{"per_page": {"2"}}, true},
		{"no result_info, a short page of the callers size", 1, "", url.Values{"per_page": {"2"}}, false},
		{"result_info without counts, a full page", 2, `{"page":1,"per_page":2}`, url.Values{"per_page": {"2"}}, true},
		{"result_info without counts, a short page", 1, `{"page":1,"per_page":2}`, url.Values{"per_page": {"2"}}, false},
		{"empty result_info, a full page", 50, `{}`, nil, true},
		{"the servers page size counts, not the requested one", 30, `{"page":1,"per_page":30}`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, listBody(numbered(tt.items), tt.info)))
			got, err := collect(t, env, "/zones", tt.query)
			if tt.wantErr {
				require.ErrorIs(t, err, errUnexpected)
				require.ErrorContains(t, err, "full page")
				require.Empty(t, got)
				require.Len(t, env.requests(), 1, "no guessing at the next page")
				return
			}
			require.NoError(t, err)
			require.Len(t, got, tt.items)
			require.Len(t, env.requests(), 1)
		})
	}
}

func TestListWithEmptyResult(t *testing.T) {
	for _, info := range []string{
		`{"page":1,"per_page":50,"count":0,"total_count":0,"total_pages":0}`,
		`{"page":1,"per_page":50,"count":0,"total_count":0,"total_pages":1}`,
		`{"page":1,"per_page":50,"count":0,"total_count":0}`,
		`{}`,
		`null`,
	} {
		env := setup(t, reply(http.StatusOK, `{"success":true,"errors":[],"result":[],"result_info":`+info+`}`))
		got, err := collect(t, env, "/zones", nil)
		require.NoError(t, err, info)
		require.Empty(t, got, info)
		require.Len(t, env.requests(), 1, info)
	}
}

func TestListNullResult(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"null with every count zero", `{"success":true,"result":null,"result_info":{"count":0,"page":1,"per_page":50,"total_count":0,"total_pages":0}}`, false},
		{"null with count zero", `{"success":true,"result":null,"result_info":{"count":0}}`, false},
		{"null with total_count zero", `{"success":true,"result":null,"result_info":{"total_count":0}}`, false},
		{"missing with count zero", `{"success":true,"result_info":{"count":0}}`, false},
		{"null without result_info", `{"success":true,"result":null}`, true},
		{"missing without result_info", `{"success":true}`, true},
		{"null with result_info null", `{"success":true,"result":null,"result_info":null}`, true},
		{"null with empty result_info", `{"success":true,"result":null,"result_info":{}}`, true},
		{"null with items reported", `{"success":true,"result":null,"result_info":{"count":3}}`, true},
		{"null with a total reported", `{"success":true,"result":null,"result_info":{"total_count":3,"total_pages":1}}`, true},
		{"null with only pages", `{"success":true,"result":null,"result_info":{"total_pages":0}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, tt.body))
			got, err := collect(t, env, "/zones", nil)
			require.Empty(t, got)
			if tt.wantErr {
				require.ErrorIs(t, err, errUnexpected)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestListResultMustBeAList(t *testing.T) {
	for name, body := range map[string]string{
		"object": okBody(`{"id":"x"}`),
		"string": okBody(`"x"`),
		"number": okBody(`3`),
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, body))
			got, err := collect(t, env, "/zones", nil)
			require.ErrorIs(t, err, errUnexpected)
			require.Empty(t, got)
		})
	}
}

func TestListTotalsMustNotChange(t *testing.T) {
	infos := func(first, second string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			page, _ := pageParams(r)
			if page == 1 {
				reply(http.StatusOK, listBody([]string{"a", "b"}, first))(w, r)
				return
			}
			reply(http.StatusOK, listBody([]string{"c", "d"}, second))(w, r)
		}
	}
	tests := []struct {
		name    string
		first   string
		second  string
		wantErr string
	}{
		{"unchanged", `{"total_count":4,"total_pages":2}`, `{"total_count":4,"total_pages":2}`, ""},
		{"only total_pages, unchanged", `{"total_pages":2}`, `{"total_pages":2}`, ""},
		{"total_count changes", `{"total_count":4,"total_pages":2}`, `{"total_count":5,"total_pages":2}`, "total_count"},
		{"total_count changes, no total_pages", `{"total_count":4,"per_page":2}`, `{"total_count":3,"per_page":2}`, "total_count"},
		{"total_pages changes", `{"total_count":4,"total_pages":2}`, `{"total_count":4,"total_pages":3}`, "total_pages"},
		{"result_info disappears", `{"total_count":4,"total_pages":2}`, ``, "total_count"},
		{"result_info is null", `{"total_count":4,"total_pages":2}`, `null`, "total_count"},
		{"total_count disappears", `{"total_count":4,"total_pages":2}`, `{"total_pages":2}`, "total_count"},
		{"total_count appears", `{"total_pages":2}`, `{"total_count":4,"total_pages":2}`, "total_count"},
		{"total_pages appears", `{"total_count":4,"per_page":2}`, `{"total_count":4,"per_page":2,"total_pages":2}`, "total_pages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, infos(tt.first, tt.second))
			got, err := collect(t, env, "/zones", url.Values{"per_page": {"2"}})
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, []string{"a", "b", "c", "d"}, got)
				return
			}
			require.ErrorIs(t, err, errListingChanged)
			require.ErrorContains(t, err, tt.wantErr)
			require.ErrorContains(t, err, "page 2")
			require.Empty(t, got, "nothing is reported for a listing that changed")
		})
	}
}

func TestListItemsMustAddUpToTotalCount(t *testing.T) {
	pages := func(counts ...int) [][]string {
		var all [][]string
		n := 0
		for _, c := range counts {
			all = append(all, numbered(n + c)[n:])
			n += c
		}
		return all
	}
	answer := func(total int, p [][]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			page, _ := pageParams(r)
			if page > len(p) {
				reply(http.StatusOK, listBody(nil, fmt.Sprintf(`{"count":0,"total_count":%d,"total_pages":%d}`, total, len(p))))(w, r)
				return
			}
			reply(http.StatusOK, listBody(p[page-1], fmt.Sprintf(`{"total_count":%d,"total_pages":%d}`, total, len(p))))(w, r)
		}
	}

	tests := []struct {
		name  string
		total int
		pages [][]string
		ok    bool
	}{
		{"adds up", 5, pages(2, 2, 1), true},
		{"adds up with a short middle page", 3, pages(1, 1, 1), true},
		{"one item missing", 6, pages(2, 2, 1), false},
		{"one item too many", 4, pages(2, 2, 1), false},
		{"count zero but items present", 0, pages(2), false},
		{"count above zero but nothing there", 3, pages(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, answer(tt.total, tt.pages))
			got, err := collect(t, env, "/zones", url.Values{"per_page": {"2"}})
			if tt.ok {
				require.NoError(t, err)
				require.Len(t, got, tt.total)
				return
			}
			require.ErrorIs(t, err, errListingChanged)
			require.Empty(t, got)
		})
	}

	t.Run("a server that reports more than it lists", func(t *testing.T) {
		// 6 reported, 5 there: with no total_pages the pages follow total_count.
		all := numbered(5)
		env := setup(t, func(w http.ResponseWriter, r *http.Request) {
			page, size := pageParams(r)
			lo := min((page-1)*size, len(all))
			hi := min(page*size, len(all))
			reply(http.StatusOK, listBody(all[lo:hi], `{"count":`+strconv.Itoa(hi-lo)+`,"total_count":6}`))(w, r)
		})
		got, err := collect(t, env, "/accounts/a1/cfd_tunnel", url.Values{"per_page": {"2"}})
		require.ErrorIs(t, err, errListingChanged)
		require.ErrorContains(t, err, "read 5 items, the server counts 6")
		require.Empty(t, got)
		require.Len(t, env.requests(), 3)
	})

	t.Run("an empty later page cannot hide missing items", func(t *testing.T) {
		env := setup(t, func(w http.ResponseWriter, r *http.Request) {
			page, _ := pageParams(r)
			if page == 1 {
				reply(http.StatusOK, listBody([]string{"a", "b"}, `{"total_count":5,"total_pages":2}`))(w, r)
				return
			}
			reply(http.StatusOK, `{"success":true,"result":null,"result_info":{"count":0,"total_count":5,"total_pages":2}}`)(w, r)
		})
		got, err := collect(t, env, "/zones", url.Values{"per_page": {"2"}})
		require.ErrorIs(t, err, errListingChanged)
		require.Empty(t, got)
	})
}

func TestListPageMustBeTheOneAskedFor(t *testing.T) {
	answerAs := func(served func(asked int) int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			asked, _ := pageParams(r)
			page := served(asked)
			items := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}[page-1]
			reply(http.StatusOK, listBody(items, fmt.Sprintf(`{"page":%d,"per_page":2,"count":%d,"total_count":5,"total_pages":3}`, page, len(items))))(w, r)
		}
	}
	tests := []struct {
		name    string
		served  func(asked int) int
		pages   int // requests made before the listing is refused
		wantErr string
	}{
		{"every page as asked", func(asked int) int { return asked }, 3, ""},
		{"the first page again", func(int) int { return 1 }, 2, "asked for page 2, got page 1"},
		{"a page skipped", func(asked int) int { return min(asked+1, 3) }, 1, "asked for page 1, got page 2"},
		{"the last page early", func(int) int { return 3 }, 1, "asked for page 1, got page 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, answerAs(tt.served))
			got, err := collect(t, env, "/zones", url.Values{"per_page": {"2"}})
			require.Len(t, env.requests(), tt.pages)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, []string{"a", "b", "c", "d", "e"}, got)
				return
			}
			require.ErrorIs(t, err, errUnexpected)
			require.ErrorContains(t, err, tt.wantErr)
			require.Empty(t, got)
		})
	}
}

func TestListEmptyPageBeforeTheEndFailsAtOnce(t *testing.T) {
	tests := []struct {
		name  string
		info  string // result_info of every page, with %d for the page
		empty int    // the page that comes back empty
	}{
		{"a middle page, with totals", `{"page":%d,"total_count":6,"total_pages":3}`, 2},
		{"a middle page, pages only", `{"page":%d,"total_pages":3}`, 2},
		{"the first page, pages only", `{"page":%d,"total_pages":3}`, 1},
		{"the last page, pages only", `{"page":%d,"total_pages":3}`, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, func(w http.ResponseWriter, r *http.Request) {
				page, _ := pageParams(r)
				items := []string{fmt.Sprintf("p%d-a", page), fmt.Sprintf("p%d-b", page)}
				if page == tt.empty {
					items = nil
				}
				reply(http.StatusOK, listBody(items, fmt.Sprintf(tt.info, page)))(w, r)
			})
			got, err := collect(t, env, "/zones", url.Values{"per_page": {"2"}})
			require.ErrorIs(t, err, errListingChanged)
			require.ErrorContains(t, err, fmt.Sprintf("page %d is empty", tt.empty))
			require.Empty(t, got)
			require.Len(t, env.requests(), tt.empty, "no page after the empty one is asked for")
		})
	}
}

func TestListNoPagesButItems(t *testing.T) {
	for _, pages := range []string{"0", "-1"} {
		t.Run(pages, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, listBody([]string{"a"}, `{"page":1,"total_pages":`+pages+`}`)))
			got, err := collect(t, env, "/zones", nil)
			require.ErrorIs(t, err, errUnexpected)
			require.ErrorContains(t, err, "total_pages is "+pages)
			require.Empty(t, got)
		})
	}
}

func TestListFailsWholeOnPageError(t *testing.T) {
	// The first page is fine and the second one fails. Whatever page 1 held
	// must not be taken for the listing.
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if page, _ := pageParams(r); page == 2 {
			reply(http.StatusInternalServerError, `{"success":false,"errors":[{"code":1000,"message":"boom"}]}`)(w, r)
			return
		}
		pagedAnswer([][]string{{"a", "b"}, {"c"}, {"d"}}, w, r)
	})

	got, err := collect(t, env, "/zones", nil)

	require.Error(t, err)
	var apiErr *Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusInternalServerError, apiErr.Status)
	require.Empty(t, got, "no callback may run for a listing that did not complete")
	require.Len(t, env.requests(), 2, "page 3 is not requested after page 2 failed")
}

func TestListFailsWholeOnAnyBadLaterPage(t *testing.T) {
	page2 := map[string]http.HandlerFunc{
		"server error":      reply(http.StatusBadGateway, `bad gateway`),
		"rate limited":      reply(http.StatusTooManyRequests, `{"success":false,"errors":[]}`),
		"unauthorized":      reply(http.StatusUnauthorized, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`),
		"success false":     reply(http.StatusOK, `{"success":false,"errors":[{"code":1003,"message":"bad"}]}`),
		"not json":          reply(http.StatusOK, `<html></html>`),
		"empty body":        reply(http.StatusOK, ``),
		"no success":        reply(http.StatusOK, `{"result":["x"],"result_info":{"total_pages":3}}`),
		"result is null":    reply(http.StatusOK, okBody(`null`)),
		"result is missing": reply(http.StatusOK, `{"success":true}`),
		"result is object":  reply(http.StatusOK, okBody(`{"id":"x"}`)),
		"result is text":    reply(http.StatusOK, okBody(`"x"`)),
		"dropped":           func(w http.ResponseWriter, _ *http.Request) { dropConnection(w) },
	}
	for name, bad := range page2 {
		t.Run(name, func(t *testing.T) {
			env := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if page, _ := pageParams(r); page == 2 {
					bad(w, r)
					return
				}
				pagedAnswer([][]string{{"a"}, {"b"}, {"c"}}, w, r)
			})
			got, err := collect(t, env, "/zones", nil)
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}

func TestListWrapsTheFailingPage(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if page, _ := pageParams(r); page == 2 {
			reply(http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"nope"}]}`)(w, r)
			return
		}
		pagedAnswer([][]string{{"a"}, {"b"}}, w, r)
	})
	_, err := collect(t, env, "/zones", nil)

	require.ErrorContains(t, err, "page 2")
	require.True(t, IsAuth(err))
}

func TestListFirstPageErrors(t *testing.T) {
	env := setup(t, reply(http.StatusUnauthorized, `{"success":false,"errors":[{"code":10000,"message":"nope"}]}`))
	got, err := collect(t, env, "/zones", nil)
	require.True(t, IsAuth(err))
	require.Empty(t, got)
}

func TestListGuardsAgainstRunawayPaging(t *testing.T) {
	t.Run("more than the limit", func(t *testing.T) {
		env := setup(t, reply(http.StatusOK, `{"success":true,"result":["a"],"result_info":{"total_pages":1001}}`))
		got, err := collect(t, env, "/zones", nil)
		require.ErrorContains(t, err, "1001 pages")
		require.Empty(t, got)
		require.Len(t, env.requests(), 1, "no page after the first is fetched")
	})
	t.Run("more than the limit by total_count", func(t *testing.T) {
		env := setup(t, reply(http.StatusOK, `{"success":true,"result":["a"],"result_info":{"total_count":50001}}`))
		got, err := collect(t, env, "/zones", nil)
		require.ErrorContains(t, err, "1001 pages")
		require.Empty(t, got)
		require.Len(t, env.requests(), 1)
	})
	t.Run("exactly the limit", func(t *testing.T) {
		env := setup(t, reply(http.StatusOK, `{"success":true,"result":["a"],"result_info":{"total_pages":1000}}`), func(o *Options) {
			// 1000 requests would otherwise wait on the default budget.
			o.Limiter = NewLimiter(1_000_000, time.Second, 1000, time.Now)
		})
		got, err := collect(t, env, "/zones", nil)
		require.NoError(t, err)
		require.Len(t, got, 1000)
	})
}

func TestListStopsWhenCallbackFails(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a", "b"}, {"c"}}, w, r) })
	errStop := errors.New("stop here")

	var seen []string
	err := env.c.list(context.Background(), "/zones", nil, func(raw json.RawMessage) error {
		seen = append(seen, string(raw))
		if len(seen) == 2 {
			return errStop
		}
		return nil
	})
	require.ErrorIs(t, err, errStop)
	require.Equal(t, []string{`"a"`, `"b"`}, seen)
}

func TestListStopsWhenContextEnds(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}, {"b"}}, w, r) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	err := env.c.list(ctx, "/zones", nil, func(json.RawMessage) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)

	cancel()
	err = env.c.list(ctx, "/zones", nil, func(json.RawMessage) error {
		calls++
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, calls)
}

func TestListEachRequestWaitsForTheLimiter(t *testing.T) {
	clock := newFakeClock()
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}, {"b"}, {"c"}}, w, r) }, func(o *Options) {
		// One token every 10 s, room for one.
		o.Limiter = newLimiter(6, time.Minute, 1, clock.now, clock.sleep)
	})
	_, err := collect(t, env, "/zones", nil)
	require.NoError(t, err)
	require.Len(t, env.requests(), 3)
	require.Equal(t, []time.Duration{10 * time.Second, 10 * time.Second}, clock.takeSleeps())
}
