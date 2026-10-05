package connector

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// The testdata files are what cloudflared 2026.9.3 served on /metrics, run
// with a token on a lab node, once over QUIC and once with --protocol http2,
// after a second configuration was pushed to the tunnel.
func TestTheMetricsOfARealConnector(t *testing.T) {
	for _, tt := range []struct {
		file string
		want Metrics
	}{
		{"testdata/metrics-quic.txt", Metrics{
			HAConnections: 4, ConfigVersion: 2, Version: "2026.9.3",
			Edges:     []Edge{{0, "prg01"}, {1, "vie05"}, {2, "prg03"}, {3, "vie05"}},
			RTTMillis: []float64{5, 11, 6, 21},
		}},
		{"testdata/metrics-http2.txt", Metrics{
			HAConnections: 4, ConfigVersion: 2, Version: "2026.9.3",
			Edges: []Edge{{0, "prg01"}, {1, "vie05"}, {2, "vie06"}, {3, "prg03"}},
		}},
	} {
		t.Run(tt.file, func(t *testing.T) {
			f, err := os.Open(tt.file)
			require.NoError(t, err)
			defer func() { _ = f.Close() }()

			got, err := ParseMetrics(f, maxMetricsBody)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestOnlyTheSeriesOfTheTrafficAreKept(t *testing.T) {
	body := `# HELP cloudflared_tunnel_total_requests Amount of requests proxied through all the tunnels
# TYPE cloudflared_tunnel_total_requests counter
cloudflared_tunnel_total_requests 1.234e+06

cloudflared_tunnel_total_requests_by_host{host="a"} 7
cloudflared_tunnel_request_errors 17 1759665600000
cloudflared_tunnel_concurrent_requests_per_tunnel 3
cloudflared_tunnel_ha_connections 2
cloudflared_orchestration_config_version 14
build_info{goversion="go1.26.8",revision="2026-09-24-16:07 UTC \"x\"",type="",version="2026.9.3"} 1
cloudflared_tunnel_server_locations{connection_id="1",edge_location="prg01"} 1
cloudflared_tunnel_server_locations{connection_id="0",edge_location="fra08"} 1
cloudflared_tunnel_server_locations{connection_id="0",edge_location="ams01"} 0
quic_client_smoothed_rtt{conn_index="1"} 18.9
quic_client_smoothed_rtt{conn_index="0"} 11.2
quic_client_latest_rtt{conn_index="0"} 99
go_gc_duration_seconds{quantile="0.5"} 3.1e-05
process_open_fds 12
something_else{label="with } and , inside"} NaN
`

	got, err := ParseMetrics(strings.NewReader(body), maxMetricsBody)

	require.NoError(t, err)
	require.Equal(t, Metrics{
		Requests: 1234000, RequestErrors: 17, Concurrent: 3, HAConnections: 2, ConfigVersion: 14, Version: "2026.9.3",
		// A location that says 0 is where the connection was before.
		Edges:     []Edge{{0, "fra08"}, {1, "prg01"}},
		RTTMillis: []float64{11.2, 18.9},
	}, got)
}

func TestMetricsThatCannotBeUsed(t *testing.T) {
	const counters = "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n"
	for _, tt := range []struct {
		name, body, want string
	}{
		{"no requests counter", "cloudflared_tunnel_request_errors 0\n", "no cloudflared_tunnel_total_requests"},
		{"no errors counter", "cloudflared_tunnel_total_requests 1\n", "no cloudflared_tunnel_request_errors"},
		{"nothing at all", "", "no cloudflared_tunnel_total_requests"},
		{"a value that is no number", counters + "cloudflared_tunnel_ha_connections four\n", `cloudflared_tunnel_ha_connections`},
		{"no value", counters + "cloudflared_tunnel_concurrent_requests_per_tunnel\n", "cloudflared_tunnel_concurrent_requests_per_tunnel"},
		{"labels that do not end", counters + `quic_client_smoothed_rtt{conn_index="0" 5` + "\n", "quic_client_smoothed_rtt"},
		{"a connection that is no number", counters + `quic_client_smoothed_rtt{conn_index="x"} 5` + "\n", "quic_client_smoothed_rtt"},
		{"a location without its connection", counters + `cloudflared_tunnel_server_locations{edge_location="fra08"} 1` + "\n", "cloudflared_tunnel_server_locations"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMetrics(strings.NewReader(tt.body), maxMetricsBody)

			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestTheMetricsAreReadUpToTheLimit(t *testing.T) {
	body := "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n"

	_, err := ParseMetrics(strings.NewReader(body), int64(len(body)))
	require.NoError(t, err, "a body of exactly the limit")

	long := body + "# " + strings.Repeat("x", 200) + "\n"
	_, err = ParseMetrics(strings.NewReader(long), 100)
	require.ErrorContains(t, err, "longer than 100 bytes")

	// A line that never ends is not read to its end.
	endless := &countingReader{r: io.MultiReader(strings.NewReader("# "), neverEnding('x'))}
	_, err = ParseMetrics(endless, 1000)
	require.ErrorContains(t, err, "longer than 1000 bytes")
	require.LessOrEqual(t, endless.n.Load(), int64(1001))
}

type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// metricsFixture is a manager whose tunnel idA has a metrics endpoint served
// by handler.
func metricsFixture(t *testing.T, handler http.HandlerFunc) (*Manager, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		require.Equal(t, "/metrics", r.URL.Path)
		require.Equal(t, http.MethodGet, r.Method)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	m, _, dir := newTestManager(t)
	m.httpc = srv.Client()
	writeFile(t, dir, idA+".env", "METRICS_ADDR="+srv.Listener.Addr().String()+"\n")
	return m, &requests
}

func TestMetricsOfAConnector(t *testing.T) {
	quic, err := os.ReadFile("testdata/metrics-quic.txt")
	require.NoError(t, err)
	m, requests := metricsFixture(t, answer(http.StatusOK, string(quic)))

	got, err := m.Metrics(t.Context(), idA)

	require.NoError(t, err)
	require.Equal(t, 4, got.HAConnections)
	require.Equal(t, "2026.9.3", got.Version)
	require.Equal(t, int32(1), requests.Load())
}

func TestMetricsThatCannotBeScraped(t *testing.T) {
	t.Run("an answer that is not 200", func(t *testing.T) {
		m, _ := metricsFixture(t, answer(http.StatusServiceUnavailable, "cloudflared_tunnel_total_requests 1\n"))

		_, err := m.Metrics(t.Context(), idA)

		require.ErrorContains(t, err, "503")
	})
	t.Run("an answer over a MiB", func(t *testing.T) {
		big := "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n" + strings.Repeat("# padding\n", maxMetricsBody/10+1)
		m, _ := metricsFixture(t, answer(http.StatusOK, big))

		_, err := m.Metrics(t.Context(), idA)

		require.ErrorContains(t, err, "longer than 1048576 bytes")
	})
	t.Run("a tunnel without an env file", func(t *testing.T) {
		m, requests := metricsFixture(t, answer(http.StatusOK, ""))

		_, err := m.Metrics(t.Context(), idB)

		require.ErrorContains(t, err, "no metrics address")
		require.Zero(t, requests.Load())
	})
	t.Run("no tunnel id", func(t *testing.T) {
		m, requests := metricsFixture(t, answer(http.StatusOK, ""))

		_, err := m.Metrics(t.Context(), "../x")

		require.ErrorContains(t, err, "invalid tunnel id")
		require.Zero(t, requests.Load())
	})
}
