package connector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

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
			HAConnections: 4, ConfigVersion: 2, Version: "2026.9.3", Started: 1.79121620879e+09,
			Edges:     []Edge{{0, "prg01"}, {1, "vie05"}, {2, "prg03"}, {3, "vie05"}},
			RTTMillis: []float64{5, 11, 6, 21},
		}},
		{"testdata/metrics-http2.txt", Metrics{
			HAConnections: 4, ConfigVersion: 2, Version: "2026.9.3", Started: 1.7912162541e+09,
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
process_start_time_seconds 1.7912162541e+09
something_else{label="with } and , inside"} NaN
`

	got, err := ParseMetrics(strings.NewReader(body), maxMetricsBody)

	require.NoError(t, err)
	require.Equal(t, Metrics{
		Requests: 1234000, RequestErrors: 17, Concurrent: 3, HAConnections: 2, ConfigVersion: 14, Version: "2026.9.3",
		Started: 1.7912162541e+09,
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

// The metrics port is a local TCP port: whatever answers on it is read as
// hostile. A value must be a number a graph can show.
func TestAValueThatIsNotFiniteIsRefused(t *testing.T) {
	const counters = "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n"
	for _, series := range []string{
		"cloudflared_tunnel_total_requests",
		"cloudflared_tunnel_request_errors",
		"cloudflared_tunnel_concurrent_requests_per_tunnel",
		"cloudflared_tunnel_ha_connections",
		"cloudflared_orchestration_config_version",
		"process_start_time_seconds",
		`build_info{version="2026.9.3"}`,
		`cloudflared_tunnel_server_locations{connection_id="0",edge_location="fra08"}`,
		`quic_client_smoothed_rtt{conn_index="0"}`,
	} {
		for _, value := range []string{"NaN", "+Inf", "-Inf"} {
			t.Run(series+" "+value, func(t *testing.T) {
				_, err := ParseMetrics(strings.NewReader(counters+series+" "+value+"\n"), maxMetricsBody)

				require.ErrorContains(t, err, "not finite")
			})
		}
	}
}

func TestAnIntegerSeriesOutsideItsRangeIsRefused(t *testing.T) {
	const counters = "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n"
	for _, series := range []string{"cloudflared_tunnel_ha_connections", "cloudflared_orchestration_config_version"} {
		for _, value := range []string{"-1", "1048577", "1e300", "9223372036854775808"} {
			t.Run(series+" "+value, func(t *testing.T) {
				_, err := ParseMetrics(strings.NewReader(counters+series+" "+value+"\n"), maxMetricsBody)

				require.ErrorContains(t, err, "out of range")
			})
		}
	}

	got, err := ParseMetrics(strings.NewReader(counters+"cloudflared_tunnel_ha_connections 1048576\n"), maxMetricsBody)

	require.NoError(t, err)
	require.Equal(t, 1<<20, got.HAConnections, "the largest value")
}

// cloudflared numbers the connections of a tunnel with a uint8.
func TestOnlyConnectionIndexesOfAByteAreAccepted(t *testing.T) {
	const counters = "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n"
	locations := func(index string) string {
		return counters + `cloudflared_tunnel_server_locations{connection_id="` + index + `",edge_location="fra08"} 1` + "\n"
	}
	rtts := func(index string) string {
		return counters + `quic_client_smoothed_rtt{conn_index="` + index + `"} 7` + "\n"
	}

	got, err := ParseMetrics(strings.NewReader(locations("255")), maxMetricsBody)
	require.NoError(t, err)
	require.Equal(t, []Edge{{255, "fra08"}}, got.Edges, "the last index")
	got, err = ParseMetrics(strings.NewReader(rtts("255")), maxMetricsBody)
	require.NoError(t, err)
	require.Len(t, got.RTTMillis, 256)
	require.Equal(t, 7.0, got.RTTMillis[255])

	for _, index := range []string{"256", "-1", "70000", "4294967296"} {
		_, err := ParseMetrics(strings.NewReader(locations(index)), maxMetricsBody)
		require.ErrorContains(t, err, "cloudflared_tunnel_server_locations", "connection "+index)
		_, err = ParseMetrics(strings.NewReader(rtts(index)), maxMetricsBody)
		require.ErrorContains(t, err, "quic_client_smoothed_rtt", "connection "+index)
	}
}

func TestTheStringsOfAnAnswerAreCapped(t *testing.T) {
	const counters = "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n"
	body := func(version, location string) string {
		return counters + `build_info{version="` + version + `"} 1` + "\n" +
			`cloudflared_tunnel_server_locations{connection_id="0",edge_location="` + location + `"} 1` + "\n"
	}

	got, err := ParseMetrics(strings.NewReader(body(strings.Repeat("v", 64), strings.Repeat("l", 64))), maxMetricsBody)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("v", 64), got.Version, "64 bytes are kept")
	require.Equal(t, strings.Repeat("l", 64), got.Edges[0].Location)

	got, err = ParseMetrics(strings.NewReader(body(strings.Repeat("v", 100000), strings.Repeat("l", 65))), maxMetricsBody)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("v", 64), got.Version)
	require.Equal(t, strings.Repeat("l", 64), got.Edges[0].Location)

	// A cut is made between two characters, not inside one.
	multi := "a" + strings.Repeat("é", 40)
	got, err = ParseMetrics(strings.NewReader(body(multi, multi)), maxMetricsBody)
	require.NoError(t, err)
	require.Equal(t, "a"+strings.Repeat("é", 31), got.Version)
	require.True(t, utf8.ValidString(got.Edges[0].Location))
	require.LessOrEqual(t, len(got.Edges[0].Location), 64)
}

// The round trip times are by connection index, whatever the connections that
// say nothing of theirs.
func TestTheRoundTripTimesAreByConnectionIndex(t *testing.T) {
	body := "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n" +
		`quic_client_smoothed_rtt{conn_index="2"} 7` + "\n" +
		`quic_client_smoothed_rtt{conn_index="0"} 5` + "\n"

	got, err := ParseMetrics(strings.NewReader(body), maxMetricsBody)

	require.NoError(t, err)
	require.Equal(t, []float64{5, 0, 7}, got.RTTMillis)
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

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stalled gives its text and then waits until its request ends.
type stalled struct {
	ctx  context.Context
	text io.Reader
}

func (s *stalled) Read(p []byte) (int, error) {
	if n, _ := s.text.Read(p); n > 0 {
		return n, nil
	}
	<-s.ctx.Done()
	return 0, s.ctx.Err()
}

func (s *stalled) Close() error { return nil }

// A connector that does not answer, or does not end its answer, is let go
// after probeTimeout: the body is read within it too.
func TestAScrapeEndsAtTheTimeout(t *testing.T) {
	for _, tt := range []struct {
		name string
		do   roundTripperFunc
	}{
		{"an answer that does not come", func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}},
		{"a body that does not end", func(r *http.Request) (*http.Response, error) {
			text := "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n"
			return &http.Response{
				StatusCode: http.StatusOK, Status: "200 OK",
				Body: &stalled{ctx: r.Context(), text: strings.NewReader(text)},
			}, nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, _, dir := newTestManager(t)
				m.httpc = &http.Client{Transport: tt.do}
				writeFile(t, dir, idA+".env", "METRICS_ADDR=127.0.0.1:20300\n")

				start := time.Now()
				_, err := m.Metrics(t.Context(), idA)

				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Equal(t, probeTimeout, time.Since(start))
			})
		})
	}
}

// What answers on the port of a connector may send the request elsewhere; the
// manager's own client does not follow it.
func TestARedirectOfAConnectorIsNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		_, _ = io.WriteString(w, "cloudflared_tunnel_total_requests 1\ncloudflared_tunnel_request_errors 0\n")
	}))
	t.Cleanup(target.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	m, sd, dir := newTestManager(t)
	writeFile(t, dir, idA+".env", "METRICS_ADDR="+redirecting.Listener.Addr().String()+"\n")
	sd.active[unitA] = true

	t.Run("the metrics", func(t *testing.T) {
		_, err := m.Metrics(t.Context(), idA)

		require.ErrorContains(t, err, "302")
		require.Zero(t, elsewhere.Load())
	})
	t.Run("the readiness", func(t *testing.T) {
		got, err := m.Status(t.Context(), idA)

		require.NoError(t, err)
		require.False(t, got.Ready)
		require.Zero(t, elsewhere.Load())
	})
}
