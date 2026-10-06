package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestTheETagOfAFilteredStateChangesWithTheVisibleSet(t *testing.T) {
	s := newTestServer(t)
	st := populated(t)
	s.daemon.serveState(st)
	s.pve.sees(reader, 101)
	s.pve.sees(reader2, 101, 201)
	bob, carol := s.signIn(reader), s.signIn(reader2)

	etag := func(b *browser) string {
		rec := b.do(http.MethodGet, "/api/v1/state", "")
		require.Equal(t, http.StatusOK, rec.Code)
		return rec.Header().Get("ETag")
	}
	tagBob, tagCarol := etag(bob), etag(carol)
	require.Regexp(t, `^"[0-9a-f]{16}"$`, tagBob)
	require.NotEqual(t, tagBob, tagCarol)
	require.NotEqual(t, `"`+st.Digest+`"`, tagBob, "a reader's tag is not the daemon's")
	require.Equal(t, tagBob, etag(bob))

	// A matching If-None-Match is a 304 without the state.
	r := bob.request(http.MethodGet, "/api/v1/state", "")
	r.Header.Set("If-None-Match", tagBob)
	rec := bob.send(r)
	require.Equal(t, http.StatusNotModified, rec.Code)
	require.Empty(t, rec.Body.String())
	r = carol.request(http.MethodGet, "/api/v1/state", "")
	r.Header.Set("If-None-Match", tagBob)
	require.Equal(t, http.StatusOK, carol.send(r).Code, "bob's tag is not carol's")

	// Another digest gives another tag for the same set.
	st.Digest = "77a1000000000000"
	s.daemon.serveState(st)
	require.NotEqual(t, tagBob, etag(bob))

	a := s.signIn(admin)
	r = a.request(http.MethodGet, "/api/v1/state", "")
	r.Header.Set("If-None-Match", `"77a1000000000000"`)
	require.Equal(t, http.StatusNotModified, a.send(r).Code)
}

func TestTheStateCacheIsSharedByTheSameSet(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(largeState())
	s.pve.sees(reader, 1000, 1002)
	s.pve.sees(reader2, 1000, 1002)
	bob, carol := s.signIn(reader), s.signIn(reader2)

	require.Equal(t, http.StatusOK, bob.do(http.MethodGet, "/api/v1/state", "").Code)
	require.Equal(t, 1, s.gw.states.filtered)
	rec := carol.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, s.gw.states.filtered, "a second reader with the same set takes the cached answer")

	s.pve.sees(reader2, 1000)
	carol2 := s.signIn(reader2)
	require.Equal(t, http.StatusOK, carol2.do(http.MethodGet, "/api/v1/state", "").Code)
	require.Equal(t, 2, s.gw.states.filtered, "another set is filtered anew")

	// The daemon sent its megabyte once; the other calls were 304s.
	calls := 0
	for _, c := range s.daemon.requests() {
		if c.route() == "GET /v1/state" {
			calls++
			if calls > 1 {
				require.Equal(t, `"1a2b3c4d5e6f7081"`, c.Header.Get("If-None-Match"))
			}
		}
	}
	require.Equal(t, 3, calls)
}

func TestTheStateCacheKeepsThirtyTwo(t *testing.T) {
	c := newStateCache()
	for i := range 40 {
		key := fmt.Sprintf("r:%d", i)
		a, err := c.get(key, func() (*encoded, error) { return &encoded{body: []byte(key)}, nil })
		require.NoError(t, err)
		require.Equal(t, key, string(a.body))
	}
	require.Equal(t, 32, c.order.Len())
	made := 0
	for i := range 40 {
		_, err := c.get(fmt.Sprintf("r:%d", i), func() (*encoded, error) { made++; return &encoded{}, nil })
		require.NoError(t, err)
	}
	require.Equal(t, 40, made, "the eight oldest were evicted, and each lookup after pushed out another")

	// The one used last is the one kept.
	c = newStateCache()
	for i := range 32 {
		_, _ = c.get(fmt.Sprintf("k%d", i), func() (*encoded, error) { return &encoded{}, nil })
	}
	_, _ = c.get("k0", func() (*encoded, error) { t.Fatal("k0 is cached"); return nil, nil })
	_, _ = c.get("k32", func() (*encoded, error) { return &encoded{}, nil })
	_, _ = c.get("k0", func() (*encoded, error) { t.Fatal("k0 was used last and stays"); return nil, nil })
	again := false
	_, _ = c.get("k1", func() (*encoded, error) { again = true; return &encoded{}, nil })
	require.True(t, again, "k1 was used longest ago and went")
}

func TestTheStateIsGzippedOnce(t *testing.T) {
	s := newTestServer(t)
	st := largeState()
	s.daemon.serveState(st)
	var some []int
	for vmid := 1000; vmid < 1200; vmid += 2 {
		some = append(some, vmid)
	}
	s.pve.sees(reader, some...)
	for _, user := range []string{admin, reader} {
		b := s.signIn(user)
		r := b.request(http.MethodGet, "/api/v1/state", "")
		r.Header.Set("Accept-Encoding", "gzip")
		rec := b.send(r)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
		plain := gunzip(t, rec.Body.Bytes())
		require.True(t, strings.HasPrefix(string(plain), `{"node":"pve1","digest":"1a2b3c4d5e6f7081"`), string(plain[:80]))

		// Without gzip the same state comes as it is.
		rec = b.do(http.MethodGet, "/api/v1/state", "")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Empty(t, rec.Header().Get("Content-Encoding"))
		require.Equal(t, string(plain), rec.Body.String())
	}
}

func TestAStateWithoutADigestIsNotCached(t *testing.T) {
	s := newTestServer(t)
	st := engine.State{Mode: engine.ModeObserve, Routes: []engine.RouteView{}}
	s.daemon.serveState(st)
	b := s.signIn(reader)
	rec := b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get("ETag"))
	rec = b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, s.daemon.last(t, "GET /v1/state").Header.Get("If-None-Match"))
}
