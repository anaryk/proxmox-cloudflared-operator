package connector

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// statusFixture is a manager whose tunnel idA has a metrics endpoint served by
// handler and a running unit.
type statusFixture struct {
	m        *Manager
	sd       *fakeSystemd
	dir      string
	addr     string
	requests atomic.Int32
}

func newStatusFixture(t *testing.T, handler http.HandlerFunc) *statusFixture {
	t.Helper()
	f := &statusFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		require.Equal(t, "/ready", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	f.m, f.sd, f.dir = newTestManager(t)
	f.m.httpc = srv.Client()
	f.addr = srv.Listener.Addr().String()
	writeFile(t, f.dir, idA+".env", "METRICS_ADDR="+f.addr+"\n")
	f.sd.active[unitA] = true
	return f
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestStatusOfAReadyConnector(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, `{"status":200,"readyConnections":4,"connectorId":"c1"}`))

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, Active: true, Ready: true, Connections: 4, MetricsAddr: f.addr}, got)
}

func TestStatusOfAConnectorThatIsNotReady(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusServiceUnavailable, `{"status":503,"readyConnections":0}`))

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, Active: true, Ready: false, MetricsAddr: f.addr}, got)
}

func TestStatusOfAnInactiveUnitDoesNotAskTheEndpoint(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, `{"readyConnections":4}`))
	f.sd.active[unitA] = false

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, MetricsAddr: f.addr}, got)
	require.Zero(t, f.requests.Load())
}

func TestStatusIsReadyWhenTheBodyCannotBeRead(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, `ready`))

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, Active: true, Ready: true, MetricsAddr: f.addr}, got)
}

func TestStatusWithoutAnEnvFileIsActiveAndNotReady(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, `{"readyConnections":4}`))
	require.NoError(t, os.Remove(filepath.Join(f.dir, idA+".env")))

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, Active: true}, got)
	require.Zero(t, f.requests.Load())
}

func TestStatusWithAMalformedEnvFileIsActiveAndNotReady(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, `{"readyConnections":4}`))
	writeFile(t, f.dir, idA+".env", "METRICS_ADDR=no-port\n")

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, Active: true}, got)
	require.Zero(t, f.requests.Load())
}

func TestStatusWhenTheEndpointDoesNotAnswerIsNotAnError(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, ``))
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.Listener.Addr().String()
	closed.Close() // nothing listens there any more
	writeFile(t, f.dir, idA+".env", "METRICS_ADDR="+addr+"\n")

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, Active: true, MetricsAddr: addr}, got)
}

func TestStatusReadsAQuotedAddress(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, `{"readyConnections":2}`))
	writeFile(t, f.dir, idA+".env", "METRICS_ADDR=\""+f.addr+"\"\n")

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, Status{TunnelID: idA, Active: true, Ready: true, Connections: 2, MetricsAddr: f.addr}, got)
}

func TestStatusReportsAFailedActivityCheck(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, ``))
	f.sd.fail["IsActive "+unitA] = errBoom

	_, err := f.m.Status(t.Context(), idA)

	require.ErrorIs(t, err, errBoom)
	require.Zero(t, f.requests.Load())
}

func TestStatusRejectsInvalidIDs(t *testing.T) {
	m, sd, dir := newTestManager(t)

	_, err := m.Status(t.Context(), "../"+idA[3:])

	require.ErrorContains(t, err, "invalid tunnel id")
	require.Empty(t, sd.calls)
	require.NoDirExists(t, dir)
}

func TestStatusReportsAnUnreadableEnvFile(t *testing.T) {
	f := newStatusFixture(t, answer(http.StatusOK, ``))
	path := filepath.Join(f.dir, idA+".env")
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700)) // reading a directory fails with something other than "not exist"

	_, err := f.m.Status(t.Context(), idA)

	require.ErrorContains(t, err, idA+".env")
	require.Zero(t, f.requests.Load())
}

// roundTripFunc is an http.RoundTripper made of a function.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestStatusGivesTheEndpointTwoSeconds(t *testing.T) {
	var deadline time.Time
	var url string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, _ = r.Context().Deadline()
		url = r.URL.String()
		return nil, errors.New("no network in this test")
	})}
	m, sd, dir := newTestManager(t)
	m.httpc = client
	writeFile(t, dir, idA+".env", "METRICS_ADDR=127.0.0.1:20300\n")
	sd.active[unitA] = true

	start := time.Now()
	got, err := m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.False(t, got.Ready)
	require.Equal(t, "http://127.0.0.1:20300/ready", url)
	require.False(t, deadline.IsZero(), "the request has no deadline")
	require.GreaterOrEqual(t, deadline.Sub(start), 2*time.Second)
	require.LessOrEqual(t, time.Until(deadline), 2*time.Second)
}

func TestStatusReadsOnlyTheStartOfTheBody(t *testing.T) {
	f := newStatusFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(" ", 1<<20))
		_, _ = io.WriteString(w, `{"readyConnections":4}`)
	})

	got, err := f.m.Status(t.Context(), idA)

	require.NoError(t, err)
	require.True(t, got.Ready)
	require.Zero(t, got.Connections)
}
