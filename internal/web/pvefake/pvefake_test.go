package pvefake_test

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/pvefake"
)

const (
	aliceTicket = "PVE:alice@pve:66F1E2D3::YWxpY2Utc2lnbmF0dXJl"
	bobTicket   = "PVE:bob@pve:66F1E2D4::Ym9iLXNpZ25hdHVyZQ=="
	secret      = "0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d"
)

func users() pvefake.Users {
	return pvefake.Users{Users: []pvefake.User{
		{
			ID:         "alice@pve",
			Privileges: map[string][]string{"/": {"Sys.Audit", "Sys.Modify"}, "/vms/101": {"VM.Audit"}},
			Guests:     []string{"qemu/101", "lxc/200"},
			Tickets:    []string{aliceTicket},
			Tokens: []pvefake.Token{
				{ID: "full", Secret: secret},
				{ID: "sep", Secret: secret, Privsep: true, Privileges: map[string][]string{"/": {"Sys.Audit", "Sys.PowerMgmt"}}, Guests: []string{"lxc/200", "qemu/300"}},
			},
		},
		{ID: "bob@pve", Privileges: map[string][]string{"/": {"Sys.Audit"}}, Guests: []string{"qemu/102"}, Tickets: []string{bobTicket}},
		{ID: "carol@ad", Tickets: []string{"PVE:carol@ad:66F1E2D5::c2ln"}},
	}}
}

func newFake(t *testing.T) *pvefake.Fake {
	t.Helper()
	f, err := pvefake.New("pve1", users())
	require.NoError(t, err)
	return f
}

type credential func(*http.Request)

func ticket(value string) credential {
	return func(r *http.Request) { r.Header.Set("Cookie", "PVEAuthCookie="+value) }
}

func token(value string) credential {
	return func(r *http.Request) { r.Header.Set("Authorization", "PVEAPIToken="+value) }
}

func call(t *testing.T, f *pvefake.Fake, target string, c credential) (int, any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if c != nil {
		c(r)
	}
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, r)
	var body struct{ Data any }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body.Data
}

func TestPermissionsOfATicket(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		name   string
		target string
		cred   credential
		status int
		want   any
	}{
		{"at the root", "/api2/json/access/permissions?path=/", ticket(aliceTicket), 200,
			map[string]any{"/": map[string]any{"Sys.Audit": 1.0, "Sys.Modify": 1.0}}},
		{"below a listed path", "/api2/json/access/permissions?path=/vms/101/x", ticket(aliceTicket), 200,
			map[string]any{"/vms/101/x": map[string]any{"VM.Audit": 1.0}}},
		{"every path", "/api2/json/access/permissions", ticket(aliceTicket), 200, map[string]any{
			"/":        map[string]any{"Sys.Audit": 1.0, "Sys.Modify": 1.0},
			"/vms/101": map[string]any{"VM.Audit": 1.0},
		}},
		{"as the page escapes the cookie", "/api2/json/access/permissions?path=/", ticket("PVE%3Abob@pve%3A66F1E2D4%3A%3AYm9iLXNpZ25hdHVyZQ%3D%3D"), 200,
			map[string]any{"/": map[string]any{"Sys.Audit": 1.0}}},
		{"a user without privileges", "/api2/json/access/permissions?path=/", ticket("PVE:carol@ad:66F1E2D5::c2ln"), 200,
			map[string]any{"/": map[string]any{}}},
		{"a ticket not listed", "/api2/json/access/permissions?path=/", ticket("PVE:alice@pve:66F1E2D3::Zm9yZ2Vk"), 401, nil},
		{"a ticket of another form", "/api2/json/access/permissions?path=/", ticket("alice@pve"), 401, nil},
		{"no credential", "/api2/json/access/permissions?path=/", nil, 401, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, data := call(t, f, c.target, c.cred)
			require.Equal(t, c.status, status)
			require.Equal(t, c.want, data)
		})
	}
}

func TestTokens(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		name   string
		token  string
		status int
		privs  any
		guests []any
	}{
		{"without privilege separation", "alice@pve!full=" + secret, 200,
			map[string]any{"Sys.Audit": 1.0, "Sys.Modify": 1.0}, []any{101.0, 200.0}},
		{"privilege separated: what both have", "alice@pve!sep=" + secret, 200,
			map[string]any{"Sys.Audit": 1.0}, []any{200.0}},
		{"a wrong secret", "alice@pve!full=0b5c3e7e-1d2f-4a6b-9c8d-000000000000", 401, nil, nil},
		{"an unknown token", "alice@pve!other=" + secret, 401, nil, nil},
		{"no secret", "alice@pve!full", 401, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, data := call(t, f, "/api2/json/access/permissions?path=/", token(c.token))
			require.Equal(t, c.status, status)
			if c.status != 200 {
				return
			}
			require.Equal(t, map[string]any{"/": c.privs}, data)
			_, rows := call(t, f, "/api2/json/cluster/resources?type=vm", token(c.token))
			var vmids []any
			for _, row := range rows.([]any) {
				vmids = append(vmids, row.(map[string]any)["vmid"])
			}
			require.Equal(t, c.guests, vmids)
		})
	}
}

func TestResources(t *testing.T) {
	f := newFake(t)
	status, data := call(t, f, "/api2/json/cluster/resources?type=vm", ticket(aliceTicket))
	require.Equal(t, 200, status)
	require.Equal(t, []any{
		map[string]any{"id": "qemu/101", "type": "qemu", "vmid": 101.0, "name": "guest-101", "node": "pve1", "status": "running"},
		map[string]any{"id": "lxc/200", "type": "lxc", "vmid": 200.0, "name": "guest-200", "node": "pve1", "status": "running"},
	}, data)

	_, data = call(t, f, "/api2/json/cluster/resources", ticket(aliceTicket))
	require.Len(t, data, 2, "without a type every guest is listed")
	_, data = call(t, f, "/api2/json/cluster/resources?type=storage", ticket(aliceTicket))
	require.Empty(t, data)
	_, data = call(t, f, "/api2/json/cluster/resources?type=vm", ticket("PVE:carol@ad:66F1E2D5::c2ln"))
	require.Equal(t, []any{}, data)

	status, _ = call(t, f, "/api2/json/cluster/resources?type=vm", nil)
	require.Equal(t, 401, status)
}

func TestDomainsNeedNoCredential(t *testing.T) {
	status, data := call(t, newFake(t), "/api2/json/access/domains", nil)
	require.Equal(t, 200, status)
	require.Equal(t, []any{
		map[string]any{"realm": "pam", "type": "pam", "comment": "Linux PAM standard authentication"},
		map[string]any{"realm": "pve", "type": "pve", "comment": "Proxmox VE authentication server"},
		map[string]any{"realm": "ad", "type": "ldap"},
	}, data)
}

func TestOtherCalls(t *testing.T) {
	f := newFake(t)
	for _, target := range []string{"/api2/json/nodes", "/api2/json/access/ticket", "/other"} {
		status, _ := call(t, f, target, ticket(aliceTicket))
		require.Equal(t, http.StatusNotImplemented, status, target)
	}
	r := httptest.NewRequest(http.MethodPost, "/api2/json/access/permissions", nil)
	ticket(aliceTicket)(r)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, r)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
}

func TestControls(t *testing.T) {
	f := newFake(t)
	root := "/api2/json/access/permissions?path=/"

	f.SetPrivileges("alice@pve", "/", "Sys.Audit")
	_, data := call(t, f, root, ticket(aliceTicket))
	require.Equal(t, map[string]any{"/": map[string]any{"Sys.Audit": 1.0}}, data)

	renewed := "PVE:alice@pve:66F1E400::cmVuZXdlZA=="
	f.AddTicket("alice@pve", renewed)
	status, _ := call(t, f, root, ticket(renewed))
	require.Equal(t, 200, status)

	f.RemoveTicket(aliceTicket)
	status, _ = call(t, f, root, ticket(aliceTicket))
	require.Equal(t, 401, status)
	status, _ = call(t, f, root, ticket(renewed))
	require.Equal(t, 200, status, "only the ticket removed is refused")

	f.RemoveToken("alice@pve!full")
	status, _ = call(t, f, root, token("alice@pve!full="+secret))
	require.Equal(t, 401, status)
	status, _ = call(t, f, root, token("alice@pve!sep="+secret))
	require.Equal(t, 200, status)

	require.Equal(t, 6, f.Calls("access/permissions"))
	require.Zero(t, f.Calls("cluster/resources"))
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		return p
	}
	good, err := json.Marshal(users())
	require.NoError(t, err)
	f, err := pvefake.Load(write("users.json", string(good)), "pve1")
	require.NoError(t, err)
	status, _ := call(t, f, "/api2/json/access/permissions?path=/", ticket(bobTicket))
	require.Equal(t, 200, status)

	cases := []struct{ name, body, want string }{
		{"an unknown field", `{"users":[{"user":"a@pve","priv":{}}]}`, "unknown field"},
		{"a user without realm", `{"users":[{"user":"alice"}]}`, "not user@realm"},
		{"a ticket of another user", `{"users":[{"user":"a@pve","tickets":["PVE:b@pve:1::s"]}]}`, "is not PVE:a@pve"},
		{"a ticket without signature", `{"users":[{"user":"a@pve","tickets":["PVE:a@pve:1::"]}]}`, "is not PVE:a@pve"},
		{"a token without secret", `{"users":[{"user":"a@pve","tokens":[{"id":"t"}]}]}`, "needs an id and a secret"},
		{"a guest of another form", `{"users":[{"user":"a@pve","guests":["vm/1"]}]}`, "guest of a@pve"},
		{"a user twice", `{"users":[{"user":"a@pve"},{"user":"a@pve"}]}`, "listed twice"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := pvefake.Load(write(c.name+".json", c.body), "pve1")
			require.ErrorContains(t, err, c.want)
		})
	}
	_, err = pvefake.Load(filepath.Join(dir, "missing.json"), "pve1")
	require.Error(t, err)
}

func TestSelfSigned(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	pair, certPEM, err := pvefake.SelfSigned(now, "127.0.0.1", "localhost")
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.Equal(t, pair.Certificate[0], cert.Raw)
	require.Equal(t, []string{"localhost"}, cert.DNSNames)
	require.Equal(t, "127.0.0.1", cert.IPAddresses[0].String())
	require.True(t, cert.NotBefore.Before(now) && cert.NotAfter.After(now.Add(300*24*time.Hour)))

	_, other, err := pvefake.SelfSigned(now, "localhost")
	require.NoError(t, err)
	require.NotEqual(t, certPEM, other, "every certificate has a key of its own")

	_, _, err = pvefake.SelfSigned(now)
	require.Error(t, err)
}
