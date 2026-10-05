package auth

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/pvefake"
)

const (
	aliceTicket = "PVE:alice@pve:66F1E2D3::YWxpY2Utc2lnbmF0dXJl"
	bobTicket   = "PVE:bob@pve:66F1E2D4::Ym9iLXNpZ25hdHVyZQ=="
	carolTicket = "PVE:carol@pve:66F1E2D5::Y2Fyb2w="
	tokenSecret = "0b5c3e7e-1d2f-4a6b-9c8d-7e6f5a4b3c2d"
	aliceToken  = "alice@pve!pco=" + tokenSecret
	aliceSep    = "alice@pve!sep=" + tokenSecret
)

// testUsers are alice, an admin with two tokens, bob, a reader, and carol,
// who may see nothing of the cluster.
func testUsers() pvefake.Users {
	return pvefake.Users{Users: []pvefake.User{
		{
			ID:         "alice@pve",
			Privileges: map[string][]string{"/": {"Sys.Audit", "Sys.Modify", "VM.Audit"}},
			Guests:     []string{"qemu/101", "qemu/102", "lxc/200"},
			Tickets:    []string{aliceTicket},
			Tokens: []pvefake.Token{
				{ID: "pco", Secret: tokenSecret},
				{ID: "sep", Secret: tokenSecret, Privsep: true, Privileges: map[string][]string{"/": {"Sys.Audit"}}, Guests: []string{"lxc/200"}},
			},
		},
		{
			ID:         "bob@pve",
			Privileges: map[string][]string{"/": {"Sys.Audit"}},
			Guests:     []string{"qemu/102", "lxc/200"},
			Tickets:    []string{bobTicket},
		},
		{ID: "carol@pve", Privileges: map[string][]string{"/": {"VM.Audit"}}, Tickets: []string{carolTicket}},
	}}
}

// fakePVE serves the fake over TLS and returns it with the certificate it
// presents.
func fakePVE(t *testing.T, users pvefake.Users) (*pvefake.Fake, *httptest.Server) {
	t.Helper()
	f, err := pvefake.New("pve1", users)
	require.NoError(t, err)
	return f, tlsServer(t, f)
}

// tlsServer serves h over TLS, without logging the handshakes the tests make
// fail on purpose.
func tlsServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func apiURL(srv *httptest.Server) string { return srv.URL + "/api2/json" }

func otherCertificate(t *testing.T) *x509.Certificate {
	t.Helper()
	_, certPEM, err := pvefake.SelfSigned(time.Now(), "127.0.0.1")
	require.NoError(t, err)
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func TestPrivileges(t *testing.T) {
	_, srv := fakePVE(t, testUsers())
	p := NewPVE(apiURL(srv), srv.Certificate(), 5*time.Second)
	ctx := context.Background()

	cases := []struct {
		name string
		cred Credential
		want map[string]bool
	}{
		{"an admin's ticket", Credential{Ticket: aliceTicket}, map[string]bool{"Sys.Audit": true, "Sys.Modify": true, "VM.Audit": true}},
		{"a reader's ticket", Credential{Ticket: bobTicket}, map[string]bool{"Sys.Audit": true}},
		{"a token", Credential{Token: aliceToken}, map[string]bool{"Sys.Audit": true, "Sys.Modify": true, "VM.Audit": true}},
		{"a privilege-separated token", Credential{Token: aliceSep}, map[string]bool{"Sys.Audit": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := p.Privileges(ctx, c.cred, "/")
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}

	vmids, err := p.VisibleVMIDs(ctx, Credential{Ticket: bobTicket})
	require.NoError(t, err)
	require.Equal(t, []int{102, 200}, vmids)
	vmids, err = p.VisibleVMIDs(ctx, Credential{Token: aliceSep})
	require.NoError(t, err)
	require.Equal(t, []int{200}, vmids)
	vmids, err = p.VisibleVMIDs(ctx, Credential{Ticket: carolTicket})
	require.NoError(t, err)
	require.Empty(t, vmids)
}

func TestRefusedAndUnreachable(t *testing.T) {
	_, srv := fakePVE(t, testUsers())
	ctx := context.Background()
	pinned := func(s *httptest.Server) PVE { return NewPVE(apiURL(s), s.Certificate(), 5*time.Second) }
	answering := func(status int) PVE {
		return pinned(tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status == http.StatusFound {
				http.Redirect(w, r, "https://elsewhere.example/", status)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"data":{"/":{"Sys.Modify":1}}}`))
		})))
	}
	down := httptest.NewTLSServer(http.NotFoundHandler())
	down.Close()
	slow := tlsServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))

	cases := []struct {
		name string
		pve  PVE
		cred Credential
		want error
	}{
		{"an expired ticket", pinned(srv), Credential{Ticket: "PVE:alice@pve:1::ZXhwaXJlZA=="}, ErrRefused},
		{"a revoked token", pinned(srv), Credential{Token: "alice@pve!gone=" + tokenSecret}, ErrRefused},
		{"Proxmox VE down", pinned(down), Credential{Ticket: aliceTicket}, ErrUnreachable},
		{"another certificate than the pin", NewPVE(apiURL(srv), otherCertificate(t), 5*time.Second), Credential{Ticket: aliceTicket}, ErrUnreachable},
		{"no pin", NewPVE(apiURL(srv), nil, 5*time.Second), Credential{Ticket: aliceTicket}, ErrUnreachable},
		{"a 403", answering(403), Credential{Ticket: aliceTicket}, ErrUnreachable},
		{"a 500", answering(500), Credential{Ticket: aliceTicket}, ErrUnreachable},
		{"a redirect", answering(302), Credential{Ticket: aliceTicket}, ErrUnreachable},
		{"no answer in time", NewPVE(apiURL(slow), slow.Certificate(), 50*time.Millisecond), Credential{Ticket: aliceTicket}, ErrUnreachable},
		{"a base URL of plain HTTP", NewPVE("http://127.0.0.1:8006/api2/json", srv.Certificate(), 5*time.Second), Credential{Ticket: aliceTicket}, ErrUnreachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.pve.Privileges(ctx, c.cred, "/")
			require.ErrorIs(t, err, c.want)
			require.NotContains(t, err.Error(), "YWxpY2U", "an error never repeats the ticket")
			require.NotContains(t, err.Error(), tokenSecret, "an error never repeats the token")
			_, err = c.pve.VisibleVMIDs(ctx, c.cred)
			require.ErrorIs(t, err, c.want)
		})
	}
}

func TestTheCredentialSent(t *testing.T) {
	var (
		mu  sync.Mutex
		got []*http.Request
	)
	srv := tlsServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Clone(context.Background()))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":{"/":{}}}`))
	}))
	p := NewPVE(apiURL(srv), srv.Certificate(), 5*time.Second)

	_, err := p.Privileges(context.Background(), Credential{Ticket: "PVE%3Aalice@pve%3A66F1E2D3%3A%3AYWxp"}, "/")
	require.NoError(t, err)
	_, err = p.Privileges(context.Background(), Credential{Token: aliceToken}, "/")
	require.NoError(t, err)
	for _, c := range []Credential{{}, {Ticket: aliceTicket, Token: aliceToken}} {
		_, err = p.Privileges(context.Background(), c, "/")
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrRefused)
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 2, "a credential with no field or both is never sent")
	require.Equal(t, "/api2/json/access/permissions?path=%2F", got[0].URL.RequestURI())
	require.Equal(t, []string{"PVEAuthCookie=PVE%3Aalice@pve%3A66F1E2D3%3A%3AYWxp"}, got[0].Header.Values("Cookie"), "the cookie goes as the browser had it")
	require.Empty(t, got[0].Header.Values("Authorization"))
	require.Equal(t, []string{"PVEAPIToken=" + aliceToken}, got[1].Header.Values("Authorization"))
	require.Empty(t, got[1].Header.Values("Cookie"))
}

func TestTicketUser(t *testing.T) {
	cases := []struct {
		ticket, user string
		ok           bool
	}{
		{aliceTicket, "alice@pve", true},
		{"PVE%3Abob@ad%3A66F1E2D4%3A%3AYm9i", "bob@ad", true},
		{"PVE:root@pam:66F1E2D4::sig", "root@pam", true},
		{"PVE::66F1E2D4::sig", "", false},
		{"PMG:alice@pmg:66F1E2D4::sig", "", false},
		{"alice@pve", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		user, ok := ticketUser(c.ticket)
		require.Equal(t, c.ok, ok, c.ticket)
		require.Equal(t, c.user, user, c.ticket)
	}
}

func TestRoleOf(t *testing.T) {
	require.Equal(t, RoleAdmin, roleOf(map[string]bool{"Sys.Modify": true}))
	require.Equal(t, RoleAdmin, roleOf(map[string]bool{"Sys.Audit": true, "Sys.Modify": true}))
	require.Equal(t, RoleReader, roleOf(map[string]bool{"Sys.Audit": true, "VM.Audit": true}))
	require.Equal(t, RoleNone, roleOf(map[string]bool{"VM.Audit": true}))
	require.Equal(t, RoleNone, roleOf(nil))
	require.Equal(t, "admin", RoleAdmin.String())
	require.Equal(t, "reader", RoleReader.String())
	require.Equal(t, "none", RoleNone.String())
}

func TestTheTicketCheckIsCached(t *testing.T) {
	f, srv := fakePVE(t, testUsers())
	clock := newClock()
	checks := newTicketChecks(NewPVE(apiURL(srv), srv.Certificate(), 5*time.Second), clock.Now)
	ctx := context.Background()

	got, err := checks.check(ctx, aliceTicket)
	require.NoError(t, err)
	require.Equal(t, ticketCheck{user: "alice@pve", role: RoleAdmin, at: t0}, got)
	clock.Add(29 * time.Second)
	_, err = checks.check(ctx, aliceTicket)
	require.NoError(t, err)
	require.Equal(t, 1, f.Calls("access/permissions"), "a check is good for 30 s")

	clock.Add(time.Second)
	f.SetPrivileges("alice@pve", "/", "Sys.Audit")
	got, err = checks.check(ctx, aliceTicket)
	require.NoError(t, err)
	require.Equal(t, RoleReader, got.role)
	require.Equal(t, 2, f.Calls("access/permissions"))

	_, err = checks.check(ctx, "PVE:alice@pve:1::Zm9yZ2Vk")
	require.ErrorIs(t, err, ErrRefused)
	_, err = checks.check(ctx, "PVE:alice@pve:1::Zm9yZ2Vk")
	require.ErrorIs(t, err, ErrRefused)
	require.Equal(t, 4, f.Calls("access/permissions"), "a refusal is not cached")
	for h := range checks.byHash {
		require.NotContains(t, string(h[:]), "alice", "the cache holds a hash, not the ticket")
	}
}

func TestTheTicketCacheIsBounded(t *testing.T) {
	clock := newClock()
	checks := newTicketChecks(staticPVE{privs: map[string]bool{"Sys.Audit": true}}, clock.Now)
	for i := range maxTicketChecks + 10 {
		_, err := checks.check(context.Background(), "PVE:u@pve:"+strconv.Itoa(i)+"::s")
		require.NoError(t, err)
	}
	require.LessOrEqual(t, len(checks.byHash), maxTicketChecks)
}

// staticPVE answers every credential alike.
type staticPVE struct {
	privs map[string]bool
	vmids []int
	err   error
}

func (s staticPVE) Privileges(context.Context, Credential, string) (map[string]bool, error) {
	return s.privs, s.err
}

func (s staticPVE) VisibleVMIDs(context.Context, Credential) ([]int, error) { return s.vmids, s.err }
