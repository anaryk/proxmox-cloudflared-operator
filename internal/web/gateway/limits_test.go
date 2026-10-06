package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

func requireRateLimited(t *testing.T, rec *httptest.ResponseRecorder, after int) {
	t.Helper()
	requireError(t, rec, http.StatusTooManyRequests, wire.CodeRateLimited)
	var body wire.Error
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, after, body.RetryAfter)
	require.Equal(t, fmt.Sprint(after), rec.Header().Get("Retry-After"))
}

func TestDiagnosesAreLimitedPerUser(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(populated(t))
	alice := s.signIn(admin)
	body := `{"hostname":"www.example.com"}`
	for range 6 {
		require.Equal(t, http.StatusOK, alice.do(http.MethodPost, "/api/v1/diagnose", body).Code)
		s.clock.advance(5 * time.Second)
	}
	requireRateLimited(t, alice.do(http.MethodPost, "/api/v1/diagnose", body), 30)
	require.Equal(t, 6, s.daemon.count("GET /v1/diagnose"))

	// Another user has a minute of its own; another session of the same
	// user has not.
	s.pve.sees(reader, 101)
	require.Equal(t, http.StatusOK, s.signIn(reader).do(http.MethodPost, "/api/v1/diagnose", body).Code)
	requireRateLimited(t, s.signIn(admin).do(http.MethodPost, "/api/v1/diagnose", body), 30)

	s.clock.advance(30 * time.Second)
	require.Equal(t, http.StatusOK, alice.do(http.MethodPost, "/api/v1/diagnose", body).Code)
}

func TestOneDiagnosisAtATime(t *testing.T) {
	s := newTestServer(t)
	alice := s.signIn(admin)
	started, release := make(chan struct{}), make(chan struct{})
	s.daemon.on("GET /v1/diagnose", func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte("[]"))
	})
	done := make(chan int)
	go func() { done <- alice.do(http.MethodPost, "/api/v1/diagnose", `{"hostname":"www.example.com"}`).Code }()
	<-started
	s.clock.advance(10 * time.Second)
	requireRateLimited(t, alice.do(http.MethodPost, "/api/v1/diagnose", `{"hostname":"api.example.com"}`), 20)
	close(release)
	require.Equal(t, http.StatusOK, <-done)

	go func() { <-started }()
	require.Equal(t, http.StatusOK, alice.do(http.MethodPost, "/api/v1/diagnose", `{"hostname":"api.example.com"}`).Code)
}

func TestTheDoctorIsLimitedPerUser(t *testing.T) {
	s := newTestServer(t)
	s.daemon.answer("GET /v1/doctor", http.StatusOK, doctorAnswer)
	alice := s.signIn(admin)
	require.Equal(t, http.StatusOK, alice.do(http.MethodPost, "/api/v1/doctor", "{}").Code)
	s.clock.advance(20 * time.Second)
	require.Equal(t, http.StatusOK, alice.do(http.MethodPost, "/api/v1/doctor", "{}").Code)
	requireRateLimited(t, alice.do(http.MethodPost, "/api/v1/doctor", "{}"), 40)
	require.Equal(t, 2, s.daemon.count("GET /v1/doctor"))
	require.Equal(t, http.StatusOK, s.signIn(reader).do(http.MethodPost, "/api/v1/doctor", "{}").Code)
	s.clock.advance(40 * time.Second)
	require.Equal(t, http.StatusOK, alice.do(http.MethodPost, "/api/v1/doctor", "{}").Code)
}

// A refused call does not count, and a diagnosis the gateway refused before
// it went up takes no place.
func TestRefusalsDoNotCount(t *testing.T) {
	l := newUserLimit("diagnoses", 6, 1, time.Minute)
	now := t0
	for range 6 {
		done, _, ok := l.start("alice@pve", now, 30*time.Second)
		require.True(t, ok)
		done()
	}
	for range 10 {
		_, wait, ok := l.start("alice@pve", now, 30*time.Second)
		require.False(t, ok)
		require.Equal(t, time.Minute, wait)
	}
	done, _, ok := l.start("alice@pve", now.Add(time.Minute), 30*time.Second)
	require.True(t, ok)
	done()
	done()
	require.Len(t, l.users, 1)
	_, _, ok = l.start("bob@pve", now.Add(3*time.Minute), 30*time.Second)
	require.True(t, ok)
	require.Len(t, l.users, 1, "a user whose minute is over is forgotten")
}

func TestStreamSlots(t *testing.T) {
	sl := newStreamSlots()
	for i := range 66 {
		for range 3 {
			require.True(t, sl.take(fmt.Sprint(i)))
		}
		require.False(t, sl.take(fmt.Sprint(i)), "the 4th of a session")
	}
	require.True(t, sl.take("a"))
	require.True(t, sl.take("b"))
	require.Equal(t, 200, sl.open())
	require.False(t, sl.take("c"), "the 201st in all")
	sl.give("a")
	require.True(t, sl.take("c"))
	sl.give("c")
	sl.give("b")
	require.Equal(t, 198, sl.open())
	require.NotContains(t, sl.bySession, "b")
}
