package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// refusedView is what the engine returns with the error for a token it refused:
// the view without an id, and the report that says what to grant.
func refusedView() engine.CredentialView {
	return engine.CredentialView{
		Label:   "main",
		Kind:    "scoped",
		Checked: true,
		Report: credentials.Report{
			Token: cfapi.TokenStatus{ID: "token-id", Status: "active"},
			Checks: []credentials.Check{
				{Capability: credentials.CapToken, OK: true},
				{Capability: credentials.CapDNSWrite, Scope: "example.com", ScopeID: "zone1", Detail: "grant Zone > DNS > Edit on example.com"},
			},
		},
	}
}

var update = flag.Bool("update", false, "write the golden files of the tests")

// requireGolden checks the JSON of an answer, indented, against the golden
// file name.
func requireGolden(t *testing.T, name string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, body, "", "  "))
	buf.WriteByte('\n')
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), buf.String(), "the answer changed; run the test with -update when that is intended")
}

func TestVersion(t *testing.T) {
	f := &fakeEngine{state: engine.State{Profile: "host", Node: "pve1"}}
	rec := do(newServer(f), http.MethodGet, "/v1/version", "")

	require.Equal(t, http.StatusOK, rec.Code)
	requireGolden(t, "version.json", rec.Body.Bytes())
	require.NotContains(t, f.called(), "state", "the version does not copy the state")

	rec = do(newServer(&fakeEngine{interval: 90 * time.Second}), http.MethodGet, "/v1/version", "")
	require.JSONEq(t, `{"version":"1.2.3","boot":"9f2c4e1a0b7d3c55","profile":"","node":"","pollInterval":"1m30s"}`, rec.Body.String(),
		"the interval of the last settings read, and no profile before a cycle read the install")
}

func TestStateIsTheEngineState(t *testing.T) {
	st := testState()
	rec := do(newServer(&fakeEngine{state: st}), http.MethodGet, "/v1/state", "")

	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(st)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())
	require.Equal(t, `"5e0c1f7a92b4d3e8"`, rec.Header().Get("ETag"))
}

func TestAStateTheClientHoldsIsNotSentAgain(t *testing.T) {
	for _, tt := range []struct {
		name, match string
		status      int
	}{
		{"the digest", `"5e0c1f7a92b4d3e8"`, http.StatusNotModified},
		{"weak", `W/"5e0c1f7a92b4d3e8"`, http.StatusNotModified},
		{"one of several", `"0123456789abcdef", "5e0c1f7a92b4d3e8"`, http.StatusNotModified},
		{"any", `*`, http.StatusNotModified},
		{"another digest", `"0123456789abcdef"`, http.StatusOK},
		{"unquoted", `5e0c1f7a92b4d3e8`, http.StatusOK},
		{"none", ``, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := request(http.MethodGet, "/v1/state", "")
			if tt.match != "" {
				req.Header.Set("If-None-Match", tt.match)
			}

			rec := send(newServer(&fakeEngine{state: testState()}), req)

			require.Equal(t, tt.status, rec.Code)
			require.Equal(t, `"5e0c1f7a92b4d3e8"`, rec.Header().Get("ETag"))
			if tt.status == http.StatusNotModified {
				require.Empty(t, rec.Body.String())
			} else {
				require.Contains(t, rec.Body.String(), `"digest":"5e0c1f7a92b4d3e8"`)
			}
		})
	}
}

func TestEveryAnswerCarriesTheBoot(t *testing.T) {
	for _, tt := range []struct {
		name, method, target, body string
		err                        error
		status                     int
	}{
		{"version", http.MethodGet, "/v1/version", "", nil, http.StatusOK},
		{"state", http.MethodGet, "/v1/state", "", nil, http.StatusOK},
		{"a refusal of the engine", http.MethodPost, "/v1/adopt", `{"name":"www.example.com"}`, engine.ErrNotFound, http.StatusNotFound},
		{"a malformed request", http.MethodGet, "/v1/events?since=yesterday", "", nil, http.StatusBadRequest},
		{"an unknown route", http.MethodGet, "/v1/nothing", "", nil, http.StatusNotFound},
		{"a wrong method", http.MethodPut, "/v1/state", "", nil, http.StatusMethodNotAllowed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(newServer(&fakeEngine{err: tt.err}), tt.method, tt.target, tt.body)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Equal(t, testBoot, rec.Header().Get("Pco-Boot"))
		})
	}

	t.Run("not to a peer that is refused", func(t *testing.T) {
		rec := send(checkedServer(&fakeEngine{}, testUID), requestFrom(testUID+1, http.MethodGet, "/v1/version", ""))

		require.Equal(t, http.StatusForbidden, rec.Code)
		require.Empty(t, rec.Header().Values("Pco-Boot"))
	})
}

func TestTheCommandLineOfRootIsTheActor(t *testing.T) {
	for _, tt := range []struct {
		name  string
		uid   uint32
		actor string
	}{
		{"root", 0, "root (cli)"},
		{"another user", testUID + 1, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			s := New(f, "1.2.3", []uint32{0, testUID + 1}, zerolog.Nop())

			rec := send(s, requestFrom(tt.uid, http.MethodPost, "/v1/adopt", `{"name":"www.example.com"}`))

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, tt.actor, engine.ActorOf(f.lastCtx()))
		})
	}
}

func TestEvents(t *testing.T) {
	events := []engine.Event{
		{At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Level: "info", Kind: "route", Subject: "a.example.com", Message: "up"},
	}
	since := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)

	for _, tt := range []struct {
		name      string
		query     string
		wantSince time.Time
	}{
		{"no since means everything", "", time.Time{}},
		{"utc", "?since=2026-03-04T05:00:00Z", since},
		{"fraction", "?since=2026-03-04T05:00:00.5Z", since.Add(500 * time.Millisecond)},
		{"offset", "?since=2026-03-04T07:00:00%2B02:00", since},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{events: events}
			rec := do(newServer(f), http.MethodGet, "/v1/events"+tt.query, "")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			want, err := json.Marshal(events)
			require.NoError(t, err)
			require.JSONEq(t, string(want), rec.Body.String())
			require.True(t, tt.wantSince.Equal(f.lastSince()), "since was %v, want %v", f.lastSince(), tt.wantSince)
		})
	}

	t.Run("no events is an empty list", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/events", "")

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `[]`, rec.Body.String())
	})

	for _, bad := range []string{"yesterday", "2026-03-04", "1709528400", "", "2026-03-04T05:00:00"} {
		t.Run("malformed "+bad, func(t *testing.T) {
			f := &fakeEngine{events: events}
			rec := do(newServer(f), http.MethodGet, "/v1/events?since="+bad, "")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "invalid", errorCode(t, rec))
			require.Contains(t, errorMessage(t, rec), "since")
			require.Empty(t, f.called())
		})
	}
}

func TestTheQueryOfTheEvents(t *testing.T) {
	for _, tt := range []struct {
		name, query string
		want        engine.EventQuery
	}{
		{"nothing", "", engine.EventQuery{}},
		{"after a seq of a boot", "?after=812&boot=9f2c4e1a0b7d3c55", engine.EventQuery{After: 812, Boot: testBoot}},
		{"after a seq of this boot", "?after=812", engine.EventQuery{After: 812}},
		{
			"every list", "?route=a.example.com&route=b.example.com&guest=qemu/101&tunnel=pco-abc123&account=acc1&kind=route&kind=claim&level=warn",
			engine.EventQuery{
				Route: []string{"a.example.com", "b.example.com"}, Guest: []string{"qemu/101"}, Tunnel: []string{"pco-abc123"},
				Account: []string{"acc1"}, Kind: []string{"route", "claim"}, Level: []string{"warn"},
			},
		},
		{"an empty value", "?route=", engine.EventQuery{Route: []string{""}}},
		{"a limit and the history", "?limit=5000&history=1", engine.EventQuery{Limit: 5000, History: true}},
		{"no history", "?history=0&limit=1", engine.EventQuery{Limit: 1}},
		{"since", "?since=2026-10-01T12:00:00Z&kind=admin", engine.EventQuery{Since: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Kind: []string{"admin"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			rec := do(newServer(f), http.MethodGet, "/v1/events"+tt.query, "")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got := f.lastQuery()
			require.True(t, tt.want.Since.Equal(got.Since))
			got.Since, tt.want.Since = time.Time{}, time.Time{}
			require.Equal(t, tt.want, got)
		})
	}

	for _, tt := range []struct{ query, field string }{
		{"?after=-1", "after"},
		{"?after=one", "after"},
		{"?limit=0", "limit"},
		{"?limit=5001", "limit"},
		{"?limit=ten", "limit"},
		{"?history=yes", "history"},
	} {
		t.Run("malformed "+tt.query, func(t *testing.T) {
			f := &fakeEngine{}
			rec := do(newServer(f), http.MethodGet, "/v1/events"+tt.query, "")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "invalid", errorCode(t, rec))
			require.Contains(t, errorMessage(t, rec), tt.field)
			require.Empty(t, f.called())
		})
	}
}

// Every tunnel of an install has the same name: the account tells their
// events apart.
func TestEventsOfAnAccount(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	events := []engine.Event{
		{Seq: 1, At: at, Level: "info", Kind: "action", Subject: "pco-abc123", Message: "put-config in account acc1: 3 rules replace version 2",
			Tunnel: "pco-abc123", Account: "acc1"},
		{Seq: 2, At: at, Level: "info", Kind: "action", Subject: "pco-abc123", Message: "put-config in account acc2: 2 rules replace version 7",
			Tunnel: "pco-abc123", Account: "acc2"},
		{Seq: 3, At: at, Level: "info", Kind: "route", Subject: "www.example.com", Message: "qemu/101: active",
			Route: "www.example.com", Guest: "qemu/101", Account: "acc1"},
		{Seq: 4, At: at, Level: "warn", Kind: "problem", Message: "a problem"},
		{Seq: 5, At: at, Level: "info", Kind: "rollout", Subject: "pco-abc123", Message: "configuration version 4 runs on 1 connector in account acc3",
			Tunnel: "pco-abc123", Account: "acc3"},
		{Seq: 6, At: at, Level: "info", Kind: "action", Subject: "www.example.com", Message: "create-record in zone example.com",
			Route: "www.example.com"},
	}
	seqs := func(t *testing.T, rec *httptest.ResponseRecorder) []uint64 {
		t.Helper()
		var got []engine.Event
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		out := []uint64{}
		for _, ev := range got {
			out = append(out, ev.Seq)
		}
		return out
	}

	rec := do(newServer(&fakeEngine{events: events}), http.MethodGet, "/v1/events?account=acc1", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "events_account.json", rec.Body.Bytes())

	for _, tt := range []struct {
		name, query string
		want        []uint64
	}{
		{"two accounts", "?account=acc3&account=acc1", []uint64{1, 3, 5}},
		{"an account no event is of", "?account=acc9", []uint64{}},
		{"an empty account", "?account=", []uint64{}},
		{"the case of an id", "?account=ACC1", []uint64{}},
		{"no account", "", []uint64{1, 2, 3, 4, 5, 6}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(newServer(&fakeEngine{events: events}), http.MethodGet, "/v1/events"+tt.query, "")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, tt.want, seqs(t, rec))
		})
	}

	t.Run("with since", func(t *testing.T) {
		f := &fakeEngine{events: events[1:]}
		rec := do(newServer(f), http.MethodGet, "/v1/events?since=2026-10-01T11:00:00Z&account=acc1", "")

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, []uint64{3}, seqs(t, rec), "the events since then, of the account")
		require.True(t, at.Add(-time.Hour).Equal(f.lastSince()))
	})
}

func TestSyncTriggersACycle(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/sync", "")

	require.Equal(t, http.StatusAccepted, rec.Code)
	require.JSONEq(t, `{}`, rec.Body.String())
	require.Equal(t, 1, f.triggered())
}

func TestApply(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		call string
	}{
		{"confirming what was offered", `{"confirmDeletes":true,"offer":"0123456789abcdef"}`, "apply:true:0123456789abcdef"},
		{"confirming without an offer", `{"confirmDeletes":true}`, "apply:true:"},
		{"not confirming", `{"confirmDeletes":false}`, "apply:false:"},
		{"empty object", `{}`, "apply:false:"},
		{"no body", ``, "apply:false:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			rec := do(newServer(f), http.MethodPost, "/v1/apply", tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.JSONEq(t, `{"leftObserveOnly":false,"accepted":[]}`, rec.Body.String(), "nothing accepted is a list")
			require.Equal(t, []string{tt.call}, f.called())
		})
	}
}

func TestApplyAnswersWhatWasAccepted(t *testing.T) {
	f := &fakeEngine{applied: engine.ApplyResult{LeftObserveOnly: true, Accepted: []engine.Waiting{
		{Kind: "dns-removals", Detail: "mass delete guard: 6 of 6 records are being removed; confirm to proceed", Items: []string{"a.example.com"}},
	}}}

	rec := do(newServer(f), http.MethodPost, "/v1/apply", `{"confirmDeletes":true,"offer":"0123456789abcdef"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"leftObserveOnly":true,"accepted":[{"kind":"dns-removals","subject":"",`+
		`"detail":"mass delete guard: 6 of 6 records are being removed; confirm to proceed","items":["a.example.com"]}]}`, rec.Body.String())
}

func TestAnOfferThatChangedIsAConflict(t *testing.T) {
	f := &fakeEngine{err: fmt.Errorf("%w: what waits for a confirmation changed since it was shown; look again and repeat", engine.ErrRefused)}

	rec := do(newServer(f), http.MethodPost, "/v1/apply", `{"confirmDeletes":true,"offer":"0123456789abcdef"}`)

	require.Equal(t, http.StatusConflict, rec.Code)
	got := parseError(t, rec)
	require.Equal(t, "refused", got.Code)
	require.Equal(t, "refused: what waits for a confirmation changed since it was shown; look again and repeat", got.Message)
}

func TestAdopt(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/adopt", `{"name":"www.example.com"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{}`, rec.Body.String())
	require.Equal(t, []string{"adopt:www.example.com"}, f.called())
}

func TestRotateTunnel(t *testing.T) {
	for _, tt := range []struct {
		name, body, call string
	}{
		{"an account", `{"account":"acc1"}`, "rotate:acc1"},
		{"the only tunnel", `{}`, "rotate:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			rec := do(newServer(f), http.MethodPost, "/v1/tunnels/rotate", tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.JSONEq(t, `{"tunnel":"pco-abc123","tunnelId":"00000000-0000-4000-8000-000000000001","accountId":"acc1"}`, rec.Body.String())
			require.Equal(t, []string{tt.call}, f.called())
		})
	}
}

func TestRotateTunnelRefusals(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"several tunnels", fmt.Errorf("%w: name one", engine.ErrInvalid), http.StatusBadRequest, "invalid"},
		{"no tunnel", fmt.Errorf("%w: no tunnel", engine.ErrNotFound), http.StatusNotFound, "not_found"},
		{"observe-only", fmt.Errorf("%w: observe-only", engine.ErrRefused), http.StatusConflict, "refused"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(newServer(&fakeEngine{err: tt.err}), http.MethodPost, "/v1/tunnels/rotate", `{}`)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Equal(t, tt.code, errorCode(t, rec))
		})
	}
	t.Run("a field it does not know", func(t *testing.T) {
		f := &fakeEngine{}
		rec := do(newServer(f), http.MethodPost, "/v1/tunnels/rotate", `{"acount":"acc1"}`)

		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Empty(t, f.called())
	})
}

// The web user may use the socket; rotating the secret of a tunnel restarts
// every connector of it and is root's alone.
func TestOnlyRootRotatesASecret(t *testing.T) {
	const web = 4242
	for _, tt := range []struct {
		name   string
		uid    uint32
		status int
	}{
		{"root", 0, http.StatusOK},
		{"the web user", web, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			s := New(f, "1.2.3", []uint32{0, web}, zerolog.Nop())
			s.checkPeers = true

			rec := send(s, requestFrom(tt.uid, http.MethodPost, "/v1/tunnels/rotate", `{}`))

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.status == http.StatusForbidden {
				require.Equal(t, "forbidden", errorCode(t, rec))
				require.Equal(t, "only root may rotate the secret of a tunnel", errorMessage(t, rec))
				require.Empty(t, f.called())
			}
		})
	}
}

func TestCredentialsAreListedByTheEngine(t *testing.T) {
	st := testState()
	f := &fakeEngine{state: engine.State{}, creds: st.Credentials}
	rec := do(newServer(f), http.MethodGet, "/v1/credentials", "")

	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(st.Credentials)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())
	require.Equal(t, []string{"credentials"}, f.called(), "not from the state of the last cycle")

	t.Run("none is an empty list", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/credentials", "")

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestAddCredentialAnswersWithTheViewOnly(t *testing.T) {
	view := engine.CredentialView{ID: "abc12345", Label: "main", Kind: "scoped"}
	f := &fakeEngine{view: view}
	rec := do(newServer(f), http.MethodPost, "/v1/credentials", `{"label":"main","token":"`+testToken+`"}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	want, err := json.Marshal(view)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())
	require.NotContains(t, rec.Body.String(), testToken)
	require.Equal(t, []string{"add:main:" + testToken}, f.called())
}

func TestAPaddedTokenIsTrimmedBeforeTheEngineSeesIt(t *testing.T) {
	f := &fakeEngine{}
	body, err := json.Marshal(map[string]string{"label": "main", "token": "  " + testToken + "\n"})
	require.NoError(t, err)

	rec := do(newServer(f), http.MethodPost, "/v1/credentials", string(body))

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, []string{"add:main:" + testToken}, f.called())
}

func TestTheTokenIsReadFromTheBodyOnly(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/credentials?token="+testToken, `{"label":"main"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "invalid", errorCode(t, rec))
	require.Empty(t, f.called())
}

func TestTheShapeOfATokenIsChecked(t *testing.T) {
	long := strings.Repeat("a", 20)
	for _, tt := range []struct {
		name  string
		token string
		ok    bool
	}{
		{"empty", "", false},
		{"blank", "   ", false},
		{"short", "abc", false},
		{"one letter", "a", false},
		{"nineteen characters", long[:19], false},
		{"twenty characters", long, true},
		{"256 characters", strings.Repeat("a", 256), true},
		{"256 characters and white space", " " + strings.Repeat("a", 256) + "\n", true},
		{"257 characters", strings.Repeat("a", 257), false},
		{"a huge one", strings.Repeat("a", 100_000), false},
		{"all allowed characters", "Abc_def-GHI_jkl-0123456789", true},
		{"surrounding white space", "\t " + long + " \n", true},
		{"space inside", long + " " + long, false},
		{"newline inside", long + "\n" + long, false},
		{"dot", long + ".", false},
		{"slash", long + "/", false},
		{"quote", long + `"`, false},
		{"backslash", long + `\`, false},
		{"percent", long + "%", false},
		{"non ascii letter", long + "é", false},
		{"nul", long + "\x00", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			body, err := json.Marshal(map[string]string{"label": "main", "token": tt.token})
			require.NoError(t, err)

			rec := do(newServer(f), http.MethodPost, "/v1/credentials", string(body))

			if tt.ok {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				require.Equal(t, []string{"add:main:" + strings.TrimSpace(tt.token)}, f.called())
				return
			}
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, "invalid", errorCode(t, rec))
			require.Empty(t, f.called())
			// A short one is part of the fixed message by chance.
			if trimmed := strings.TrimSpace(tt.token); len(trimmed) >= 8 {
				require.NotContains(t, rec.Body.String(), trimmed, "the answer must not echo the token")
			}
		})
	}
}

func TestOnlyAPostTakesABody(t *testing.T) {
	for _, tt := range []struct {
		name, method, target string
		length               int64
	}{
		{"get with a body", http.MethodGet, "/v1/state", 2},
		{"get with a body of unknown length", http.MethodGet, "/v1/state", -1},
		{"delete with a body", http.MethodDelete, "/v1/credentials/abc12345", 2},
		{"delete with a body of unknown length", http.MethodDelete, "/v1/credentials/abc12345", -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			req := requestFrom(testUID, tt.method, tt.target, "xx")
			req.ContentLength = tt.length

			rec := send(newServer(f), req)

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, "invalid", errorCode(t, rec))
			require.Equal(t, "this request takes no body", errorMessage(t, rec))
			require.Equal(t, "close", rec.Header().Get("Connection"), "what follows is not read")
			require.Empty(t, f.called())
		})
	}

	t.Run("an empty body is none", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/state", "")

		require.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestARefusedTokenKeepsItsReport(t *testing.T) {
	view := refusedView()
	f := &fakeEngine{
		view: view,
		err:  fmt.Errorf("%w: the token cannot be used: dns.write on example.com: %s", engine.ErrInvalid, view.Report.Checks[1].Detail),
	}

	rec := do(newServer(f), http.MethodPost, "/v1/credentials", `{"label":"main","token":"`+testToken+`"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	a := parseError(t, rec)
	require.Equal(t, "invalid", a.Code)
	require.Contains(t, a.Message, "grant Zone > DNS > Edit on example.com")
	want, err := json.Marshal(view)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(a.Credential))
	require.NotContains(t, rec.Body.String(), testToken)
}

func TestTheReportOfARefusedTokenIsScrubbed(t *testing.T) {
	view := refusedView()
	view.Report.Checks[1].Detail = "cloudflare rejected " + testToken
	f := &fakeEngine{view: view, err: fmt.Errorf("%w: cloudflare rejected %s", engine.ErrInvalid, testToken)}

	rec := do(newServer(f), http.MethodPost, "/v1/credentials", `{"label":"main","token":"`+testToken+`"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.NotContains(t, rec.Body.String(), testToken)
	a := parseError(t, rec)
	require.Contains(t, a.Message, "[redacted]")
	require.Contains(t, string(a.Credential), "cloudflare rejected [redacted]")
}

func TestAFailureWithoutAReportHasNoCredential(t *testing.T) {
	for _, tt := range []struct {
		name string
		view engine.CredentialView
	}{
		{"zero view", engine.CredentialView{}},
		{"view that was not checked", engine.CredentialView{Label: "main", Kind: "scoped", Checked: false}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{view: tt.view, err: fmt.Errorf("%w: the label is empty", engine.ErrInvalid)}

			rec := do(newServer(f), http.MethodPost, "/v1/credentials", `{"label":"","token":"`+testToken+`"}`)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Nil(t, parseError(t, rec).Credential)
			require.NotContains(t, rec.Body.String(), `"credential"`)
		})
	}
}

func TestTheCheckedFlagDecidesWhetherAViewHasAReport(t *testing.T) {
	for _, tt := range []struct {
		name string
		view engine.CredentialView
		want bool
	}{
		{"checked", engine.CredentialView{Label: "main", Kind: "scoped", Checked: true}, true},
		{"not checked, whatever the report holds", func() engine.CredentialView {
			v := refusedView()
			v.Checked = false
			return v
		}(), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{view: tt.view, err: fmt.Errorf("%w: no", engine.ErrInvalid)}

			rec := do(newServer(f), http.MethodPost, "/v1/credentials", `{"label":"main","token":"`+testToken+`"}`)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, tt.want, parseError(t, rec).Credential != nil, rec.Body.String())
		})
	}
}

func TestOnlyAddingACredentialAnswersWithAView(t *testing.T) {
	for _, rt := range engineRoutes {
		if rt.name == "add credential" {
			continue
		}
		t.Run(rt.name, func(t *testing.T) {
			f := &fakeEngine{view: refusedView(), err: fmt.Errorf("%w: no", engine.ErrInvalid)}

			rec := do(newServer(f), rt.method, rt.target, rt.body)

			require.GreaterOrEqual(t, rec.Code, 400)
			require.Nil(t, parseError(t, rec).Credential)
		})
	}
}

func TestCheckCredential(t *testing.T) {
	view := engine.CredentialView{ID: "abc12345", Label: "main", Kind: "scoped"}
	for _, tt := range []struct {
		name string
		body string
		call string
	}{
		{"deep", `{"deep":true}`, "check:abc12345:true"},
		{"shallow", `{"deep":false}`, "check:abc12345:false"},
		{"no body", ``, "check:abc12345:false"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{view: view}
			rec := do(newServer(f), http.MethodPost, "/v1/credentials/abc12345/check", tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			want, err := json.Marshal(view)
			require.NoError(t, err)
			require.JSONEq(t, string(want), rec.Body.String())
			require.Equal(t, []string{tt.call}, f.called())
		})
	}
}

func TestRemoveCredential(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodDelete, "/v1/credentials/abc12345", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{}`, rec.Body.String())
	require.Equal(t, []string{"remove:abc12345"}, f.called())
}

var engineRoutes = []struct{ name, method, target, body string }{
	{"apply", http.MethodPost, "/v1/apply", `{}`},
	{"credentials", http.MethodGet, "/v1/credentials", ""},
	{"adopt", http.MethodPost, "/v1/adopt", `{"name":"www.example.com"}`},
	{"add credential", http.MethodPost, "/v1/credentials", `{"label":"main","token":"` + testToken + `"}`},
	{"check credential", http.MethodPost, "/v1/credentials/abc12345/check", `{}`},
	{"remove credential", http.MethodDelete, "/v1/credentials/abc12345", ""},
	{"claims", http.MethodGet, "/v1/claims", ""},
	{"resolve claim", http.MethodPost, "/v1/claims/resolve", `{"hostname":"www.example.com","owner":"qemu/102"}`},
	{"approvals", http.MethodGet, "/v1/approvals", ""},
	{"approve guest", http.MethodPost, "/v1/guests/approve", `{"owner":"qemu/101"}`},
	{"revoke guest", http.MethodPost, "/v1/guests/revoke", `{"owner":"qemu/101"}`},
	{"segments", http.MethodGet, "/v1/segments", ""},
	{"acknowledge segment", http.MethodPost, "/v1/segments/acknowledge", `{"bridge":"vmbr1"}`},
	{"revoke segment", http.MethodPost, "/v1/segments/revoke", `{"bridge":"vmbr1"}`},
	{"diagnose", http.MethodGet, "/v1/diagnose?hostname=www.example.com", ""},
}

func TestEngineErrorsAreMapped(t *testing.T) {
	for _, tt := range []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"invalid", fmt.Errorf("%w: the label is empty", engine.ErrInvalid), http.StatusBadRequest, "invalid", "invalid request: the label is empty"},
		{"not found", fmt.Errorf("%w: no credential %q", engine.ErrNotFound, "abc12345"), http.StatusNotFound, "not_found", `not found: no credential "abc12345"`},
		{"refused", fmt.Errorf("%w: credential abc12345 still manages 2 records", engine.ErrRefused), http.StatusConflict, "refused", "refused: credential abc12345 still manages 2 records"},
		{"deadline", fmt.Errorf("checking the token: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, "unavailable", "the operation timed out"},
		{"cancelled", fmt.Errorf("checking the token: %w", context.Canceled), http.StatusServiceUnavailable, "unavailable", "the operation was cancelled"},
		{"busy", fmt.Errorf("%w and took longer than 45s; try again", engine.ErrBusy), http.StatusServiceUnavailable, "unavailable",
			"a cycle is running and took longer than 45s; try again"},
		{"anything else", errors.New("reading the settings: disk gone"), http.StatusInternalServerError, "internal", "reading the settings: disk gone"},
	} {
		for _, rt := range engineRoutes {
			t.Run(tt.name+" on "+rt.name, func(t *testing.T) {
				rec := do(newServer(&fakeEngine{err: tt.err}), rt.method, rt.target, rt.body)

				require.Equal(t, tt.status, rec.Code, rec.Body.String())
				require.Equal(t, tt.code, errorCode(t, rec))
				require.Equal(t, tt.message, errorMessage(t, rec))
			})
		}
	}
}

func TestEveryErrorCarriesACode(t *testing.T) {
	s := newServer(&fakeEngine{})
	for _, tt := range []struct {
		name        string
		req         *http.Request
		status      int
		code        string
		contentType string
	}{
		{"unknown route", request(http.MethodGet, "/v1/nope", ""), http.StatusNotFound, "no_route", ""},
		{"wrong method", request(http.MethodPost, "/v1/state", ""), http.StatusMethodNotAllowed, "method_not_allowed", ""},
		{"wrong content type", func() *http.Request {
			r := request(http.MethodPost, "/v1/apply", `{}`)
			r.Header.Set("Content-Type", "text/plain")
			return r
		}(), http.StatusUnsupportedMediaType, "unsupported_media_type", ""},
		{"too large", request(http.MethodPost, "/v1/apply", `{"confirmDeletes":false}`+strings.Repeat(" ", maxBody)), http.StatusRequestEntityTooLarge, "too_large", ""},
		{"bad body", request(http.MethodPost, "/v1/apply", `nonsense`), http.StatusBadRequest, "invalid", ""},
		{"bad since", request(http.MethodGet, "/v1/events?since=x", ""), http.StatusBadRequest, "invalid", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(s, tt.req)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Equal(t, tt.code, errorCode(t, rec))
		})
	}
}

func TestErrorsDoNotEchoTheSubmittedToken(t *testing.T) {
	const padded = "  " + testToken + "\n"
	for _, tt := range []struct {
		name   string
		err    error
		status int
	}{
		{"invalid", fmt.Errorf("%w: %s is not a token", engine.ErrInvalid, testToken), http.StatusBadRequest},
		{"refused", fmt.Errorf("%w: cloudflare rejected %q", engine.ErrRefused, testToken), http.StatusConflict},
		{"anything else", fmt.Errorf("calling cloudflare with %s: boom", testToken), http.StatusInternalServerError},
		{"as submitted", fmt.Errorf("calling cloudflare with %s: boom", padded), http.StatusInternalServerError},
		{"as submitted and quoted", fmt.Errorf("calling cloudflare with %q: boom", padded), http.StatusInternalServerError},
		{"twice", fmt.Errorf("%s then %s", testToken, testToken), http.StatusInternalServerError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logged syncBuffer
			s := New(&fakeEngine{err: tt.err}, "v", []uint32{testUID}, zerolog.New(&logged).Level(zerolog.DebugLevel))
			body, err := json.Marshal(map[string]string{"label": "main", "token": padded})
			require.NoError(t, err)

			rec := send(s, request(http.MethodPost, "/v1/credentials", string(body)))

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), testToken)
			require.Contains(t, errorMessage(t, rec), "[redacted]")
			require.NotContains(t, logged.String(), testToken)
		})
	}
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	f := &fakeEngine{}
	s := newServer(f)

	for _, tt := range []struct {
		name, method, target string
		status               int
		code                 string
		allow                string
	}{
		{"unknown path", http.MethodGet, "/v1/nope", http.StatusNotFound, "no_route", ""},
		{"root", http.MethodGet, "/", http.StatusNotFound, "no_route", ""},
		{"trailing slash", http.MethodGet, "/v1/state/", http.StatusNotFound, "no_route", ""},
		{"unknown post", http.MethodPost, "/v1/nope", http.StatusNotFound, "no_route", ""},
		{"post to a get route", http.MethodPost, "/v1/state", http.StatusMethodNotAllowed, "method_not_allowed", "GET"},
		{"get to a post route", http.MethodGet, "/v1/apply", http.StatusMethodNotAllowed, "method_not_allowed", "POST"},
		{"put on credentials", http.MethodPut, "/v1/credentials", http.StatusMethodNotAllowed, "method_not_allowed", ""},
		{"delete on the collection", http.MethodDelete, "/v1/credentials", http.StatusMethodNotAllowed, "method_not_allowed", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil) // no content type: the route is not found first
			rec := send(s, req.WithContext(withPeerUID(req.Context(), testUID)))

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			require.Equal(t, tt.code, errorCode(t, rec))
			require.NotEmpty(t, errorMessage(t, rec))
			if tt.allow != "" {
				require.Equal(t, tt.allow, rec.Header().Get("Allow"))
			}
		})
	}
	require.Empty(t, f.called())
}

func TestRequestBodies(t *testing.T) {
	big := `{"label":"` + strings.Repeat("a", 1<<20) + `"}`
	for _, tt := range []struct {
		name        string
		target      string
		contentType string
		body        string
		status      int
		code        string
		contains    string
	}{
		{"no content type", "/v1/apply", "", `{}`, http.StatusUnsupportedMediaType, "unsupported_media_type", "application/json"},
		{"wrong content type", "/v1/apply", "text/plain", `{}`, http.StatusUnsupportedMediaType, "unsupported_media_type", "application/json"},
		{"form content type", "/v1/apply", "application/x-www-form-urlencoded", `confirmDeletes=true`, http.StatusUnsupportedMediaType, "unsupported_media_type", "application/json"},
		{"charset is fine", "/v1/apply", "application/json; charset=utf-8", `{}`, http.StatusOK, "", ""},
		{"upper case is fine", "/v1/apply", "Application/JSON", `{}`, http.StatusOK, "", ""},
		{"unknown field", "/v1/apply", "application/json", `{"confirmDeletes":true,"force":true}`, http.StatusBadRequest, "invalid", "force"},
		{"not json", "/v1/apply", "application/json", `confirmDeletes`, http.StatusBadRequest, "invalid", "JSON"},
		{"truncated", "/v1/adopt", "application/json", `{"name":`, http.StatusBadRequest, "invalid", "JSON"},
		{"wrong type", "/v1/apply", "application/json", `{"confirmDeletes":"yes"}`, http.StatusBadRequest, "invalid", "confirmDeletes"},
		{"an offer that is no string", "/v1/apply", "application/json", `{"confirmDeletes":true,"offer":7}`, http.StatusBadRequest, "invalid", "offer"},
		{"not an object", "/v1/apply", "application/json", `[true]`, http.StatusBadRequest, "invalid", "object"},
		{"two objects", "/v1/apply", "application/json", `{} {}`, http.StatusBadRequest, "invalid", "after"},
		{"trailing junk", "/v1/apply", "application/json", `{}x`, http.StatusBadRequest, "invalid", "after"},
		{"adopt needs a body", "/v1/adopt", "application/json", ``, http.StatusBadRequest, "invalid", "empty"},
		{"add needs a body", "/v1/credentials", "application/json", ``, http.StatusBadRequest, "invalid", "empty"},
		{"too large", "/v1/credentials", "application/json", big, http.StatusRequestEntityTooLarge, "too_large", "too large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			req := httptest.NewRequest(http.MethodPost, tt.target, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rec := send(newServer(f), req.WithContext(withPeerUID(req.Context(), testUID)))

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.status == http.StatusOK {
				return
			}
			require.Equal(t, tt.code, errorCode(t, rec))
			require.Contains(t, errorMessage(t, rec), tt.contains)
			require.Empty(t, f.called())
		})
	}

	t.Run("a body just under the limit is read", func(t *testing.T) {
		f := &fakeEngine{}
		pad := strings.Repeat(" ", 1<<20-len(`{"deep":true}`))
		rec := do(newServer(f), http.MethodPost, "/v1/credentials/abc12345/check", `{"deep":true}`+pad)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, []string{"check:abc12345:true"}, f.called())
	})

	t.Run("a body over the limit closes the connection", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodPost, "/v1/apply", `{}`+strings.Repeat(" ", 1<<20))

		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		require.Equal(t, "close", rec.Header().Get("Connection"))
	})

	t.Run("get and delete need no content type", func(t *testing.T) {
		s := newServer(&fakeEngine{})
		for _, tt := range []struct{ method, target string }{
			{http.MethodGet, "/v1/version"},
			{http.MethodGet, "/v1/state"},
			{http.MethodDelete, "/v1/credentials/abc12345"},
		} {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			rec := send(s, req.WithContext(withPeerUID(req.Context(), testUID)))
			require.Equal(t, http.StatusOK, rec.Code, tt.method+" "+tt.target)
		}
	})
}

func TestTheRequestContextReachesTheEngine(t *testing.T) {
	type key struct{}
	f := &fakeEngine{}
	for _, rt := range engineRoutes {
		if rt.name == "claims" || rt.name == "approvals" || rt.name == "credentials" || rt.name == "segments" {
			continue // read from the store, with nothing to cancel
		}
		t.Run(rt.name, func(t *testing.T) {
			req := request(rt.method, rt.target, rt.body)
			req = req.WithContext(context.WithValue(req.Context(), key{}, "marker"))

			rec := send(newServer(f), req)

			require.Less(t, rec.Code, 300, rec.Body.String())
			require.Equal(t, "marker", f.lastCtx().Value(key{}))
		})
	}
}

func TestClaimsAreListed(t *testing.T) {
	f := &fakeEngine{claims: testClaims()}

	rec := do(newServer(f), http.MethodGet, "/v1/claims", "")

	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(f.claims)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())

	t.Run("none is an empty list", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/claims", "")

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestApprovalsAreListed(t *testing.T) {
	f := &fakeEngine{approvals: []engine.ApprovalView{{Owner: "qemu/101", Identity: "uuid:101"}}}

	rec := do(newServer(f), http.MethodGet, "/v1/approvals", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `[{"owner":"qemu/101","identity":"uuid:101","matches":false}]`, rec.Body.String())

	t.Run("none is an empty list", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/approvals", "")

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestTheAdminActionsOnClaimsAndGuests(t *testing.T) {
	for _, tt := range []struct {
		name, target, body, call string
	}{
		{"resolve a claim", "/v1/claims/resolve", `{"hostname":"www.example.com","owner":"qemu/102"}`, "resolve:www.example.com:qemu/102"},
		{"approve a guest", "/v1/guests/approve", `{"owner":"qemu/101"}`, "approve:qemu/101:"},
		{"approve a guest as it was shown", "/v1/guests/approve", `{"owner":"qemu/101","identity":"uuid:101"}`, "approve:qemu/101:uuid:101"},
		{
			"approve a guest with what it waits for", "/v1/guests/approve",
			`{"owner":"qemu/101","identity":"uuid:101","macs":["bc:24:11:00:00:09"],"addresses":["10.0.0.1"]}`,
			"approve:qemu/101:uuid:101 macs=[bc:24:11:00:00:09] addresses=[10.0.0.1]",
		},
		{"revoke a guest", "/v1/guests/revoke", `{"owner":"lxc/200"}`, "revoke:lxc/200"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}

			rec := do(newServer(f), http.MethodPost, tt.target, tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			switch {
			case strings.HasSuffix(tt.name, "waits for"):
				require.JSONEq(t, `{"owner":"qemu/101","identity":"uuid:1","mode":"approve","macs":["bc:24:11:00:00:09"],"addresses":["10.0.0.1"]}`,
					rec.Body.String(), "the approval as it was made")
			case strings.HasPrefix(tt.name, "approve"):
				require.JSONEq(t, `{"owner":"qemu/101","identity":"uuid:1","mode":"approve"}`, rec.Body.String(), "the approval as it was made")
			default:
				require.JSONEq(t, `{}`, rec.Body.String())
			}
			require.Equal(t, []string{tt.call}, f.called())
		})
		t.Run(tt.name+" needs a body", func(t *testing.T) {
			f := &fakeEngine{}

			rec := do(newServer(f), http.MethodPost, tt.target, "")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "the request body is empty", errorMessage(t, rec))
			require.Empty(t, f.called())
		})
		t.Run(tt.name+" takes no other field", func(t *testing.T) {
			f := &fakeEngine{}

			rec := do(newServer(f), http.MethodPost, tt.target, `{"owner":"qemu/1","force":true}`)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, errorMessage(t, rec), "force")
			require.Empty(t, f.called())
		})
	}
}

func TestTheSegmentsAreListed(t *testing.T) {
	f := &fakeEngine{segments: []engine.SegmentView{
		{Bridge: "vmbr1", Acknowledged: true, AcknowledgedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Routes: 2},
		{Bridge: "vmbr1", VLAN: 20, Routes: 1},
	}}

	rec := do(newServer(f), http.MethodGet, "/v1/segments", "")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `[
		{"bridge":"vmbr1","acknowledged":true,"acknowledgedAt":"2026-10-01T12:00:00Z","routes":2},
		{"bridge":"vmbr1","vlan":20,"acknowledged":false,"routes":1}
	]`, rec.Body.String())

	t.Run("none is an empty list", func(t *testing.T) {
		rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/segments", "")

		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `[]`, rec.Body.String())
	})
}

func TestTheAdminActionsOnSegments(t *testing.T) {
	for _, tt := range []struct {
		name, target, body, call string
	}{
		{"acknowledge a bridge", "/v1/segments/acknowledge", `{"bridge":"vmbr1"}`, "acknowledge:vmbr1:0"},
		{"acknowledge a VLAN", "/v1/segments/acknowledge", `{"bridge":"vmbr1","vlan":20}`, "acknowledge:vmbr1:20"},
		{"revoke a VLAN", "/v1/segments/revoke", `{"bridge":"vmbr1","vlan":20}`, "unacknowledge:vmbr1:20"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}

			rec := do(newServer(f), http.MethodPost, tt.target, tt.body)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.JSONEq(t, `{}`, rec.Body.String())
			require.Equal(t, []string{tt.call}, f.called())
		})
		t.Run(tt.name+" needs a body", func(t *testing.T) {
			f := &fakeEngine{}

			rec := do(newServer(f), http.MethodPost, tt.target, "")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "the request body is empty", errorMessage(t, rec))
			require.Empty(t, f.called())
		})
		t.Run(tt.name+" takes no other field", func(t *testing.T) {
			f := &fakeEngine{}

			rec := do(newServer(f), http.MethodPost, tt.target, `{"bridge":"vmbr1","force":true}`)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, errorMessage(t, rec), "force")
			require.Empty(t, f.called())
		})
	}
}

func TestDiagnose(t *testing.T) {
	steps := []doctor.Step{
		{Name: "route", Level: doctor.LevelOK, Detail: "qemu/101 holds it"},
		{Name: "zone", Level: doctor.LevelFail, Detail: "no Cloudflare zone for this hostname in any credential"},
	}
	f := &fakeEngine{steps: steps}

	rec := do(newServer(f), http.MethodGet, "/v1/diagnose?hostname=www.example.com", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	want, err := json.Marshal(steps)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())
	require.Equal(t, []string{"diagnose:www.example.com"}, f.called())

	for _, query := range []string{"", "?hostname=", "?host=www.example.com", "?hostname=a.example.com&hostname=b.example.com"} {
		t.Run("query "+query, func(t *testing.T) {
			f := &fakeEngine{}

			rec := do(newServer(f), http.MethodGet, "/v1/diagnose"+query, "")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "invalid", errorCode(t, rec))
			require.Equal(t, "name one hostname: /v1/diagnose?hostname=<name>", errorMessage(t, rec))
			require.Empty(t, f.called())
		})
	}
}

func TestDoctor(t *testing.T) {
	findings := []doctor.Finding{
		{Check: "mode", Level: doctor.LevelWarn, Detail: "observe-only: nothing is changed at Cloudflare", Fix: "pco apply"},
		{Check: "store", Level: doctor.LevelOK, Detail: "the store is mounted and set up"},
	}
	f := &fakeEngine{findings: findings}

	rec := do(newServer(f), http.MethodGet, "/v1/doctor", "")

	require.Equal(t, http.StatusOK, rec.Code)
	want, err := json.Marshal(findings)
	require.NoError(t, err)
	require.JSONEq(t, string(want), rec.Body.String())
	require.Equal(t, []string{"doctor"}, f.called())
}
