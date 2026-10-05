package auth

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func keysOf(l *limiter) []string {
	return slices.Sorted(maps.Keys(l.byKey))
}

func TestAFullLimiterLetsTheOldestGo(t *testing.T) {
	l := newLimiter(maxLimitedPeers)
	at := t0
	for range signInsPerMinute {
		ok, _ := l.allow("192.0.2.1", at)
		require.True(t, ok)
	}
	ok, _ := l.allow("192.0.2.1", at)
	require.False(t, ok)

	for i := range maxLimitedPeers {
		at = at.Add(time.Millisecond)
		ok, _ := l.allow(fmt.Sprintf("198.51.%d.%d", i/256, i%256), at)
		require.True(t, ok, "address %d", i+2)
	}
	require.Len(t, l.byKey, maxLimitedPeers)
	require.NotContains(t, l.byKey, "192.0.2.1", "the oldest is gone")
	ok, _ = l.allow("192.0.2.1", at)
	require.True(t, ok, "which only loosens its limit")
}

func TestTheLimiterGoesByTheLastAttempt(t *testing.T) {
	l := newLimiter(3)
	for i, key := range []string{"a", "b", "c", "a", "d"} {
		ok, _ := l.allow(key, t0.Add(time.Duration(i)*time.Second))
		require.True(t, ok, key)
	}
	require.Equal(t, []string{"a", "c", "d"}, keysOf(l), "b was tried longest ago, though a came first")

	ok, _ := l.allow("e", t0.Add(63*time.Second))
	require.True(t, ok)
	require.Equal(t, []string{"d", "e"}, keysOf(l), "a key a window after its last attempt is dropped")
}

func TestARefusedAttemptDoesNotCount(t *testing.T) {
	l := newLimiter(maxLimitedPeers)
	for range signInsPerMinute {
		ok, _ := l.allow("192.0.2.1", t0)
		require.True(t, ok)
	}
	for s := 1; s < 60; s++ {
		for range 3 {
			ok, wait := l.allow("192.0.2.1", t0.Add(time.Duration(s)*time.Second))
			require.False(t, ok, s)
			require.Equal(t, time.Duration(60-s)*time.Second, wait, "the wait runs from the first attempt allowed")
		}
	}
	ok, _ := l.allow("192.0.2.1", t0.Add(time.Minute))
	require.True(t, ok, "a minute after the attempts it allowed, whatever it refused since")
}

func TestBucket(t *testing.T) {
	cases := map[string]string{
		"192.0.2.7":                  "192.0.2.7",
		"::ffff:192.0.2.7":           "192.0.2.7",
		"2001:db8:1:2::1":            "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff:0:1": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":            "2001:db8:1:3::/64",
		"fe80::1%eth0":               "fe80::/64",
		"not an address":             "not an address",
	}
	for addr, want := range cases {
		require.Equal(t, want, bucket(addr), addr)
	}
}

func TestAnIPv6NetworkSharesItsLimit(t *testing.T) {
	h := newHarness(t)
	attempt := func(peer string) int {
		b := h.browser("")
		b.peer = peer
		return b.signInToken("alice@pve!guess=" + tokenSecret).Code
	}
	for i := range signInsPerMinute {
		require.Equal(t, http.StatusUnauthorized, attempt(fmt.Sprintf("[2001:db8:1:2::%x]:443", i+1)), i)
	}
	require.Equal(t, http.StatusTooManyRequests, attempt("[2001:db8:1:2:ffff:ffff:ffff:ffff]:443"), "another address of the same /64")
	require.Equal(t, http.StatusUnauthorized, attempt("[2001:db8:1:3::1]:443"), "the next /64 has a limit of its own")
	require.Contains(t, h.log.String(), `"client":"2001:db8:1:2::1"`, "the log has the address itself")
}
