//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// attach attaches a network through the CLI, as an admin does from a script,
// and waits until its leg on this node is up.
func (s *suite) attach(t testing.TB, bridge string, vlan int, addr string) engine.NetworkView {
	t.Helper()
	seg := bridge
	if vlan != 0 {
		seg += ":" + strconv.Itoa(vlan)
	}
	out, err := s.pcoCombined("network", "attach", seg, "--address", addr, "--yes")
	t.Logf("pco network attach %s --address %s:\n%s", seg, addr, out)
	require.NoError(t, err)
	return s.waitLeg(t, store.SegmentID(bridge, vlan), true)
}

// detach detaches a network through the CLI and waits until the state no
// longer lists it.
func (s *suite) detach(t testing.TB, id string) {
	t.Helper()
	out, err := s.pcoCombined("network", "detach", id, "--yes")
	t.Logf("pco network detach %s:\n%s", id, out)
	require.NoError(t, err)
	s.waitState(t, "network "+id+" to be gone", 2*time.Minute, func(st engine.State) bool {
		_, ok := networkOf(st, id)
		return !ok
	})
}

func networkOf(st engine.State, id string) (engine.NetworkView, bool) {
	i := slices.IndexFunc(st.Networks, func(n engine.NetworkView) bool { return n.ID == id })
	if i < 0 {
		return engine.NetworkView{}, false
	}
	return st.Networks[i], true
}

// legOn is the leg of the network on this node, as the last cycle found it.
func legOn(n engine.NetworkView, node string) (engine.LegView, bool) {
	i := slices.IndexFunc(n.Legs, func(l engine.LegView) bool { return l.Node == node })
	if i < 0 {
		return engine.LegView{}, false
	}
	return n.Legs[i], true
}

// waitLeg waits until the leg of the network on this node is up, or, with up
// false, is down with a problem.
func (s *suite) waitLeg(t testing.TB, id string, up bool) engine.NetworkView {
	t.Helper()
	var found engine.NetworkView
	s.waitState(t, fmt.Sprintf("the leg of %s to be up: %v", id, up), 3*time.Minute, func(st engine.State) bool {
		n, ok := networkOf(st, id)
		if !ok {
			return false
		}
		found = n
		leg, ok := legOn(n, st.Node)
		if !ok {
			return false
		}
		return leg.Up == up && (up || leg.Problem != "")
	})
	return found
}

// serviceOf is the service address a route is served through: the host of
// its service, which lies in the service prefix for a route behind a leg.
func serviceOf(t testing.TB, r engine.RouteView) netip.Addr {
	t.Helper()
	u, err := url.Parse(r.Service)
	require.NoError(t, err, r.Service)
	addr, err := netip.ParseAddr(u.Hostname())
	require.NoError(t, err, r.Service)
	require.True(t, gateway.ServicePrefix.Contains(addr), "%s is served at %s, outside the service prefix", r.Hostname, r.Service)
	require.NotNil(t, r.Path, "%s has no path", r.Hostname)
	require.Equal(t, addr, r.Path.Service, "the path of %s names another service address", r.Hostname)
	return addr
}

// waitVia waits until the route is active through via, and returns it.
func (s *suite) waitVia(t testing.TB, host, via string) engine.RouteView {
	t.Helper()
	var found engine.RouteView
	s.waitState(t, fmt.Sprintf("route %s active via %s", host, via), 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, "")
		found = r
		return ok && r.State == "active" && r.Path != nil && r.Path.Via == via
	})
	return found
}

// fetch makes one request from the node to a target, as a connector would,
// and returns the body.
func fetch(target string) (string, error) { return fetchWithin(target, 5*time.Second) }

func fetchWithin(target string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + target + "/")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %d", target, resp.StatusCode)
	}
	return strings.TrimSpace(string(raw)), nil
}

// requireAnswer fetches a target from the node until it answers with body,
// for at most timeout.
func (s *suite) requireAnswer(t testing.TB, target, body string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		got, err := fetch(target)
		if err == nil && got == body {
			return
		}
		last = got
		if err != nil {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer %q within %s; the last answer: %s", target, body, timeout, last)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// netns runs a command inside the gateway namespace.
func (s *suite) netns(name string, args ...string) (string, error) {
	return s.run("ip", append([]string{"netns", "exec", gateway.Namespace, name}, args...)...)
}

// dnatHas says whether the DNAT map of the namespace holds the service
// address.
func (s *suite) dnatHas(t testing.TB, service netip.Addr) bool {
	t.Helper()
	out, err := s.netns("nft", "-j", "list", "map", "inet", "pco_gw", "svc_dnat")
	require.NoError(t, err)
	return strings.Contains(out, `"`+service.String()+`"`)
}

// netnsStatus runs pco netns status and says whether the namespace is as pco
// left it.
func (s *suite) netnsStatus(t testing.TB) (string, bool) {
	t.Helper()
	out, err := s.pcoCombined("netns", "status")
	require.Contains(t, []int{0, 1}, exitCode(err), "pco netns status: %v", err)
	return out, err == nil
}

func (s *suite) requireNetnsKept(t testing.TB) {
	t.Helper()
	out, ok := s.netnsStatus(t)
	require.True(t, ok, "the namespace is not as pco left it:\n%s", out)
}

// counter reads the packets of a named counter of an nftables table, on the
// host or in the namespace.
func (s *suite) counter(t testing.TB, inNamespace bool, family, table, name string) int {
	t.Helper()
	var out string
	var err error
	if inNamespace {
		out, err = s.netns("nft", "-j", "list", "counter", family, table, name)
	} else {
		out, err = s.run("nft", "-j", "list", "counter", family, table, name)
	}
	require.NoError(t, err)
	var listing struct {
		Nftables []struct {
			Counter *struct {
				Packets int `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listing), out)
	for _, item := range listing.Nftables {
		if item.Counter != nil {
			return item.Counter.Packets
		}
	}
	t.Fatalf("no counter %s in %s %s:\n%s", name, family, table, out)
	return 0
}

var counterValues = regexp.MustCompile(`"(packets|bytes|handle)":\s*\d+`)

// ruleset is the namespace's ruleset without the values of its counters and
// the handles of its rules, so that two readings compare.
func (s *suite) ruleset(t testing.TB) string {
	t.Helper()
	out, err := s.netns("nft", "-j", "list", "ruleset")
	require.NoError(t, err)
	return counterValues.ReplaceAllString(out, `"$1":0`)
}

// hostRoutes are the routes of the service prefix on the host: the one
// through the transit link and the unreachable fallback behind it.
func (s *suite) hostRoutes(t testing.TB) string {
	t.Helper()
	return s.must(t, "ip", "route", "show", gateway.ServicePrefix.String())
}

func (s *suite) requireHostRoutes(t testing.TB) {
	t.Helper()
	routes := s.hostRoutes(t)
	require.Contains(t, routes, "via "+gateway.TransitNS.String(), routes)
	require.Contains(t, routes, "unreachable", routes)
}

// serviceAddrs returns the service addresses the store holds, by service.
func (s *suite) serviceAddrs(t testing.TB) map[netip.Addr]store.ServiceAddr {
	t.Helper()
	st, err := store.Open(store.DefaultPaths())
	require.NoError(t, err)
	mappings, err := st.ServiceAddrs()
	require.NoError(t, err)
	out := map[netip.Addr]store.ServiceAddr{}
	for _, m := range mappings {
		out[m.Service] = m
	}
	return out
}

// load makes n requests to a target from the node, concurrent ones at a
// time, and counts the answers with the body, with another body (crossed),
// and the failures.
func load(target, body string, n, concurrent int) (ok, crossed, failed int) {
	var okN, crossedN, failedN atomic.Int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrent)
	for range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			got, err := fetchWithin(target, 20*time.Second)
			switch {
			case err != nil:
				failedN.Add(1)
			case got == body:
				okN.Add(1)
			default:
				crossedN.Add(1)
			}
		}()
	}
	wg.Wait()
	return int(okN.Load()), int(crossedN.Load()), int(failedN.Load())
}

// conntrackPorts returns the source ports of the host's conntrack entries
// of TCP flows to dst on port, in their original direction, and whether
// conntrack is there to ask.
func (s *suite) conntrackPorts(t testing.TB, dst string, port int) ([]int, bool) {
	t.Helper()
	out, err := s.run("conntrack", "-L", "-p", "tcp", "--dport", strconv.Itoa(port))
	if err != nil {
		if exitCode(err) == -1 {
			return nil, false
		}
		// Exit 1 with nothing matched is fine; anything else is not.
		require.Contains(t, []int{0, 1}, exitCode(err), "conntrack -L: %v", err)
	}
	var ports []int
	for line := range strings.Lines(out) {
		if !strings.Contains(line, " dst="+dst+" ") {
			continue
		}
		f := strings.Fields(line)
		for _, kv := range f {
			if sport, ok := strings.CutPrefix(kv, "sport="); ok {
				p, err := strconv.Atoi(sport)
				require.NoError(t, err, line)
				ports = append(ports, p)
				break
			}
		}
	}
	return ports, true
}

// traffic keeps requesting targets from the node, as visitors would through
// the connectors, and counts the requests that failed.
type traffic struct {
	targets map[string]string // body by target
	stop    chan struct{}
	stopped sync.Once
	done    chan struct{}
	total   atomic.Int64
	failed  atomic.Int64
	mu      sync.Mutex
	errs    []string
}

// startTraffic requests every target about ten times a second until ended,
// or until the test ends, whichever comes first.
func startTraffic(t testing.TB, targets map[string]string) *traffic {
	tr := &traffic{targets: targets, stop: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(func() { tr.stopped.Do(func() { close(tr.stop) }) })
	go func() {
		defer close(tr.done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tr.stop:
				return
			case <-tick.C:
			}
			for target, body := range targets {
				got, err := fetch(target)
				tr.total.Add(1)
				if err == nil && got == body {
					continue
				}
				tr.failed.Add(1)
				tr.mu.Lock()
				if len(tr.errs) < 20 {
					msg := got
					if err != nil {
						msg = err.Error()
					}
					tr.errs = append(tr.errs, time.Now().Format(time.TimeOnly)+" "+target+": "+msg)
				}
				tr.mu.Unlock()
			}
		}
	}()
	return tr
}

// end stops the traffic and returns how many requests were made and how
// many failed, with the first failures.
func (tr *traffic) end() (total, failed int, errs []string) {
	tr.stopped.Do(func() { close(tr.stop) })
	<-tr.done
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return int(tr.total.Load()), int(tr.failed.Load()), tr.errs
}
