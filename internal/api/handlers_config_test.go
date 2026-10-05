package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// testSettingsView is the settings of revision 7 as a daemon shows them.
func testSettingsView() engine.SettingsView {
	s := store.DefaultSettings()
	s.ObserveOnly = false
	limits := map[string]engine.Limit{}
	for name, l := range store.Limits() {
		limits[name] = engine.Limit{Min: l.Min, Max: l.Max}
	}
	return engine.SettingsView{
		Rev: 7, Settings: s, Limits: limits, Notes: []string{},
		ReadAtStart: []string{"cloudflareBudget", "gateTag", "trustStatic", "trustedCIDRs"},
	}
}

func TestTheSettingsAreShownWithTheirRevision(t *testing.T) {
	rec := do(newServer(&fakeEngine{settings: testSettingsView()}), http.MethodGet, "/v1/settings", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "settings.json", rec.Body.Bytes())
}

func TestTheSettingsAreSavedWithTheRevisionTheyWereReadAt(t *testing.T) {
	f := &fakeEngine{settings: testSettingsView(), restart: []string{"gateTag"}}
	body := `{"rev":7,"settings":{"gateTag":"publish","pollInterval":"10s","grace":"1m0s","admission":"tag",` +
		`"observeOnly":false,"identityMinimum":"port","maxHostnamesPerGuest":32,"reverifyInterval":"1m0s","cloudflareBudget":1000}}`

	rec := do(newServer(f), http.MethodPut, "/v1/settings", body)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"save settings:7:publish"}, f.called())
	var got SavedSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, 8, got.Rev)
	require.Equal(t, "publish", got.Settings.GateTag)
	require.Equal(t, []string{"gateTag"}, got.RestartNeeded)
	require.Contains(t, rec.Body.String(), `"readAtStart":[`)
}

func TestASaveOfTheSettingsIsRefusedAsTheEngineSays(t *testing.T) {
	valid := `"settings":{"gateTag":"cf-tunnel","pollInterval":"10s","grace":"1m0s","admission":"tag","observeOnly":true,` +
		`"identityMinimum":"port","maxHostnamesPerGuest":32,"reverifyInterval":"1m0s","cloudflareBudget":1000}`
	for _, tt := range []struct {
		name, body  string
		err         error
		status      int
		code, field string
	}{
		{"no revision", `{` + valid + `}`, nil, http.StatusBadRequest, "invalid", "rev"},
		{"no settings", `{"rev":7}`, nil, http.StatusBadRequest, "invalid", "settings"},
		{"a key the settings do not have", `{"rev":7,"settings":{"denyhost":["a.example.com"]}}`, nil, http.StatusBadRequest, "invalid", ""},
		{"a stale revision", `{"rev":6,` + valid + `}`, fmt.Errorf("%w: the settings changed since they were read at revision 6", engine.ErrRefused),
			http.StatusConflict, "refused", ""},
		{"a setting out of its range", `{"rev":7,` + valid + `}`, &engine.FieldError{Field: "denyHosts[2]", Err: errors.New(`denyHosts[2] "a..b": not a pattern`)},
			http.StatusBadRequest, "invalid", "denyHosts[2]"},
		{"a cycle that runs too long", `{"rev":7,` + valid + `}`, engine.ErrBusy, http.StatusServiceUnavailable, "unavailable", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{settings: testSettingsView(), err: tt.err}

			rec := do(newServer(f), http.MethodPut, "/v1/settings", tt.body)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			body := parseError(t, rec)
			require.Equal(t, tt.code, body.Code)
			require.Equal(t, tt.field, body.Field)
		})
	}
}

func TestAPutCarriesJSON(t *testing.T) {
	req := request(http.MethodPut, "/v1/settings", `{"rev":7}`)
	req.Header.Del("Content-Type")

	rec := send(newServer(&fakeEngine{}), req)

	require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
}

func testManualRoute() engine.ManualRouteView {
	return engine.ManualRouteView{
		ID: "status", Rev: 2, Hostname: "status.example.com",
		Target: engine.ManualTarget{Kind: "address", Scheme: "http", Addr: netip.MustParseAddr("10.0.5.20"), Port: 9000},
	}
}

func TestTheManualRoutes(t *testing.T) {
	f := &fakeEngine{manual: []engine.ManualRouteView{testManualRoute()}}
	rec := do(newServer(f), http.MethodGet, "/v1/routes/manual", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "manual_routes.json", rec.Body.Bytes())

	rec = do(newServer(&fakeEngine{}), http.MethodGet, "/v1/routes/manual", "")
	require.JSONEq(t, `[]`, rec.Body.String())
}

func TestAManualRouteIsMade(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/routes/manual",
		`{"hostname":"status.example.com","target":{"kind":"address","scheme":"http","addr":"10.0.5.20","port":9000}}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, []string{"create manual::status.example.com"}, f.called())
	require.Contains(t, rec.Body.String(), `"rev":1`)

	rec = do(newServer(f), http.MethodPost, "/v1/routes/manual", `{"id":"status","rev":3,"hostname":"status.example.com"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "rev", parseError(t, rec).Field, "a new route has no revision")

	f = &fakeEngine{err: &engine.FieldError{Field: "target.addr", Err: errors.New("target.addr 192.168.1.1: not inside the trusted prefixes")}}
	rec = do(newServer(f), http.MethodPost, "/v1/routes/manual", `{"hostname":"status.example.com"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, ErrorBody{Error: "target.addr 192.168.1.1: not inside the trusted prefixes", Code: "invalid", Field: "target.addr"},
		errorBody(t, rec))
}

func TestAManualRouteIsChangedAtItsRevision(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPut, "/v1/routes/manual/status",
		`{"rev":2,"hostname":"status.example.com","target":{"kind":"address","scheme":"http","addr":"10.0.5.20","port":9001}}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"update manual:status:2:status.example.com"}, f.called())

	f = &fakeEngine{err: fmt.Errorf("%w: the manual route manual/status changed since it was read at revision 2", engine.ErrRefused)}
	rec = do(newServer(f), http.MethodPut, "/v1/routes/manual/status", `{"rev":2,"hostname":"status.example.com"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "refused", errorCode(t, rec))
}

func TestAManualRouteIsRemovedAtItsRevision(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodDelete, "/v1/routes/manual/status?rev=2", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"delete manual:status:2"}, f.called())

	for _, query := range []string{"", "?rev=", "?rev=two", "?rev=-1", "?rev=0", "?rev=1&rev=2"} {
		t.Run("rev "+query, func(t *testing.T) {
			f := &fakeEngine{}
			rec := do(newServer(f), http.MethodDelete, "/v1/routes/manual/status"+query, "")

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "rev", parseError(t, rec).Field)
			require.Empty(t, f.called())
		})
	}
}

func TestTheGuests(t *testing.T) {
	f := &fakeEngine{guests: []engine.GuestListView{
		{Ref: "qemu/101", Name: "web-1", Node: "pve1", Running: true, Tagged: true, Identity: "uuid:4c4c4544-0042-3510-8051-b4c04f4e3432",
			Approval: "approved", Routes: 2},
		{Ref: "lxc/200", Node: "pve1", Approval: "not-needed"},
	}}
	rec := do(newServer(f), http.MethodGet, "/v1/guests", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "guests.json", rec.Body.Bytes())
}

func TestTheAnnotationOfAGuest(t *testing.T) {
	f := &fakeEngine{annotation: engine.AnnotationView{
		Ref: "qemu/103", Block: "```cf-tunnel\napp.example.com -> :3000\n```", StartLine: 4,
		Issues: []planner.Issue{{Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 103}, Line: 5, Col: 5, Msg: "a broken entry"}},
	}}
	rec := do(newServer(f), http.MethodGet, "/v1/guests/qemu/103/annotation", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"annotation:qemu/103"}, f.called())
	requireGolden(t, "annotation.json", rec.Body.Bytes())

	for _, path := range []string{"/v1/guests/vm/103/annotation", "/v1/guests/qemu/0103/annotation", "/v1/guests/qemu/x/annotation"} {
		rec := do(newServer(f), http.MethodGet, path, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, path)
		require.Equal(t, "invalid", errorCode(t, rec))
	}

	rec = do(newServer(&fakeEngine{err: fmt.Errorf("%w: qemu/104 is not in the last listing of Proxmox", engine.ErrNotFound)}),
		http.MethodGet, "/v1/guests/qemu/104/annotation", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "not_found", errorCode(t, rec))
}

func TestARestartIsAskedFor(t *testing.T) {
	f := &fakeEngine{}
	rec := do(newServer(f), http.MethodPost, "/v1/daemon/restart", "")

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, []string{"restart"}, f.called())
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var body ErrorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}
