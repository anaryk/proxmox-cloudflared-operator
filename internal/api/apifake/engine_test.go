package apifake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

var t0 = time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func load(t *testing.T, name string) (*Engine, *clock) {
	t.Helper()
	c := &clock{t: t0}
	e, err := Scenario(name, c.now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, e.Close()) })
	return e, c
}

// daemon serves e with the real API on a socket, and its controls.
type daemon struct {
	*apiclient.Client
	socket  string
	control string
}

func serve(t *testing.T, e *Engine) daemon {
	t.Helper()
	socket := filepath.Join(testutil.ShortDir(t), "pco", "pco.sock")
	srv := api.New(e, "1.2.3", []uint32{uint32(os.Getuid())}, zerolog.Nop())
	if uid := os.Getuid(); uid != 0 {
		srv.SetWebUID(uint32(uid))
	}
	ready := make(chan struct{})
	srv.OnListening(func() { close(ready) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, socket, os.Getgid()) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	control := httptest.NewServer(e.Control())
	t.Cleanup(func() {
		control.Close()
		cancel()
		require.NoError(t, <-done)
	})
	return daemon{Client: apiclient.New(socket), socket: socket, control: control.URL}
}

// reply is an answer of the API.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// call makes a request of the API as the web interface does.
func (d daemon) call(t *testing.T, method, path, body string, header ...string) reply {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", d.socket)
	}}}
	req, err := http.NewRequest(method, "http://pco"+path, strings.NewReader(body))
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return reply{res.StatusCode, res.Header, data}
}

// steer uses a control and returns its answer.
func (d daemon) steer(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, d.control+path, strings.NewReader(body))
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return res.StatusCode, data
}

func (d daemon) subscribers(t *testing.T) int {
	t.Helper()
	status, data := d.steer(t, http.MethodGet, "/subscribers", "")
	require.Equal(t, http.StatusOK, status)
	var out struct{ Subscribers int }
	require.NoError(t, json.Unmarshal(data, &out))
	return out.Subscribers
}

func next(t *testing.T, ch <-chan engine.Notice) engine.Notice {
	t.Helper()
	select {
	case n, ok := <-ch:
		require.True(t, ok, "the stream ended")
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notice came")
	}
	return engine.Notice{}
}

func nothing(t *testing.T, ch <-chan engine.Notice) {
	t.Helper()
	select {
	case n, ok := <-ch:
		require.False(t, ok, "a notice came: %+v", n)
		t.Fatal("the stream ended")
	case <-time.After(200 * time.Millisecond):
	}
}

func ended(t *testing.T, ch <-chan engine.Notice) {
	t.Helper()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the stream did not end")
		}
	}
}

func TestEveryScenarioLoadsAndEndsItsLastCycleNow(t *testing.T) {
	for _, name := range Scenarios() {
		e, _ := load(t, name)
		st := e.State()
		require.Equal(t, digestOf(st), st.Digest, name)
		if !st.At.IsZero() {
			require.Equal(t, t0, st.FinishedAt, name)
		}
		_, err := json.Marshal(st)
		require.NoError(t, err, name)
	}
}

func TestLoadReadsADirectory(t *testing.T) {
	e, err := Load(filepath.Join(scenariosDir, "populated"), func() time.Time { return t0 })
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	require.NotEmpty(t, e.State().Routes)

	_, err = Load(t.TempDir(), nil)
	require.ErrorContains(t, err, "state.json")
	_, err = Scenario("nothing", nil)
	require.ErrorContains(t, err, "there is no scenario")
}

func TestTheAPIAnswersFromTheScenario(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()

	st, err := d.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, e.State().Digest, st.Digest)
	require.Len(t, st.Routes, len(e.State().Routes))

	res := d.call(t, http.MethodGet, "/v1/version", "")
	var v api.Version
	require.NoError(t, json.Unmarshal(res.body, &v))
	require.Equal(t, api.Version{Version: "1.2.3", Boot: e.Boot(), Profile: "host", Node: "pve1", PollInterval: "10s"}, v)
	require.Equal(t, e.Boot(), res.header.Get("Pco-Boot"))

	res = d.call(t, http.MethodGet, "/v1/state", "", "If-None-Match", `"`+st.Digest+`"`)
	require.Equal(t, http.StatusNotModified, res.status)

	tv, err := d.Traffic(ctx)
	require.NoError(t, err)
	require.Len(t, tv.Tunnels, 1)
	require.Equal(t, t0, tv.Tunnels[0].Samples[len(tv.Tunnels[0].Samples)-1].At)
	require.NotEmpty(t, tv.Routes)
	res = d.call(t, http.MethodGet, "/v1/traffic/route?hostname=www.example.com", "")
	require.Equal(t, http.StatusOK, res.status)
	var series engine.RouteSeries
	require.NoError(t, json.Unmarshal(res.body, &series))
	require.Equal(t, "10.0.0.11:8080", series.Target)
	require.Len(t, series.Samples, routeHistory)
	res = d.call(t, http.MethodGet, "/v1/traffic/route?hostname=media.example.io", "")
	require.Equal(t, http.StatusNotFound, res.status)

	events, err := d.Events(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, events, 10)
	require.Equal(t, uint64(10), events[9].Seq)
	require.Equal(t, e.Boot(), events[9].Boot)

	creds, err := d.Credentials(ctx)
	require.NoError(t, err)
	require.Len(t, creds, 2)
	claims, err := d.Claims(ctx)
	require.NoError(t, err)
	require.Len(t, claims, 5)
	approvals, err := d.Approvals(ctx)
	require.NoError(t, err)
	require.Len(t, approvals, 2)
	segments, err := d.Segments(ctx)
	require.NoError(t, err)
	require.Len(t, segments, 2)
	guests, err := d.Guests(ctx)
	require.NoError(t, err)
	require.Len(t, guests, len(e.guests))

	res = d.call(t, http.MethodGet, "/v1/guests/qemu/103/annotation", "")
	require.Equal(t, http.StatusOK, res.status)
	var note engine.AnnotationView
	require.NoError(t, json.Unmarshal(res.body, &note))
	require.Equal(t, "```cf-tunnel\napp.example.com -> :3000\n```", note.Block)
	require.Equal(t, 4, note.StartLine)
	res = d.call(t, http.MethodGet, "/v1/guests/qemu/999/annotation", "")
	require.Equal(t, http.StatusNotFound, res.status)

	settings, err := d.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, settings.Rev)
	require.Equal(t, "approve", settings.Settings.Admission)
	require.Equal(t, []string{"cloudflareBudget", "gateTag", "trustStatic", "trustedCIDRs"}, settings.ReadAtStart)
	require.Contains(t, settings.Limits, "pollInterval")
	manual, err := d.ManualRoutes(ctx)
	require.NoError(t, err)
	require.Len(t, manual, 1)
	require.Equal(t, "status.example.com", manual[0].Hostname)
}

func TestDiagnosisWalksTheRouteAndAsksNoTarget(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()

	steps, err := d.Diagnose(ctx, "www.example.com")
	require.NoError(t, err)
	require.Len(t, steps, 8)
	require.Equal(t, doctor.Step{Name: "http", Level: doctor.LevelOK, Detail: "the origin answered 200 OK"}, steps[7])

	steps, err = d.Diagnose(ctx, "monitor.example.com")
	require.NoError(t, err)
	require.Equal(t, doctor.LevelFail, steps[5].Level, "the identity of an unreachable target fails")
	require.True(t, steps[7].Skipped)

	_, err = d.Diagnose(ctx, "nobody.example.com")
	require.ErrorIs(t, err, engine.ErrNotFound)
}

func TestTheDoctorChecksTheStateOnAHealthyHost(t *testing.T) {
	e, _ := load(t, "populated")
	findings, err := serve(t, e).Doctor(context.Background())
	require.NoError(t, err)
	byCheck := map[string]doctor.Finding{}
	for _, f := range findings {
		byCheck[f.Check] = f
	}
	require.Equal(t, doctor.LevelOK, byCheck["cycle"].Level)
	require.Equal(t, doctor.LevelOK, byCheck["cloudflared"].Level)
	require.Equal(t, doctor.LevelFail, byCheck["rogue connectors"].Level)
}

func TestAdminActionsChangeWhatIsServed(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()
	offer := e.State().Offer

	_, err := d.Apply(ctx, true, "0000000000000000")
	require.ErrorIs(t, err, engine.ErrRefused)
	res, err := d.Apply(ctx, true, offer)
	require.NoError(t, err)
	require.Len(t, res.Accepted, 4)
	st := e.State()
	require.Empty(t, st.Waiting)
	require.Empty(t, st.Offer)

	require.NoError(t, d.Adopt(ctx, "api.example.com"))
	require.ErrorIs(t, d.Adopt(ctx, "www.example.com"), engine.ErrNotFound)

	_, err = e.RotateTunnel(ctx, "")
	require.ErrorIs(t, err, engine.ErrInvalid, "the install has tunnels in several accounts")
	rot, err := e.RotateTunnel(ctx, "acc1")
	require.NoError(t, err)
	require.Equal(t, engine.TunnelRotation{Tunnel: "pco-abc123", TunnelID: "00000000-0000-4000-8000-000000000001", Account: "acc1"}, rot)

	cred, err := d.AddCredential(ctx, "third", strings.Repeat("t", 40))
	require.NoError(t, err)
	require.Equal(t, "cred3", cred.ID)
	require.True(t, cred.Checked)
	cred, err = d.CheckCredential(ctx, "cred3", true)
	require.NoError(t, err)
	require.True(t, cred.Report.Deep)
	require.NoError(t, d.RemoveCredential(ctx, "cred3"))
	require.ErrorIs(t, d.RemoveCredential(ctx, "cred3"), engine.ErrNotFound)

	require.NoError(t, d.ResolveClaim(ctx, "www.example.com", "qemu/102"))
	claims, err := d.Claims(ctx)
	require.NoError(t, err)
	for _, c := range claims {
		if c.Hostname == "www.example.com" {
			require.Equal(t, "qemu/102", c.Holder)
			require.Equal(t, "qemu/101", c.Waiting[len(c.Waiting)-1].Owner)
		}
	}
	require.ErrorIs(t, d.ResolveClaim(ctx, "www.example.com", "qemu/109"), engine.ErrRefused)

	approved, err := d.ApproveGuest(ctx, "lxc/201", "uuid:201", nil, nil)
	require.NoError(t, err)
	require.Equal(t, "approve", approved.Mode)
	require.NotContains(t, unapproved(e.State()), "lxc/201")
	_, err = d.ApproveGuest(ctx, "lxc/202", "uuid:202", nil, nil)
	require.ErrorIs(t, err, engine.ErrRefused, "the guest waits for a MAC it was not approved with")
	_, err = d.ApproveGuest(ctx, "lxc/202", "uuid:999", []string{"bc:24:11:00:02:02"}, nil)
	require.ErrorIs(t, err, engine.ErrRefused, "the guest has another identity than it was shown in")
	_, err = d.ApproveGuest(ctx, "lxc/202", "uuid:202", []string{"bc:24:11:00:02:02"}, []netip.Addr{netip.MustParseAddr("10.0.0.1")})
	require.NoError(t, err)
	require.NoError(t, d.RevokeGuest(ctx, "qemu/101"))
	require.ErrorIs(t, d.RevokeGuest(ctx, "qemu/101"), engine.ErrNotFound)

	require.NoError(t, d.AcknowledgeSegment(ctx, "vmbr1", 20))
	segs, err := d.Segments(ctx)
	require.NoError(t, err)
	require.True(t, segs[1].Acknowledged)
	require.NoError(t, d.RevokeSegment(ctx, "vmbr1", 20))
	require.ErrorIs(t, d.RevokeSegment(ctx, "vmbr9", 0), engine.ErrNotFound)

	require.NoError(t, d.Sync(ctx))
	before := e.Boot()
	require.NoError(t, d.Restart(ctx))
	require.NotEqual(t, before, e.Boot())
}

func unapproved(st engine.State) []string {
	var out []string
	for _, u := range st.Unapproved {
		out = append(out, u.String())
	}
	return out
}

func TestSettingsAndManualRoutesAreWrittenAtTheirRevision(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()

	view, err := d.Settings(ctx)
	require.NoError(t, err)
	raw, err := json.Marshal(view.Settings)
	require.NoError(t, err)
	saved, err := d.SaveSettings(ctx, view.Rev, bytes.Replace(raw, []byte(`"pollInterval":"10s"`), []byte(`"pollInterval":"20s"`), 1))
	require.NoError(t, err)
	var answer api.SavedSettings
	require.NoError(t, json.Unmarshal(saved, &answer))
	require.Equal(t, 2, answer.Rev)
	require.Empty(t, answer.RestartNeeded)
	require.Equal(t, 20*time.Second, e.PollInterval())

	_, err = d.SaveSettings(ctx, view.Rev, raw)
	require.ErrorIs(t, err, engine.ErrRefused, "the settings were read at a revision they no longer have")
	res := d.call(t, http.MethodPut, "/v1/settings",
		fmt.Sprintf(`{"rev": 2, "settings": %s}`, bytes.Replace(raw, []byte(`"pollInterval":"10s"`), []byte(`"pollInterval":"1s"`), 1)))
	require.Equal(t, http.StatusBadRequest, res.status)
	require.Contains(t, string(res.body), `"field":"pollInterval"`)
	res = d.call(t, http.MethodPut, "/v1/settings",
		fmt.Sprintf(`{"rev": 2, "settings": %s}`, bytes.Replace(raw, []byte(`"gateTag":"cf-tunnel"`), []byte(`"gateTag":"pco"`), 1)))
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	require.NoError(t, json.Unmarshal(res.body, &answer))
	require.Equal(t, []string{"gateTag"}, answer.RestartNeeded)

	docs := engine.ManualRouteView{ID: "docs", Hostname: "Docs.Example.com", Target: engine.ManualTarget{
		Kind: engine.TargetAddress, Scheme: "http", Addr: netip.MustParseAddr("10.0.5.21"), Port: 8080,
	}}
	made, err := d.CreateManualRoute(ctx, docs)
	require.NoError(t, err)
	require.Equal(t, 1, made.Rev)
	require.Equal(t, "docs.example.com", made.Hostname)
	_, err = d.CreateManualRoute(ctx, docs)
	require.ErrorIs(t, err, engine.ErrRefused, "the id is taken")
	outside := docs
	outside.ID, outside.Target.Addr = "far", netip.MustParseAddr("192.0.2.1")
	res = d.call(t, http.MethodPost, "/v1/routes/manual", string(must(json.Marshal(outside))))
	require.Equal(t, http.StatusBadRequest, res.status)
	require.Contains(t, string(res.body), `"field":"target.addr"`)

	made.Target.Port = 8081
	res = d.call(t, http.MethodPut, "/v1/routes/manual/docs", string(must(json.Marshal(made))))
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	require.ErrorIs(t, d.DeleteManualRoute(ctx, "docs", 1), engine.ErrRefused)
	require.NoError(t, d.DeleteManualRoute(ctx, "docs", 2))
	require.ErrorIs(t, d.DeleteManualRoute(ctx, "docs", 2), engine.ErrNotFound)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestAControlRefusesTheNextCall(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()

	status, _ := d.steer(t, http.MethodPost, "/refuse", `{"method": "Apply", "code": "refused", "message": "not now"}`)
	require.Equal(t, http.StatusNoContent, status)
	_, err := d.Apply(ctx, false, "")
	require.ErrorIs(t, err, engine.ErrRefused)
	require.EqualError(t, err, "not now")
	_, err = d.Apply(ctx, false, "")
	require.NoError(t, err, "a refusal is used up")

	status, _ = d.steer(t, http.MethodPost, "/refuse", `{"method": "SaveSettings", "code": "invalid", "message": "grace: too short", "field": "grace"}`)
	require.Equal(t, http.StatusNoContent, status)
	res := d.call(t, http.MethodPut, "/v1/settings", `{"rev": 1, "settings": {}}`)
	require.Equal(t, http.StatusBadRequest, res.status)
	require.JSONEq(t, `{"error": "grace: too short", "code": "invalid", "field": "grace"}`, string(res.body))

	for code, want := range map[string]int{"not_found": 404, "unavailable": 503, "internal": 500} {
		status, _ = d.steer(t, http.MethodPost, "/refuse", fmt.Sprintf(`{"method": "Guests", "code": %q, "message": "no"}`, code))
		require.Equal(t, http.StatusNoContent, status)
		res = d.call(t, http.MethodGet, "/v1/guests", "")
		require.Equal(t, want, res.status, code)
	}

	status, _ = d.steer(t, http.MethodPost, "/refuse", `{"method": "Subscribe", "code": "unavailable", "message": "16 streams are open already"}`)
	require.Equal(t, http.StatusNoContent, status)
	_, _, err = d.Stream(ctx, "", 0)
	require.ErrorIs(t, err, apiclient.ErrNoAnswer)
	require.ErrorContains(t, err, "16 streams are open already")

	for _, body := range []string{
		`{"method": "State", "code": "refused", "message": "no"}`,
		`{"method": "Nothing", "code": "refused", "message": "no"}`,
		`{"method": "Apply", "code": "teapot", "message": "no"}`,
		`{"method": "Apply", "code": "refused", "message": "no", "field": "offer"}`,
		`{"method": "Apply", "code": "refused"}`,
	} {
		status, _ = d.steer(t, http.MethodPost, "/refuse", body)
		require.Equal(t, http.StatusBadRequest, status, body)
	}
}

func TestEveryCallIsRecorded(t *testing.T) {
	e, _ := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()
	offer := e.State().Offer

	_, err := e.Apply(engine.WithActor(ctx, "alice@pve (ticket)"), true, offer)
	require.NoError(t, err)
	_, err = d.AddCredential(ctx, "secret", "0123456789abcdefghij0123456789")
	require.NoError(t, err)
	_, err = d.Status(ctx)
	require.NoError(t, err)

	status, body := d.steer(t, http.MethodGet, "/calls?method=Apply&method=AddCredential", "")
	require.Equal(t, http.StatusOK, status)
	var calls []Call
	require.NoError(t, json.Unmarshal(body, &calls))
	require.Len(t, calls, 2)
	require.Equal(t, "Apply", calls[0].Method)
	require.JSONEq(t, fmt.Sprintf(`{"confirmDeletes": true, "offer": %q}`, offer), string(calls[0].Args))
	require.Equal(t, "alice@pve (ticket)", calls[0].Actor)
	require.Equal(t, t0, calls[0].At)
	require.JSONEq(t, `{"label": "secret", "tokenLength": 30}`, string(calls[1].Args), "no token is kept")

	var methods []string
	for _, c := range e.Calls() {
		methods = append(methods, c.Method)
	}
	require.Contains(t, methods, "State")
	require.NotContains(t, methods, "Boot", "the API asks for the boot in every answer")

	status, _ = d.steer(t, http.MethodDelete, "/calls", "")
	require.Equal(t, http.StatusNoContent, status)
	require.Empty(t, e.Calls())
}

func TestTheStreamFollowsTheControls(t *testing.T) {
	e, c := load(t, "populated")
	d := serve(t, e)
	ctx := context.Background()

	notices, hello, err := d.Stream(ctx, "", 0)
	require.NoError(t, err)
	require.Equal(t, engine.Hello{Boot: e.Boot(), Version: "1.2.3", Seq: 10, Digest: e.State().Digest, PollInterval: "10s"}, hello)
	require.Equal(t, 1, d.subscribers(t))

	status, body := d.steer(t, http.MethodPost, "/event", `{"level": "info", "kind": "admin", "subject": "x", "message": "one"}`)
	require.Equal(t, http.StatusOK, status, string(body))
	n := next(t, notices)
	require.Equal(t, engine.NoticeEvent, n.Kind)
	require.Equal(t, uint64(11), n.Event.Seq)
	require.Equal(t, t0, n.Event.At)

	c.add(10 * time.Second)
	e.Cycle()
	n = next(t, notices)
	require.Equal(t, engine.NoticeState, n.Kind)
	require.Equal(t, t0.Add(10*time.Second), n.State.FinishedAt)
	require.Equal(t, e.State().Digest, n.State.Digest, "a cycle over the same state has the same digest")

	status, _ = d.steer(t, http.MethodPost, "/streams/pause", "")
	require.Equal(t, http.StatusNoContent, status)
	e.Cycle()
	e.Sample()
	d.steer(t, http.MethodPost, "/event", `[{"level": "warn", "kind": "problem", "message": "two"}]`)
	nothing(t, notices)
	d.steer(t, http.MethodPost, "/streams/resume", "")
	n = next(t, notices)
	require.Equal(t, "two", n.Event.Message, "what happened meanwhile comes after the pause, and no cycle did")
	last := n.Event.Seq

	d.steer(t, http.MethodPost, "/streams/drop", "")
	ended(t, notices)
	require.Eventually(t, func() bool { return d.subscribers(t) == 0 }, 5*time.Second, 10*time.Millisecond)

	notices, hello, err = d.Stream(ctx, hello.Boot, last-1)
	require.NoError(t, err)
	require.Equal(t, e.Boot(), hello.Boot, "a dropped stream keeps the boot")
	require.Equal(t, last, next(t, notices).Event.Seq, "the client resumes where it was")

	old := e.Boot()
	d.steer(t, http.MethodPost, "/boot", "")
	ended(t, notices)
	notices, hello, err = d.Stream(ctx, old, last)
	require.NoError(t, err)
	require.NotEqual(t, old, hello.Boot)
	require.Zero(t, hello.Seq)
	require.Equal(t, engine.Notice{Kind: engine.NoticeReset, Reason: "boot changed"}, next(t, notices))
	res := d.call(t, http.MethodGet, "/v1/version", "")
	require.Equal(t, hello.Boot, res.header.Get("Pco-Boot"))

	status, body = d.steer(t, http.MethodPost, "/traffic",
		`{"tunnels": [{"tunnelId": "00000000-0000-4000-8000-000000000001", "rps": 500, "haConnections": 4}],
		  "routes": [{"hostname": "www.example.com", "flowsPerSec": 9}]}`)
	require.Equal(t, http.StatusNoContent, status, string(body))
	n = next(t, notices)
	require.Equal(t, engine.NoticeTraffic, n.Kind)
	require.Equal(t, 500.0, n.Traffic.Tunnels[0].RPS)
	require.Equal(t, "www.example.com", n.Traffic.Routes[0].Hostname, "the busiest route comes first")

	st := e.State()
	st.Problems = append(st.Problems, "a new problem")
	status, body = d.steer(t, http.MethodPost, "/state", string(must(json.Marshal(st))))
	require.Equal(t, http.StatusOK, status)
	n = next(t, notices)
	require.Equal(t, engine.NoticeState, n.Kind)
	require.NotEqual(t, st.Digest, n.State.Digest)
	require.Contains(t, string(body), n.State.Digest)
}

func TestABatchOfRouteEventsIsCoalescedAsAtTheDaemon(t *testing.T) {
	e, _ := load(t, "empty")
	d := serve(t, e)
	notices, _, err := d.Stream(context.Background(), "", 0)
	require.NoError(t, err)

	batch := make([]engine.Event, 40)
	for i := range batch {
		batch[i] = engine.Event{Level: "info", Kind: "route", Message: fmt.Sprintf("route %d", i)}
	}
	batch[35].Level = "warn"
	batch = append(batch, engine.Event{Level: "warn", Kind: "problem", Message: "stays"})
	e.AddEvents(batch...)
	for i := range 32 {
		require.Equal(t, uint64(i+1), next(t, notices).Event.Seq)
	}
	require.Equal(t, engine.GapNotice{Boot: e.Boot(), From: 33, To: 40, Count: 8, Level: "warn"}, *next(t, notices).Gap)
	require.Equal(t, "stays", next(t, notices).Event.Message)
}

func TestTheSeventeenthStreamIsRefused(t *testing.T) {
	e, _ := load(t, "empty")
	d := serve(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range maxSubscribers {
		_, _, err := d.Stream(ctx, "", 0)
		require.NoError(t, err)
	}
	_, _, err := d.Stream(ctx, "", 0)
	require.ErrorIs(t, err, apiclient.ErrNoAnswer)
	require.ErrorContains(t, err, "16 streams are open already")
	cancel()
	require.Eventually(t, func() bool { return d.subscribers(t) == 0 }, 5*time.Second, 10*time.Millisecond)
}

func TestNoCycleBeforeTheFirst(t *testing.T) {
	e, _ := load(t, "first-run")
	d := serve(t, e)
	notices, hello, err := d.Stream(context.Background(), "", 0)
	require.NoError(t, err)
	require.Zero(t, hello.Seq)
	e.Cycle()
	nothing(t, notices)
	require.True(t, e.State().At.IsZero())

	cred, err := d.AddCredential(context.Background(), "main", strings.Repeat("k", 40))
	require.NoError(t, err)
	require.Equal(t, "cred1", cred.ID)
	require.True(t, cred.Report.Usable, "the credential gets the report of the scenario")
	require.Equal(t, t0, cred.Report.CheckedAt)
}

func TestSamplesKeepFifteenMinutes(t *testing.T) {
	e, c := load(t, "populated")
	for range keepSamples + 10 {
		c.add(engine.TrafficInterval)
		e.Sample()
	}
	tv := e.Traffic()
	require.Len(t, tv.Tunnels[0].Samples, keepSamples)
	require.Equal(t, c.now(), tv.Tunnels[0].Samples[keepSamples-1].At)
	series, err := e.RouteSeries("www.example.com")
	require.NoError(t, err)
	require.Len(t, series.Samples, keepSamples)

	why := whyOff
	require.NoError(t, e.ChangeTraffic(TrafficChange{RoutesWhy: &why}))
	tv = e.Traffic()
	require.Empty(t, tv.Routes)
	require.Equal(t, whyOff, tv.RoutesWhy)
	_, err = e.RouteSeries("www.example.com")
	require.ErrorIs(t, err, engine.ErrNotFound)
	require.Error(t, e.ChangeTraffic(TrafficChange{Tunnels: []engine.TunnelNotice{{TunnelID: "nope"}}}))
}

func TestTheStateFollowsTheSettings(t *testing.T) {
	e, _ := load(t, "first-run")
	ctx := context.Background()
	_, err := e.RotateTunnel(ctx, "")
	require.ErrorIs(t, err, engine.ErrRefused, "an install in observe-only mode changes nothing")
	res, err := e.Apply(ctx, false, "")
	require.NoError(t, err)
	require.True(t, res.LeftObserveOnly)
	require.Equal(t, engine.ModeEnforce, e.State().Mode)
	view, err := e.SettingsView()
	require.NoError(t, err)
	require.False(t, view.Settings.ObserveOnly)
	same, _, err := e.SaveSettings(ctx, view.Rev, view.Settings)
	require.NoError(t, err)
	require.Equal(t, view.Rev, same.Rev, "settings that did not change keep their revision")

	view.Settings.ObserveOnly = true
	back, _, err := e.SaveSettings(ctx, view.Rev, view.Settings)
	require.NoError(t, err, "observe-only is a setting to go back to")
	require.Equal(t, engine.ModeObserve, e.State().Mode)
	view.Settings.ObserveOnly = false
	_, _, err = e.SaveSettings(ctx, back.Rev, view.Settings)
	var fe *engine.FieldError
	require.True(t, errors.As(err, &fe))
	require.Equal(t, "observeOnly", fe.Field)
}

func TestAnAnnotationIsTheBlockOfTheNotes(t *testing.T) {
	e, _ := load(t, "populated")
	v, err := e.Annotation(model.GuestRef{Kind: model.KindLXC, VMID: 210})
	require.NoError(t, err)
	require.Equal(t, "```cf-tunnel\n*.store.example.com -> :8080\nstore.example.com -> http://:80\nwww.store.example.com -> http://:80\n```", v.Block)
	require.Empty(t, v.Issues)
	v, err = e.Annotation(model.GuestRef{Kind: model.KindQEMU, VMID: 103})
	require.NoError(t, err)
	require.Equal(t, []planner.Issue{e.State().Issues[0]}, v.Issues)
}
