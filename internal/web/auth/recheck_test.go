package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recheck asks about the session of b as a stream of it would.
func (b *browser) recheck() (Role, error) {
	return b.h.auth.Recheck(context.Background(), b.request(http.MethodGet, StreamPath, ""), b.session)
}

func TestRecheckFollowsTheRoleOfTheTicket(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	role, err := b.recheck()
	require.NoError(t, err)
	require.Equal(t, RoleAdmin, role)

	// Sys.Modify taken away: the cached answer holds for 30 s, then not.
	h.fake.SetPrivileges("alice@pve", "/", "Sys.Audit")
	h.clock.Add(10 * time.Second)
	role, _ = b.recheck()
	require.Equal(t, RoleAdmin, role)
	h.clock.Add(21 * time.Second)
	role, err = b.recheck()
	require.NoError(t, err)
	require.Equal(t, RoleReader, role)

	// Nothing on / at all.
	h.fake.SetPrivileges("alice@pve", "/")
	h.clock.Add(31 * time.Second)
	role, err = b.recheck()
	require.NoError(t, err)
	require.Equal(t, RoleNone, role)
}

func TestRecheckIsNoActivityAndEndsNothing(t *testing.T) {
	h := newHarness(t)
	b := h.browser(bobTicket)
	before := b.signIn()
	h.clock.Add(29 * time.Minute)
	for range 3 {
		role, err := b.recheck()
		require.NoError(t, err)
		require.Equal(t, RoleReader, role)
	}
	s, ok := h.auth.sessions.get(b.session)
	require.True(t, ok)
	require.Equal(t, before.IdleExpiresAt, s.idleExpiresAt().UTC(), "a recheck is no activity")

	// The ticket refused: the answer is no, and the session is the next
	// request's to end.
	h.fake.RemoveTicket(bobTicket)
	h.clock.Add(31 * time.Second)
	role, err := b.recheck()
	require.NoError(t, err)
	require.Equal(t, RoleNone, role)
	_, ok = h.auth.sessions.get(b.session)
	require.True(t, ok)
}

func TestRecheckSaysNoForASessionThatIsOver(t *testing.T) {
	cases := []struct {
		name string
		end  func(h *harness, b *browser)
	}{
		{"signed out", func(_ *harness, b *browser) {
			require.Equal(t, http.StatusNoContent, b.do(http.MethodDelete, "/api/session", "").Code)
		}},
		{"idle", func(h *harness, _ *browser) { h.clock.Add(30 * time.Minute) }},
		{"at the end of its life", func(h *harness, b *browser) {
			// In use all along, so idle never; 12 h after the sign-in.
			for range 25 {
				h.clock.Add(29 * time.Minute)
				if h.clock.Now().Before(t0.Add(12 * time.Hour)) {
					_, err := h.auth.sessions.touch(b.session, h.clock.Now())
					require.NoError(h.t, err)
				}
			}
			_, ok := h.auth.sessions.get(b.session)
			require.True(h.t, ok)
		}},
		{"replaced by a new sign-in", func(_ *harness, b *browser) {
			old := b.session
			b.signIn()
			b.session = old
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			b := h.browser(aliceTicket)
			b.signIn()
			c.end(h, b)
			role, err := b.recheck()
			require.NoError(t, err)
			require.Equal(t, RoleNone, role)
		})
	}
	h := newHarness(t)
	role, err := h.browser(aliceTicket).recheck()
	require.NoError(t, err)
	require.Equal(t, RoleNone, role, "no session at all")
}

func TestRecheckOfATokenSession(t *testing.T) {
	h := newHarness(t)
	b := h.browser("")
	require.Equal(t, http.StatusOK, b.signInToken(aliceToken).Code)
	calls := h.fake.Calls("access/permissions")
	role, err := b.recheck()
	require.NoError(t, err)
	require.Equal(t, RoleAdmin, role)
	require.Equal(t, calls, h.fake.Calls("access/permissions"), "a token checked within the minute is not asked about")

	h.fake.RemoveToken("alice@pve!pco")
	h.clock.Add(time.Minute)
	role, err = b.recheck()
	require.NoError(t, err)
	require.Equal(t, RoleNone, role)
}

func TestRecheckWithProxmoxDownIsAnError(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	h.clock.Add(time.Minute)
	h.pve.setDown(true)
	_, err := b.recheck()
	require.ErrorIs(t, err, ErrUnreachable)
}

func TestOnEndTellsOfEverySessionThatEnds(t *testing.T) {
	h := newHarness(t)
	var (
		mu    sync.Mutex
		ended []string
	)
	h.auth.OnEnd(func(id string) {
		mu.Lock()
		defer mu.Unlock()
		ended = append(ended, id)
	})
	endedNow := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := ended
		ended = nil
		return out
	}

	b := h.browser(aliceTicket)
	b.signIn()
	first := b.session
	b.signIn()
	require.Equal(t, []string{first}, endedNow(), "replaced by a new sign-in")

	second := b.session
	require.Equal(t, http.StatusNoContent, b.do(http.MethodDelete, "/api/session", "").Code)
	require.Equal(t, []string{second}, endedNow(), "signed out")

	b.signIn()
	third := b.session
	h.clock.Add(31 * time.Minute)
	b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, []string{third}, endedNow(), "expired")

	b.signIn()
	fourth := b.session
	h.fake.RemoveTicket(aliceTicket)
	h.clock.Add(31 * time.Second)
	b.do(http.MethodGet, "/api/v1/state", "")
	require.Equal(t, []string{fourth}, endedNow(), "refused by Proxmox VE")
}
