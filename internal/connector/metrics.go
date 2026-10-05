package connector

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// maxMetricsBody is how much of /metrics is read. cloudflared serves about
// 20 KiB.
const maxMetricsBody = 1 << 20

// The series of cloudflared's /metrics that the traffic is made of.
const (
	seriesRequests   = "cloudflared_tunnel_total_requests"
	seriesErrors     = "cloudflared_tunnel_request_errors"
	seriesConcurrent = "cloudflared_tunnel_concurrent_requests_per_tunnel"
	seriesHA         = "cloudflared_tunnel_ha_connections"
	seriesConfig     = "cloudflared_orchestration_config_version"
	seriesBuild      = "build_info"
	seriesLocations  = "cloudflared_tunnel_server_locations"
	seriesRTT        = "quic_client_smoothed_rtt"
)

// Edge is a connection of a connector to Cloudflare's edge and where it ends.
type Edge struct {
	Connection int    `json:"connection"`
	Location   string `json:"location"` // as cloudflared reports it, e.g. "fra08"
}

// Metrics is what a connector's /metrics says of its traffic and its
// connections. The counters count from the start of the connector.
type Metrics struct {
	Requests      float64   // cloudflared_tunnel_total_requests
	RequestErrors float64   // cloudflared_tunnel_request_errors
	Concurrent    float64   // cloudflared_tunnel_concurrent_requests_per_tunnel
	HAConnections int       // cloudflared_tunnel_ha_connections
	ConfigVersion int       // cloudflared_orchestration_config_version
	Version       string    // build_info{version}
	Edges         []Edge    // cloudflared_tunnel_server_locations{connection_id,edge_location}, by connection
	RTTMillis     []float64 // quic_client_smoothed_rtt{conn_index}, by index; empty over http2
}

// Metrics scrapes the connector's /metrics, at the address its env file
// names, within probeTimeout and maxMetricsBody.
func (m *Manager) Metrics(ctx context.Context, tunnelID string) (Metrics, error) {
	if err := checkID(tunnelID); err != nil {
		return Metrics{}, err
	}
	values, err := readEnv(m.path(envFile(tunnelID)))
	if err != nil {
		return Metrics{}, fmt.Errorf("tunnel %s: reading env file: %w", tunnelID, err)
	}
	addr, _, err := metricsOf(values)
	if err != nil {
		return Metrics{}, fmt.Errorf("tunnel %s: %w", tunnelID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	if err != nil {
		return Metrics{}, fmt.Errorf("tunnel %s: %w", tunnelID, err)
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		return Metrics{}, fmt.Errorf("tunnel %s: scraping %s: %w", tunnelID, addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Metrics{}, fmt.Errorf("tunnel %s: %s answered %s", tunnelID, addr, resp.Status)
	}
	got, err := ParseMetrics(resp.Body, maxMetricsBody)
	if err != nil {
		return Metrics{}, fmt.Errorf("tunnel %s: %w", tunnelID, err)
	}
	return got, nil
}

// ParseMetrics reads the Prometheus text format and keeps the series of
// Metrics; others are skipped. At most limit bytes are read: a longer answer
// is an error. Both counters must be there, as no rate can be made without
// them; the rest is zero or empty when it is missing.
func ParseMetrics(r io.Reader, limit int64) (Metrics, error) {
	var (
		m        Metrics
		read     int64
		seen     = map[string]bool{}
		rtts     = map[int]float64{}
		at       = map[int]string{}
		tooLong  = fmt.Errorf("the metrics are longer than %d bytes", limit)
		buffered = bufio.NewReader(io.LimitReader(r, limit+1))
	)
	for {
		line, err := buffered.ReadString('\n')
		if read += int64(len(line)); read > limit {
			return Metrics{}, tooLong
		}
		if perr := parseLine(&m, strings.TrimSpace(line), seen, rtts, at); perr != nil {
			return Metrics{}, perr
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Metrics{}, fmt.Errorf("reading the metrics: %w", err)
		}
	}
	for _, name := range []string{seriesRequests, seriesErrors} {
		if !seen[name] {
			return Metrics{}, fmt.Errorf("no %s in the metrics", name)
		}
	}
	for _, conn := range slices.Sorted(maps.Keys(rtts)) {
		m.RTTMillis = append(m.RTTMillis, rtts[conn])
	}
	for conn, loc := range at {
		m.Edges = append(m.Edges, Edge{Connection: conn, Location: loc})
	}
	slices.SortFunc(m.Edges, func(a, b Edge) int { return cmp.Compare(a.Connection, b.Connection) })
	return m, nil
}

// parseLine takes what a line says of the series of Metrics into m.
func parseLine(m *Metrics, line string, seen map[string]bool, rtts map[int]float64, at map[int]string) error {
	if line == "" || line[0] == '#' {
		return nil
	}
	name := line[:strings.IndexAny(line+" ", "{ \t")]
	switch name {
	case seriesRequests, seriesErrors, seriesConcurrent, seriesHA, seriesConfig, seriesBuild, seriesLocations, seriesRTT:
	default:
		return nil
	}
	labels, value, err := splitSample(line[len(name):])
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	seen[name] = true
	switch name {
	case seriesRequests:
		m.Requests = value
	case seriesErrors:
		m.RequestErrors = value
	case seriesConcurrent:
		m.Concurrent = value
	case seriesHA:
		m.HAConnections = int(value)
	case seriesConfig:
		m.ConfigVersion = int(value)
	case seriesBuild:
		m.Version = labels["version"]
	case seriesLocations:
		conn, err := strconv.Atoi(labels["connection_id"])
		if err != nil {
			return fmt.Errorf("reading %s: connection_id %q is no number", name, labels["connection_id"])
		}
		// 1 is where a connection is now, 0 where it was before.
		if value > 0 {
			at[conn] = labels["edge_location"]
		}
	case seriesRTT:
		conn, err := strconv.Atoi(labels["conn_index"])
		if err != nil {
			return fmt.Errorf("reading %s: conn_index %q is no number", name, labels["conn_index"])
		}
		rtts[conn] = value
	}
	return nil
}

// splitSample reads what follows the name of a series on its line: the
// labels, when there are any, and the value. A timestamp after the value is
// left alone.
func splitSample(rest string) (map[string]string, float64, error) {
	labels := map[string]string{}
	if strings.HasPrefix(rest, "{") {
		var err error
		if labels, rest, err = readLabels(rest[1:]); err != nil {
			return nil, 0, err
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return nil, 0, errors.New("no value")
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil, 0, fmt.Errorf("value %q is no number", fields[0])
	}
	return labels, value, nil
}

// readLabels reads `name="value",...}` and returns the labels and what follows
// the closing brace. Values are unescaped as the text format escapes them.
func readLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t,")
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], nil
		}
		name, rest, ok := strings.Cut(s, "=")
		if !ok || !strings.HasPrefix(rest, `"`) {
			return nil, "", errors.New("the labels do not end")
		}
		var value strings.Builder
		i := 1
		for ; i < len(rest) && rest[i] != '"'; i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
				if rest[i] == 'n' {
					value.WriteByte('\n')
					continue
				}
			}
			value.WriteByte(rest[i])
		}
		if i >= len(rest) {
			return nil, "", errors.New("the labels do not end")
		}
		labels[strings.TrimSpace(name)] = value.String()
		s = rest[i+1:]
	}
}
