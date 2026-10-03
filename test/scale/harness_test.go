//go:build scale

package scale

import (
	"context"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	testInstall = "abc123"
	testAccount = "acc1"
	testZone    = "zone1"
	testToken   = "cf-api-token-0123456789"
	pollEvery   = 10 * time.Second
)

var start = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// unlimited is a limiter that never makes a request wait, so that a cycle
// shows its own cost and not the budget of a credential.
func unlimited() *cfapi.Limiter { return cfapi.NewLimiter(1_000_000_000, time.Second, 1_000_000, nil) }

// bench is the real engine over a real store in a temporary directory, the
// real Cloudflare client over HTTP to the fake Cloudflare, and the real
// resolver over a prober that needs no network.
type bench struct {
	t      *testing.T
	clock  *clock
	fleet  *fleet
	inv    *inventoryFake
	prober *prober
	cf     *cffake.Fake
	eng    *engine.Engine

	root          string // where the store lives
	renamed, gone map[int]bool
}

func newBench(t *testing.T, guests, routes int, limiter *cfapi.Limiter) *bench {
	t.Helper()
	b := &bench{
		t:       t,
		clock:   &clock{t: start},
		fleet:   newFleet(guests, routes),
		inv:     &inventoryFake{},
		cf:      cffake.New(),
		renamed: map[int]bool{},
		gone:    map[int]bool{},
	}
	b.prober = newProber(b.fleet)
	b.inv.set(b.fleet.snapshot(nil, nil))

	base := tempRoot(t)
	b.root = base
	paths := store.Paths{
		Cluster: filepath.Join(base, "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "local"),
	}
	st, err := store.Open(paths)
	require.NoError(t, err)
	require.NoError(t, st.Init())
	require.NoError(t, st.SaveInstall(store.Install{ID: testInstall, CreatedAt: start}))
	require.NoError(t, st.SaveNode(store.NodeEntry{Name: testNode, Since: start}))
	require.NoError(t, st.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "n1"}))
	require.NoError(t, st.SaveCredential(store.Credential{ID: "cred1", Label: "main", Kind: "scoped", Token: store.NewSecret(testToken), AddedAt: start}))
	settings, err := st.Settings()
	require.NoError(t, err)
	settings.ObserveOnly = false
	require.NoError(t, st.SaveSettings(settings))

	b.cf.SetNow(b.clock.now)
	b.cf.AddAccount(testAccount, "Main")
	b.cf.AddZone(testZone, zoneName, testAccount)
	srv := httptest.NewServer(cffake.Handler(b.cf))
	t.Cleanup(srv.Close)

	eng, err := engine.New(engine.Deps{
		Store:      st,
		Inventory:  b.inv,
		Resolver:   resolve.NewResolver(b.prober, resolve.Settings{LocalNode: testNode}, b.clock.now),
		Connectors: &connectors{tokens: map[string]string{}},
		Egress:     egressFake{},
		NewClient: func(c store.Credential) (cfapi.API, error) {
			return cfapi.New(cfapi.Options{
				BaseURL:    srv.URL + "/client/v4",
				Token:      c.Token.Reveal(),
				Limiter:    limiter,
				HTTPClient: srv.Client(),
			})
		},
		Node:     testNode,
		Now:      b.clock.now,
		Log:      zerolog.Nop(),
		LocalDir: paths.Local,
	})
	require.NoError(t, err)
	b.eng = eng
	return b
}

// publish makes the inventory list the fleet with these guests renamed and
// gone.
func (b *bench) publish() { b.inv.set(b.fleet.snapshot(b.renamed, b.gone)) }

func (b *bench) records() int { return len(b.cf.RecordsIn(testZone)) }

// calls counts what the fake Cloudflare was asked, by kind.
type calls struct {
	dnsRead, dnsWrite, tunnelRead, tunnelWrite, other int
}

func (c calls) total() int { return c.dnsRead + c.dnsWrite + c.tunnelRead + c.tunnelWrite + c.other }

func classify(log []string) calls {
	var c calls
	for _, call := range log {
		switch kindOf(call) {
		case "Records":
			c.dnsRead++
		case "CreateRecord", "UpdateRecord", "DeleteRecord":
			c.dnsWrite++
		case "FindTunnel", "Tunnels", "TunnelToken", "TunnelConfig", "Connectors":
			c.tunnelRead++
		case "CreateTunnel", "DeleteTunnel", "PutTunnelConfig":
			c.tunnelWrite++
		default:
			c.other++
		}
	}
	return c
}

func (c calls) writes() int { return c.dnsWrite + c.tunnelWrite }

// result is what one cycle cost.
type result struct {
	wall    time.Duration
	calls   calls
	files   int    // files of the store that were written
	alloc   uint64 // bytes allocated in total
	mallocs uint64
	peak    uint64 // highest heap seen above the heap before the cycle
	state   engine.State
}

// cycle runs one cycle of the engine, after the poll interval has passed on
// its clock, and measures it. The heap is sampled while it runs: a peak that
// is only read after the cycle would miss the garbage a cycle makes.
func (b *bench) cycle() result {
	b.t.Helper()
	return b.after(pollEvery)
}

// after is cycle with another time between this cycle and the one before.
func (b *bench) after(d time.Duration) result {
	b.t.Helper()
	b.clock.advance(d)
	return b.run()
}

func (b *bench) run() result {
	b.t.Helper()
	before := len(b.cf.Calls())
	files := inodes(b.t, b.root)
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	sampler := startSampler()

	began := time.Now()
	st := b.eng.Cycle(context.Background())
	wall := time.Since(began)

	peak := sampler.stop()
	runtime.ReadMemStats(&m1)
	res := result{
		wall:    wall,
		calls:   classify(b.cf.Calls()[before:]),
		files:   written(files, inodes(b.t, b.root)),
		alloc:   m1.TotalAlloc - m0.TotalAlloc,
		mallocs: m1.Mallocs - m0.Mallocs,
		state:   st,
	}
	if peak > m0.HeapAlloc {
		res.peak = peak - m0.HeapAlloc
	}
	return res
}

// sampler reads the size of the heap often while a cycle runs. It reads the
// runtime metrics, which does not stop the world as ReadMemStats does.
type sampler struct {
	done chan struct{}
	wg   sync.WaitGroup
	peak uint64
}

func startSampler() *sampler {
	s := &sampler{done: make(chan struct{})}
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	s.wg.Go(func() {
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			metrics.Read(sample)
			s.peak = max(s.peak, sample[0].Value.Uint64())
			select {
			case <-s.done:
				return
			case <-tick.C:
			}
		}
	})
	return s
}

func (s *sampler) stop() uint64 {
	close(s.done)
	s.wg.Wait()
	return s.peak
}

// table prints aligned columns to standard output.
type table struct{ w *tabwriter.Writer }

func newTable() table { return table{tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)} }

func (t table) line(format string, args ...any) { _, _ = fmt.Fprintf(t.w, format+"\n", args...) }

func (t table) flush() { _ = t.w.Flush() }

func kindOf(call string) string {
	name, _, _ := strings.Cut(call, " ")
	return name
}

// tempRoot is where a bench keeps its store: a temporary directory in
// SCALE_DIR when that is set, which is how to take the disk out of a
// measurement by pointing it at a RAM disk, and in the default one otherwise.
func tempRoot(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("SCALE_DIR")
	if dir == "" {
		return t.TempDir()
	}
	root, err := os.MkdirTemp(dir, "pco-scale-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// inodes lists the files below root with the inode each has. The store
// replaces a file through a new one, so a file that was written has a new
// inode; unlike a modification time, that holds on a disk that keeps seconds.
func inodes(t *testing.T, root string) map[string]uint64 {
	t.Helper()
	out := map[string]uint64{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			out[path] = st.Ino
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

// written counts the files that are new or have another inode than before.
func written(before, after map[string]uint64) int {
	n := 0
	for path, ino := range after {
		if was, ok := before[path]; !ok || was != ino {
			n++
		}
	}
	return n
}
