package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// heldPVE answers every credential alike; while it is held, every call waits
// until it is let go.
type heldPVE struct {
	privs map[string]bool

	mu    sync.Mutex
	calls int
	held  chan struct{}
}

func (p *heldPVE) Privileges(context.Context, Credential, string) (map[string]bool, error) {
	p.mu.Lock()
	p.calls++
	held := p.held
	p.mu.Unlock()
	if held != nil {
		<-held
	}
	return p.privs, nil
}

func (p *heldPVE) VisibleVMIDs(context.Context, Credential) ([]int, error) { return nil, nil }

// hold makes the calls wait until the channel it returns is closed.
func (p *heldPVE) hold() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held = make(chan struct{})
	return p.held
}

func (p *heldPVE) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestRequestsAtTheEndOfACheckShareOneCall(t *testing.T) {
	cases := []struct {
		name, path, body string
		age              time.Duration
	}{
		{"a ticket", "/api/session/ticket", "{}", ticketCheckAge},
		{"a token", "/api/session/token", `{"token":"` + aliceToken + `"}`, tokenCheckAge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pve := &heldPVE{privs: map[string]bool{"Sys.Audit": true, "Sys.Modify": true}}
				clock := newClock()
				a := New(pve, Config{Now: clock.Now, Hosts: func() []string { return []string{testHost} }, Log: zerolog.Nop()})
				gin.SetMode(gin.ReleaseMode)
				r := gin.New()
				a.Mount(r)
				r.GET("/api/v1/state", a.Require(RoleReader), func(c *gin.Context) { c.Status(http.StatusNoContent) })
				ticket := &http.Cookie{Name: ticketCookieName, Value: aliceTicket}

				req := httptest.NewRequest(http.MethodPost, "https://"+testHost+c.path, strings.NewReader(c.body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Origin", testOrigin)
				req.AddCookie(ticket)
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, req)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				session := rec.Result().Cookies()[0]
				require.Equal(t, 1, pve.count())

				clock.Add(c.age)
				held := pve.hold()
				codes := make([]int, 50)
				var wg sync.WaitGroup
				for i := range codes {
					wg.Go(func() {
						req := httptest.NewRequest(http.MethodGet, "https://"+testHost+"/api/v1/state", nil)
						req.AddCookie(ticket)
						req.AddCookie(session)
						rec := httptest.NewRecorder()
						r.ServeHTTP(rec, req)
						codes[i] = rec.Code
					})
				}
				synctest.Wait()
				during := pve.count()
				close(held)
				wg.Wait()

				require.Equal(t, 2, during, "the sign-in, and one check for all fifty")
				require.Equal(t, 2, pve.count())
				for i, code := range codes {
					require.Equal(t, http.StatusNoContent, code, i)
				}
			})
		})
	}
}

func TestAFlight(t *testing.T) {
	t.Run("the call runs without the cancellation of its caller", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var f flight[string, int]
			release := make(chan struct{})
			ctx, cancel := context.WithCancel(context.Background())
			var first, second int
			var firstErr, secondErr error
			var wg sync.WaitGroup
			wg.Go(func() {
				first, firstErr = f.do(ctx, "k", func(ctx context.Context) (int, error) {
					<-release
					return 1, ctx.Err()
				})
			})
			synctest.Wait()
			wg.Go(func() {
				second, secondErr = f.do(context.Background(), "k", func(context.Context) (int, error) { return 2, nil })
			})
			synctest.Wait()
			cancel()
			close(release)
			wg.Wait()
			require.NoError(t, firstErr, "the call goes on without the caller's cancellation")
			require.Equal(t, 1, first)
			require.NoError(t, secondErr)
			require.Equal(t, 1, second, "the second caller has the first call's answer")
		})
	})
	t.Run("a waiter whose context ends stops waiting", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var f flight[string, int]
			release := make(chan struct{})
			var wg sync.WaitGroup
			wg.Go(func() {
				_, _ = f.do(context.Background(), "k", func(context.Context) (int, error) { <-release; return 1, nil })
			})
			synctest.Wait()
			ctx, cancel := context.WithCancel(context.Background())
			errs := make(chan error, 1)
			go func() {
				_, err := f.do(ctx, "k", func(context.Context) (int, error) { return 2, nil })
				errs <- err
			}()
			synctest.Wait()
			cancel()
			require.ErrorIs(t, <-errs, context.Canceled, "while the call still runs")
			close(release)
			wg.Wait()
		})
	})
	t.Run("a call that panics fails its waiters", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var f flight[string, int]
			release := make(chan struct{})
			var wg sync.WaitGroup
			var recovered any
			wg.Go(func() {
				defer func() { recovered = recover() }()
				_, _ = f.do(context.Background(), "k", func(context.Context) (int, error) { <-release; panic("broken") })
			})
			synctest.Wait()
			var err error
			wg.Go(func() { _, err = f.do(context.Background(), "k", func(context.Context) (int, error) { return 2, nil }) })
			synctest.Wait()
			close(release)
			wg.Wait()
			require.Equal(t, "broken", recovered, "the panic is the caller's")
			require.Error(t, err)
			require.False(t, errors.Is(err, context.Canceled))

			got, err := f.do(context.Background(), "k", func(context.Context) (int, error) { return 3, nil })
			require.NoError(t, err)
			require.Equal(t, 3, got, "the next call is a new one")
		})
	})
}
