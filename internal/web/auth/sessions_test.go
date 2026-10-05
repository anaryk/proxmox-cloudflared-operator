package auth

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// clock is a fake time that moves only when a test says so.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: t0} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testSession(id, user, method string, at time.Time) *Session {
	return &Session{ID: id, Principal: Principal{User: user, Method: method, Role: RoleReader}, created: at, lastSeen: at}
}

func TestTheSixthSessionOfAUserEvictsItsOldest(t *testing.T) {
	st := newStore()
	at := t0
	st.add(testSession("bob-1", "bob@pve", MethodTicket, at), at)
	for i := 1; i <= perUser; i++ {
		at = at.Add(time.Second)
		evicted := st.add(testSession(fmt.Sprintf("alice-%d", i), "alice@pve", MethodTicket, at), at)
		require.Empty(t, evicted)
	}
	at = at.Add(time.Second)
	evicted := st.add(testSession("alice-6", "alice@pve", MethodToken, at), at)
	require.Len(t, evicted, 1)
	require.Equal(t, "alice-1", evicted[0].ID)

	for id, want := range map[string]lookupState{"alice-1": missing, "alice-2": found, "alice-6": found, "bob-1": found} {
		_, state := st.lookup(id, at)
		require.Equal(t, want, state, id)
	}
}

func TestThe501stSessionEvictsTheOldest(t *testing.T) {
	st := newStore()
	at := t0
	for i := range maxSessions {
		at = at.Add(time.Millisecond)
		st.add(testSession(fmt.Sprintf("s-%d", i), fmt.Sprintf("u%d@pve", i), MethodTicket, at), at)
	}
	// The oldest is seen again, which makes it not the oldest: eviction goes by
	// the start of a session, not by its last request.
	_, err := st.touch("s-0", at)
	require.NoError(t, err)
	at = at.Add(time.Millisecond)
	evicted := st.add(testSession("s-500", "u500@pve", MethodTicket, at), at)
	require.Len(t, evicted, 1)
	require.Equal(t, "s-0", evicted[0].ID)
	require.Len(t, st.byID, maxSessions)
	_, state := st.lookup("s-500", at)
	require.Equal(t, found, state)
}

func TestExpiredSessionsMakeRoomFirst(t *testing.T) {
	st := newStore()
	for i := range maxSessions {
		st.add(testSession(fmt.Sprintf("s-%d", i), fmt.Sprintf("u%d@pve", i), MethodTicket, t0), t0)
	}
	at := t0.Add(idleTimeout)
	evicted := st.add(testSession("new", "new@pve", MethodTicket, at), at)
	require.Empty(t, evicted, "expired sessions are not evicted, they are over")
	require.Len(t, st.byID, 1)
}

func TestIdleAndAbsoluteExpiry(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		lifetime time.Duration
	}{
		{"a ticket session", MethodTicket, 12 * time.Hour},
		{"a token session", MethodToken, 8 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore()
			st.add(testSession("idle", "a@pve", c.method, t0), t0)
			_, state := st.lookup("idle", t0.Add(idleTimeout-time.Second))
			require.Equal(t, found, state)
			_, state = st.lookup("idle", t0.Add(idleTimeout))
			require.Equal(t, expired, state)
			_, state = st.lookup("idle", t0.Add(idleTimeout))
			require.Equal(t, missing, state, "an expired session is gone")

			st.add(testSession("busy", "a@pve", c.method, t0), t0)
			at := t0
			for at.Before(t0.Add(c.lifetime - 20*time.Minute)) {
				at = at.Add(20 * time.Minute)
				_, err := st.touch("busy", at)
				require.NoError(t, err, at.Sub(t0))
			}
			s, state := st.lookup("busy", t0.Add(c.lifetime-time.Second))
			require.Equal(t, found, state)
			require.Equal(t, t0.Add(c.lifetime), s.expiresAt())
			_, state = st.lookup("busy", t0.Add(c.lifetime))
			require.Equal(t, expired, state, "activity does not move the absolute limit")
		})
	}
}

func TestTouchAndUpdate(t *testing.T) {
	st := newStore()
	st.add(testSession("s", "a@pve", MethodTicket, t0), t0)
	s, err := st.touch("s", t0.Add(10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, t0.Add(40*time.Minute), s.idleExpiresAt())

	s, ok := st.update("s", func(s *Session) { s.Principal.Role = RoleAdmin })
	require.True(t, ok)
	require.Equal(t, RoleAdmin, s.Principal.Role)

	gone, ok := st.remove("s")
	require.True(t, ok)
	require.Equal(t, "s", gone.ID)
	_, ok = st.update("s", func(*Session) { t.Fatal("updated a removed session") })
	require.False(t, ok)
	_, err = st.touch("s", t0)
	require.ErrorIs(t, err, errNoSession)
	_, ok = st.remove("s")
	require.False(t, ok)
}

func TestSecrets(t *testing.T) {
	s, err := secret(bytes.NewReader(make([]byte, 64)))
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("A", 43), s, "256 bits, unpadded base64url")

	s, err = secret(rand.Reader)
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`), s)

	_, err = secret(bytes.NewReader(make([]byte, 31)))
	require.Error(t, err, "a short read is no secret")
	_, err = secret(iotest.ErrReader(errors.New("no entropy")))
	require.ErrorContains(t, err, "no entropy")
}
