package credentials

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

const unexpectedAnswer = "unexpected answer to the probe create; nothing deleted; probe may exist: "

func TestCreateOfUnknownOutcomeRemovesTheProbe(t *testing.T) {
	f := newFake()
	spy := &spyAPI{API: f, lostCreate: errors.New("i/o timeout")}

	got := newChecker().Run(t.Context(), spy, true)

	require.Equal(t, failed(CapDNSWrite, exampleCom, "i/o timeout"), find(t, got, CapDNSWrite, exampleCom))
	require.Equal(t, failed(CapTunnelWrite, acme, "i/o timeout"), find(t, got, CapTunnelWrite, acme))
	require.False(t, got.Usable)
	requireNothingLeft(t, f, []string{"zone1"}, []string{"acct1"})
	require.Equal(t, []cfapi.RecordFilter{
		{CommentPrefix: "pco:abc"},
		{Type: "TXT", Name: probeRecord, CommentPrefix: "pco:abc"},
	}, spy.filters)
	require.Equal(t, []string{
		"VerifyToken",
		"Accounts",
		"Zones",
		"Records zone1",
		"CreateRecord zone1 " + probeRecord,
		"Records zone1",
		"DeleteRecord zone1 rec-1",
		"FindTunnel acct1 pco-abc",
		"CreateTunnel acct1 " + probeTunnel,
		"FindTunnel acct1 " + probeTunnel,
		"DeleteTunnel acct1 00000000-0000-4000-8000-000000000001",
	}, f.Calls())
}

func TestLookupAfterUnknownOutcomeDeletesOnlyTheProbe(t *testing.T) {
	f := newFake()
	f.SeedRecord("zone1", cfapi.Record{Type: "TXT", Name: probeRecord, Content: "mine", Comment: "pco:abc"})
	f.SeedRecord("zone1", cfapi.Record{Type: "TXT", Name: probeRecord, Content: "longer", Comment: "pco:abc probe of something else"})
	f.SeedRecord("zone1", cfapi.Record{Type: "TXT", Name: "_pco-probe-other.example.com", Content: "x", Comment: "pco:abc probe"})
	seeded := f.RecordsIn("zone1")
	spy := &spyAPI{API: f, lostCreate: errors.New("i/o timeout")}

	got := newChecker().Run(t.Context(), spy, true)

	require.Equal(t, "i/o timeout", find(t, got, CapDNSWrite, exampleCom).Detail)
	require.Equal(t, seeded, f.RecordsIn("zone1"))
	require.Len(t, callsTo(f, "DeleteRecord"), 1)
}

func TestFailedLookupAfterUnknownOutcomeSaysTheProbeMayExist(t *testing.T) {
	tests := []struct {
		name  string
		spy   func(f *spyAPI)
		cause string
	}{
		{
			name: "lookup fails",
			spy:  func(s *spyAPI) { s.lookupErr = errors.New("lookup failed") },
		},
		{
			name: "delete fails",
			spy:  func(s *spyAPI) { s.deleteErr = errors.New("delete failed") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			spy := &spyAPI{API: f, lostCreate: errors.New("i/o timeout")}
			tc.spy(spy)

			got := newChecker().Run(t.Context(), spy, true)

			require.Equal(t,
				failed(CapDNSWrite, exampleCom, "i/o timeout; probe may exist: "+probeRecord),
				find(t, got, CapDNSWrite, exampleCom))
			require.Equal(t,
				failed(CapTunnelWrite, acme, "i/o timeout; probe may exist: "+probeTunnel),
				find(t, got, CapTunnelWrite, acme))
			require.False(t, got.Usable)
			require.Len(t, f.RecordsIn("zone1"), 1)
			require.Len(t, f.TunnelsIn("acct1"), 1)
		})
	}
}

func TestOnlyAnUnknownOutcomeIsLookedUp(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		lookup bool
	}{
		{"unauthorised", &cfapi.Error{Status: 401, Message: "no"}, false},
		{"forbidden", &cfapi.Error{Status: 403, Message: "no"}, false},
		{"bad request", &cfapi.Error{Status: 400, Message: "bad"}, false},
		{"not found", &cfapi.Error{Status: 404, Message: "none"}, false},
		{"conflict", &cfapi.Error{Status: 409, Message: "exists"}, false},
		{"unprocessable", &cfapi.Error{Status: 422, Message: "invalid"}, false},
		{"rate limited", &cfapi.Error{Status: 429, Message: "slow down"}, false},
		{"invalid argument", fmt.Errorf("creating: %w", cfapi.ErrInvalidArgument), false},
		{"request timeout", &cfapi.Error{Status: 408, Message: "timeout"}, true},
		{"server error", &cfapi.Error{Status: 500, Message: "oops"}, true},
		{"bad gateway", &cfapi.Error{Status: 502, Message: "gateway"}, true},
		{"network error", errors.New("connection reset by peer"), true},
		{"deadline", fmt.Errorf("creating: %w", context.DeadlineExceeded), true},
		{"unreadable answer", fmt.Errorf("decoding: %w", errors.New("unexpected end of JSON input")), true},
	}
	for _, op := range []struct{ name, op, lookup, delete string }{
		{"record", "dns.write", "Records", "DeleteRecord"},
		{"tunnel", "tunnel.write", "FindTunnel", "DeleteTunnel"},
	} {
		for _, tc := range tests {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				f := newFake()
				f.FailNext(op.op, 1, tc.err)

				got := newChecker().Run(t.Context(), f, true)

				wantCalls := 1 // the read probe
				if tc.lookup {
					wantCalls++
				}
				require.Len(t, callsTo(f, op.lookup), wantCalls)
				require.Empty(t, callsTo(f, op.delete))
				require.Len(t, failures(got), 1)
				require.NotContains(t, failures(got)[0].Detail, "may exist")
				if !cfapi.IsAuth(tc.err) {
					require.Equal(t, tc.err.Error(), failures(got)[0].Detail)
				}
				require.False(t, got.Usable)
			})
		}
	}
}

// pausedWrites sends the probe creates through a real client, so that they
// meet its limiter.
type pausedWrites struct {
	cfapi.API
	client *cfapi.Client
}

func (p pausedWrites) CreateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	return p.client.CreateRecord(ctx, zoneID, r)
}

func (p pausedWrites) CreateTunnel(ctx context.Context, accountID, name string) (cfapi.Tunnel, error) {
	return p.client.CreateTunnel(ctx, accountID, name)
}

func TestProbeCreateHeldByAPausedLimiterWasNotSent(t *testing.T) {
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	limiter := cfapi.NewLimiter(300, 5*time.Minute, 20, nil)
	limiter.Pause(time.Hour) // as after a 429
	client, err := cfapi.New(cfapi.Options{BaseURL: srv.URL, Token: "secret", Limiter: limiter})
	require.NoError(t, err)
	f := newFake()

	got := newChecker().Run(t.Context(), pausedWrites{API: f, client: client}, true)

	for _, check := range []Check{find(t, got, CapDNSWrite, exampleCom), find(t, got, CapTunnelWrite, acme)} {
		require.False(t, check.OK)
		require.Contains(t, check.Detail, "HTTP 429")
		require.NotContains(t, check.Detail, "may exist", "a create that was never sent left nothing behind")
	}
	require.False(t, got.Usable)
	require.Zero(t, sent.Load(), "nothing reached Cloudflare")
	require.Len(t, callsTo(f, "Records"), 1, "no lookup of a probe record that was never sent")
	require.Len(t, callsTo(f, "FindTunnel"), 1, "no lookup of a probe tunnel that was never sent")
	require.Empty(t, callsTo(f, "DeleteRecord", "DeleteTunnel"))
}

func TestCancelledCreateOfUnknownOutcomeStillRemovesTheProbe(t *testing.T) {
	tests := []struct {
		name string
		hook func(s *spyAPI, cancel context.CancelFunc)
	}{
		{"record", func(s *spyAPI, cancel context.CancelFunc) { s.afterCreateRecord = cancel }},
		{"tunnel", func(s *spyAPI, cancel context.CancelFunc) { s.afterCreateTunnel = cancel }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			spy := &spyAPI{API: f, lostCreate: context.Canceled}
			tc.hook(spy, cancel)

			got := newChecker().Run(ctx, spy, true)

			requireNothingLeft(t, f, []string{"zone1"}, []string{"acct1"})
			require.Equal(t, context.Canceled.Error(), find(t, got, CapDNSWrite, exampleCom).Detail)
			require.False(t, got.Usable)
		})
	}
}

func TestUnexpectedAnswerToTheCreateDeletesNothing(t *testing.T) {
	tests := []struct {
		name   string
		record func(got, foreign cfapi.Record) cfapi.Record
		tunnel func(got, foreign cfapi.Tunnel) cfapi.Tunnel
	}{
		{
			name:   "another name",
			record: func(got, _ cfapi.Record) cfapi.Record { got.Name = "other.example.com"; return got },
			tunnel: func(got, _ cfapi.Tunnel) cfapi.Tunnel { got.Name = "other"; return got },
		},
		{
			name:   "another comment",
			record: func(got, _ cfapi.Record) cfapi.Record { got.Comment = "pco:abc"; return got },
			tunnel: func(got, _ cfapi.Tunnel) cfapi.Tunnel { got.Name = probeTunnel + "x"; return got },
		},
		{
			name:   "no id",
			record: func(got, _ cfapi.Record) cfapi.Record { got.ID = ""; return got },
			tunnel: func(got, _ cfapi.Tunnel) cfapi.Tunnel { got.ID = ""; return got },
		},
		{
			name:   "someone else's object",
			record: func(_, foreign cfapi.Record) cfapi.Record { return foreign },
			tunnel: func(_, foreign cfapi.Tunnel) cfapi.Tunnel { return foreign },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			foreignRecord := f.SeedRecord("zone1", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "x.cfargotunnel.com", Comment: "pco:abc"})
			foreignTunnel := f.SeedTunnel("acct1", "someone-elses", nil)
			spy := &spyAPI{
				API:          f,
				answerRecord: func(got cfapi.Record) cfapi.Record { return tc.record(got, foreignRecord) },
				answerTunnel: func(got cfapi.Tunnel) cfapi.Tunnel { return tc.tunnel(got, foreignTunnel) },
			}

			got := newChecker().Run(t.Context(), spy, true)

			require.Equal(t, failed(CapDNSWrite, exampleCom, unexpectedAnswer+probeRecord), find(t, got, CapDNSWrite, exampleCom))
			require.Equal(t, failed(CapTunnelWrite, acme, unexpectedAnswer+probeTunnel), find(t, got, CapTunnelWrite, acme))
			require.False(t, got.Usable)
			require.Empty(t, callsTo(f, "DeleteRecord", "DeleteTunnel"))
			require.Len(t, f.RecordsIn("zone1"), 2) // the foreign one and the probe, which nothing deleted
			require.Len(t, f.TunnelsIn("acct1"), 2)
			require.Contains(t, f.RecordsIn("zone1"), foreignRecord)
			require.Contains(t, f.TunnelsIn("acct1"), foreignTunnel)
		})
	}
}

func TestAnswerNamingTheProbeInAnotherCaseIsAccepted(t *testing.T) {
	f := newFake()
	spy := &spyAPI{
		API: f,
		answerRecord: func(r cfapi.Record) cfapi.Record {
			r.Name = "_PCO-Probe-R4ND.Example.COM"
			return r
		},
	}

	got := newChecker().Run(t.Context(), spy, true)

	require.Equal(t, ok(CapDNSWrite, exampleCom), find(t, got, CapDNSWrite, exampleCom))
	require.Empty(t, f.RecordsIn("zone1"))
}

func TestProbeAlreadyDeletedCountsAsDeleted(t *testing.T) {
	f := newFake()
	spy := &spyAPI{API: f, deleteGone: true}

	got := newChecker().Run(t.Context(), spy, true)

	require.Equal(t, ok(CapDNSWrite, exampleCom), find(t, got, CapDNSWrite, exampleCom))
	require.Equal(t, ok(CapTunnelWrite, acme), find(t, got, CapTunnelWrite, acme))
	require.True(t, got.Usable)
	requireNothingLeft(t, f, []string{"zone1"}, []string{"acct1"})
}
