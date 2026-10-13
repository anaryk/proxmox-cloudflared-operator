package pve

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// labServer serves h over TLS and records what it was asked.
func labServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// answering is a node that answers every read with the version and every
// write with an empty answer.
func answering(t *testing.T) (*httptest.Server, *recorder) {
	t.Helper()
	return labServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, fixture(t, "version.json"))
			return
		}
		_, _ = io.WriteString(w, `{"data":null}`)
	})
}

// failing is a node whose pveproxy answers everything with status.
func failing(t *testing.T, status int) (*httptest.Server, *recorder) {
	t.Helper()
	return labServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"data":null,"message":"`+http.StatusText(status)+`"}`)
	})
}

// down is a node whose pveproxy is not listening.
func down(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	srv.Close()
	return srv
}

// lab is a Multi for the nodes pve1, pve2, ... of a lab, each served by one
// of the servers, verified under example.com, the name of the certificate
// the test servers present, and counts the connections made to each.
type lab struct {
	m     *Multi
	dials []atomic.Int32
}

func newLab(t *testing.T, cfg Config, servers ...*httptest.Server) *lab {
	t.Helper()
	cfg.BaseURL = cmp.Or(cfg.BaseURL, "https://pve1.lab.invalid:8006")
	cfg.CAFile = cmp.Or(cfg.CAFile, writeCA(t, servers[0].Certificate()))
	cfg.ServerName = cmp.Or(cfg.ServerName, "example.com")
	var eps []Endpoint
	for i := 1; i < len(servers); i++ {
		eps = append(eps, Endpoint{Address: fmt.Sprintf("pve%d.lab.invalid", i+1), ServerName: "example.com"})
	}
	m, err := NewMulti(cfg, eps)
	require.NoError(t, err)
	return wire(t, m, servers...)
}

// wire makes the targets of m, in order, connect to servers.
func wire(t *testing.T, m *Multi, servers ...*httptest.Server) *lab {
	t.Helper()
	require.Len(t, m.fb.targets, len(servers))
	l := &lab{m: m, dials: make([]atomic.Int32, len(servers))}
	for i, srv := range servers {
		addr := srv.Listener.Addr().String()
		tr := m.fb.targets[i].rt.(*http.Transport)
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			l.dials[i].Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
	}
	return l
}

func (l *lab) dialed(i int) int { return int(l.dials[i].Load()) }

func TestMultiTalksToTheFirstEndpointWhileItAnswers(t *testing.T) {
	a, recA := answering(t)
	b, recB := answering(t)
	l := newLab(t, testConfig(""), a, b)

	for range 3 {
		v, err := l.m.Version(t.Context())
		require.NoError(t, err)
		require.Equal(t, "9.1", v.Release)
	}
	require.Len(t, recA.requests(), 3)
	require.Empty(t, recB.requests())
	require.Equal(t, Endpoint{Address: "pve1.lab.invalid:8006", ServerName: "example.com"}, l.m.Current())
	require.Equal(t, []EndpointStatus{
		{Endpoint: Endpoint{Address: "pve1.lab.invalid:8006", ServerName: "example.com"}, Current: true},
		{Endpoint: Endpoint{Address: "pve2.lab.invalid", ServerName: "example.com"}},
	}, l.m.Status())
}

func TestMultiFallsBackWhenTheEndpointRefusesAndKeepsTheOneThatAnswered(t *testing.T) {
	b, recB := answering(t)
	l := newLab(t, testConfig(""), down(t), b)

	v, err := l.m.Version(t.Context())
	require.NoError(t, err)
	require.Equal(t, "9.1", v.Release)
	require.Equal(t, 1, l.dialed(0))
	require.Equal(t, "pve2.lab.invalid", l.m.Current().Address)

	_, err = l.m.Version(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, l.dialed(0), "the second request goes to the node that answered")
	require.Len(t, recB.requests(), 2)

	st := l.m.Status()
	require.False(t, st[0].Current)
	var op *net.OpError
	require.ErrorAs(t, st[0].Err, &op)
	require.True(t, st[1].Current)
	require.NoError(t, st[1].Err)
}

func TestMultiFallsBackOnAServerErrorOfARead(t *testing.T) {
	a, recA := failing(t, http.StatusServiceUnavailable)
	b, recB := answering(t)
	l := newLab(t, testConfig(""), a, b)

	v, err := l.m.Version(t.Context())
	require.NoError(t, err)
	require.Equal(t, "9.1", v.Release)
	require.Len(t, recA.requests(), 1)
	require.Len(t, recB.requests(), 1)

	var apiErr *APIError
	require.ErrorAs(t, l.m.Status()[0].Err, &apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.Status)
	require.Equal(t, "pve2.lab.invalid", l.m.Current().Address)
}

func TestMultiReturnsAClientErrorFromTheFirstEndpoint(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a, _ := failing(t, status)
			b, recB := answering(t)
			l := newLab(t, testConfig(""), a, b)

			_, err := l.m.Version(t.Context())
			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, status, apiErr.Status)
			require.Empty(t, recB.requests(), "an answer of the API is the same everywhere")
			require.Equal(t, "pve1.lab.invalid:8006", l.m.Current().Address)
			require.NoError(t, l.m.Status()[0].Err, "the endpoint answered")
		})
	}
}

func TestMultiVerifiesEachEndpointUnderItsOwnName(t *testing.T) {
	b, recB := answering(t)
	cfg := testConfig("https://pve1.lab.invalid:8006")
	cfg.CAFile = writeCA(t, b.Certificate())
	cfg.ServerName = "example.com"
	m, err := NewMulti(cfg, []Endpoint{{Address: "pve2.lab.invalid", ServerName: "pve2.lab.invalid"}})
	require.NoError(t, err)
	l := wire(t, m, down(t), b)

	_, err = l.m.Version(t.Context())
	require.Error(t, err)
	require.Empty(t, recB.requests())
	var wrongName x509.HostnameError
	require.ErrorAs(t, l.m.Status()[1].Err, &wrongName)
	require.Equal(t, "pve1.lab.invalid:8006", l.m.Current().Address, "nothing answered")

	m, err = NewMulti(cfg, []Endpoint{{Address: "pve2.lab.invalid", ServerName: "example.com"}})
	require.NoError(t, err)
	l = wire(t, m, down(t), b)
	_, err = l.m.Version(t.Context())
	require.NoError(t, err)
	require.Len(t, recB.requests(), 1)
}

// A fallback endpoint on a loopback address is pinned to the certificate of
// this node, as the first endpoint is: the test servers all present the
// same certificate, so the node's directory decides.
func TestMultiPinsAFallbackEndpointOnALoopbackAddress(t *testing.T) {
	tests := []struct {
		name   string
		served *x509.Certificate
		ok     bool
	}{
		{"the certificate of the node", nil, true},
		{"another certificate", namedCA(t, "another node"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, recB := answering(t)
			served := tt.served
			if served == nil {
				served = b.Certificate()
			}
			cfg := testConfig("https://127.0.0.1:8006")
			cfg.NodeCertDir = t.TempDir()
			writeCert(t, filepath.Join(cfg.NodeCertDir, "pve-ssl.pem"), tls.Certificate{Certificate: [][]byte{served.Raw}})
			m, err := NewMulti(cfg, []Endpoint{{Address: "127.0.0.1:" + portOf(t, b)}})
			require.NoError(t, err)
			l := wire(t, m, down(t), b)

			_, err = l.m.Version(t.Context())
			if tt.ok {
				require.NoError(t, err)
				require.Len(t, recB.requests(), 1)
				return
			}
			require.Error(t, err)
			require.Empty(t, recB.requests())
			require.ErrorContains(t, l.m.Status()[1].Err, "certificate of this node")
		})
	}
}

func TestMultiAWriteMovesOnOnlyBeforeAnythingWasSent(t *testing.T) {
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	values := map[string]string{"net0": "virtio=BC:24:11:00:AA:B5,bridge=vmbr0"}

	t.Run("refused", func(t *testing.T) {
		b, recB := answering(t)
		l := newLab(t, testConfig(""), down(t), b)
		require.NoError(t, l.m.SetGuestConfig(t.Context(), "pve1", ref, values, "d1g3st"))
		require.Len(t, recB.requests(), 1)
		require.Equal(t, http.MethodPut, recB.requests()[0].method)
		require.Equal(t, "/api2/json/nodes/pve1/qemu/101/config", recB.requests()[0].uri)
	})

	t.Run("server error", func(t *testing.T) {
		a, recA := failing(t, http.StatusInternalServerError)
		b, recB := answering(t)
		l := newLab(t, testConfig(""), a, b)
		err := l.m.SetGuestConfig(t.Context(), "pve1", ref, values, "d1g3st")
		var apiErr *APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, http.StatusInternalServerError, apiErr.Status)
		require.Len(t, recA.requests(), 1)
		require.Empty(t, recB.requests(), "the write may have been applied; it is not sent again")
	})
}

func TestMultiReturnsTheErrorOfTheFirstEndpointWhenNoneAnswers(t *testing.T) {
	l := newLab(t, testConfig(""), down(t), down(t))
	_, err := l.m.Version(t.Context())
	var op *net.OpError
	require.ErrorAs(t, err, &op)
	require.Equal(t, 1, l.dialed(0))
	require.Equal(t, 1, l.dialed(1))
	for _, st := range l.m.Status() {
		require.Error(t, st.Err)
	}
	require.Equal(t, "pve1.lab.invalid:8006", l.m.Current().Address)
}

func TestMultiStopsAtADeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	a, _ := labServer(t, func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	})
	b, recB := answering(t)
	cfg := testConfig("")
	cfg.Timeout = 100 * time.Millisecond
	l := newLab(t, cfg, a, b)

	_, err := l.m.Version(t.Context())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	<-started
	require.Empty(t, recB.requests(), "a deadline is for the whole request, not a reason to ask another node")
}

func TestNewMultiChecksTheEndpoints(t *testing.T) {
	srv := remoteServer(t)
	withCA := testConfig("https://pve1.lab.invalid:8006")
	withCA.CAFile = writeCA(t, srv.Certificate())

	tests := []struct {
		name    string
		cfg     Config
		ep      Endpoint
		wantErr string
	}{
		{"remote without ca", testConfig("https://127.0.0.1:8006"), Endpoint{Address: "pve2.lab.invalid"}, "CA file"},
		{"empty address", withCA, Endpoint{}, "address"},
		{"an url", withCA, Endpoint{Address: "https://pve2.lab.invalid:8006"}, "address"},
		{"a path", withCA, Endpoint{Address: "pve2.lab.invalid/api2"}, "address"},
		{"a port alone", withCA, Endpoint{Address: ":8006"}, "address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewMulti(tt.cfg, []Endpoint{tt.ep})
			require.ErrorContains(t, err, tt.wantErr)
		})
	}

	m, err := NewMulti(withCA, []Endpoint{
		{Address: "pve2.lab.invalid"},
		{Address: "pve3.lab.invalid:8443"},
		{Address: "fd00:4::3"},
		{Address: "[fd00:4::4]:8006"},
		{Address: "127.0.0.1"},
	})
	require.NoError(t, err)
	var hosts []string
	for _, tg := range m.fb.targets {
		hosts = append(hosts, tg.host)
	}
	require.Equal(t, []string{"pve1.lab.invalid:8006", "pve2.lab.invalid:8006", "pve3.lab.invalid:8443", "[fd00:4::3]:8006", "[fd00:4::4]:8006", "127.0.0.1:8006"}, hosts)

	m, err = NewMulti(withCA, nil)
	require.NoError(t, err)
	require.Len(t, m.Status(), 1)
}

func TestMultiIsAClient(t *testing.T) {
	a, recA := answering(t)
	l := newLab(t, testConfig(""), a)
	require.NotNil(t, l.m.Client)
	_, err := l.m.Resources(t.Context())
	require.Error(t, err, "the version fixture is no list of resources; the call went through the client")
	require.False(t, errors.Is(err, context.Canceled))
	require.Equal(t, "/api2/json/cluster/resources?type=vm", recA.requests()[0].uri)
}

func portOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	return port
}
