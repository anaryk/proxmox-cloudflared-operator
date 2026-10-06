package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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

func TestOnEndTellsOfASessionThatIsEvicted(t *testing.T) {
	h := newHarness(t)
	var ended []string
	h.auth.OnEnd(func(id string) { ended = append(ended, id) })
	var oldest string
	for i := range perUser + 1 {
		b := h.browser(aliceTicket)
		b.signIn()
		if i == 0 {
			oldest = b.session
		}
		if i < perUser {
			require.Empty(t, ended, "five sessions of a user are kept")
		}
		h.clock.Add(time.Second)
	}
	require.Equal(t, []string{oldest}, ended, "the sixth sign-in of a user ends its oldest")
}

func TestLiveSaysWhetherTheSessionIsOverWithoutBeingActivity(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	before := b.signIn()
	require.True(t, h.auth.Live(b.session))
	h.clock.Add(29 * time.Minute)
	require.True(t, h.auth.Live(b.session))
	s, ok := h.auth.sessions.get(b.session)
	require.True(t, ok)
	require.Equal(t, before.IdleExpiresAt, s.idleExpiresAt().UTC(), "asking is no activity")
	h.clock.Add(time.Minute)
	require.False(t, h.auth.Live(b.session), "idle for half an hour")

	b = h.browser(aliceTicket)
	b.signIn()
	require.Equal(t, http.StatusNoContent, b.do(http.MethodDelete, "/api/session", "").Code)
	require.False(t, h.auth.Live(b.session), "signed out")
	require.False(t, h.auth.Live("no such session"))
}

// The set a stream's session sees is the one the store holds, so that a read
// by any request of the session counts for the stream too, and a read by the
// stream for the requests.
func TestVisibleOfIsTheSetTheStoreHolds(t *testing.T) {
	h := newHarness(t)
	b := h.browser(bobTicket)
	b.signIn()
	stream := func() (Visible, string, error) {
		return h.auth.VisibleOf(context.Background(), b.request(http.MethodGet, StreamPath, ""), b.session)
	}
	b.do(http.MethodGet, "/api/v1/visible", "")
	require.Equal(t, 1, h.fake.Calls("cluster/resources"))
	h.clock.Add(time.Minute)
	rec := b.do(http.MethodGet, "/api/v1/visible", "")
	require.Equal(t, 2, h.fake.Calls("cluster/resources"))
	var got struct {
		Hash string `json:"hash"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	h.clock.Add(30 * time.Second)
	visible, hash, err := stream()
	require.NoError(t, err)
	require.Equal(t, 2, h.fake.Calls("cluster/resources"), "what a request read 30 s ago is not read again")
	require.Equal(t, got.Hash, hash)
	require.True(t, visible(model.GuestRef{Kind: model.KindQEMU, VMID: 102}))
	require.False(t, visible(model.GuestRef{Kind: model.KindQEMU, VMID: 101}))

	h.clock.Add(31 * time.Second)
	_, _, err = stream()
	require.NoError(t, err)
	require.Equal(t, 3, h.fake.Calls("cluster/resources"), "a minute old, it is read again")
	b.do(http.MethodGet, "/api/v1/visible", "")
	require.Equal(t, 3, h.fake.Calls("cluster/resources"), "and the requests have it")

	h.pve.setDown(true)
	h.clock.Add(time.Minute)
	_, _, err = stream()
	require.ErrorIs(t, err, ErrUnreachable)
}

func TestVisibleOfAnAdminAndOfASessionThatIsOver(t *testing.T) {
	h := newHarness(t)
	b := h.browser(aliceTicket)
	b.signIn()
	visible, hash, err := h.auth.VisibleOf(context.Background(), b.request(http.MethodGet, StreamPath, ""), b.session)
	require.NoError(t, err)
	require.Empty(t, hash)
	require.True(t, visible(model.GuestRef{Kind: model.KindLXC, VMID: 999}))
	require.Zero(t, h.fake.Calls("cluster/resources"))

	require.Equal(t, http.StatusNoContent, b.do(http.MethodDelete, "/api/session", "").Code)
	_, _, err = h.auth.VisibleOf(context.Background(), b.request(http.MethodGet, StreamPath, ""), b.session)
	require.Error(t, err)
}
