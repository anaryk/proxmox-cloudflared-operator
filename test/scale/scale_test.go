//go:build scale

package scale

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The sizes of the design: guests in the inventory and, of them, the routes
// that are published.
var sizes = []struct{ guests, routes int }{
	{200, 100},
	{1000, 500},
	{2000, 1000},
	{5000, 2000},
}

const (
	// grace is a little more than the removal grace of the settings, which is a
	// minute: a clock that moves by it makes a removal due in the next cycle.
	grace = 61 * time.Second

	// resolveConcurrency is the number of routes the engine resolves at once.
	resolveConcurrency = 32

	// recheck is the default reverifyInterval: in every one of them, each
	// address is proven again once, at a time of its own.
	recheck = time.Minute

	// realARP is the window the real prober waits for the answers to every ARP
	// request, and shortARP a latency that checks the model at little cost.
	shortARP = 20 * time.Millisecond
	realARP  = 600 * time.Millisecond
)

// row is one cycle of the table.
type row struct {
	size     string
	scenario string
	res      result
	note     string
}

func TestScale(t *testing.T) {
	var rows []row
	for _, sz := range sizes {
		t.Run(fmt.Sprintf("%d guests %d routes", sz.guests, sz.routes), func(t *testing.T) {
			rows = append(rows, runSize(t, sz.guests, sz.routes)...)
		})
	}
	printCycles(rows)
}

func runSize(t *testing.T, guests, routes int) []row {
	b := newBench(t, guests, routes, unlimited)
	size := fmt.Sprintf("%d/%d", guests, routes)
	var rows []row
	add := func(scenario, note string, res result) {
		rows = append(rows, row{size: size, scenario: scenario, res: res, note: note})
	}

	first := b.cycle()
	require.Empty(t, first.state.Problems)
	require.Equal(t, routes, b.records())
	add("first enforcing cycle", "", first)

	b.prober.proven()
	second := b.cycle()
	require.Zero(t, second.calls.writes())
	require.Empty(t, second.state.Problems)
	add("second cycle", fmt.Sprintf("proves again: %d proven", b.prober.proven()), second)

	idle := b.cycle()
	require.Zero(t, idle.calls.writes())
	require.Empty(t, idle.state.Problems)
	add("idle", fmt.Sprintf("%d proven", b.prober.proven()), idle)

	for _, latency := range arpLatencies(routes) {
		model := func(proven int) string {
			m := time.Duration((proven+resolveConcurrency-1)/resolveConcurrency) * latency
			return fmt.Sprintf("%d proven, model %s", proven, m.Round(10*time.Millisecond))
		}
		b.prober.setARPLatency(latency)
		b.prober.proven()
		var busiest result
		var most int
		for range recheck / pollEvery {
			res := b.cycle()
			if n := b.prober.proven(); res.wall > busiest.wall {
				busiest, most = res, n
			}
		}
		add(fmt.Sprintf("busiest of a minute, ARP %s", latency), model(most), busiest)
		b.eng.Watching(false)
		res := b.cycle()
		add(fmt.Sprintf("no watch, ARP %s", latency), model(b.prober.proven()), res)
		b.eng.Watching(true)
		b.prober.setARPLatency(0)
		b.cycle()
	}

	renamed := routes / 10
	for i := 0; len(b.renamed) < renamed; i += 10 {
		b.renamed[i] = true
	}
	b.publish()
	change := b.cycle()
	require.Empty(t, change.state.Problems)
	require.Equal(t, routes+renamed, b.records())
	add("10% of routes renamed", "", change)
	removed, n := b.untilRemoved(t, routes)
	add("... old names removed", fmt.Sprintf("%d cycles later", n), removed)

	vanished := max(1, guests/100)
	stride := routes / vanished
	for j := range vanished {
		b.gone[j*stride+1] = true
	}
	b.publish()
	gone := b.cycle()
	require.Empty(t, gone.state.Problems)
	require.Equal(t, routes, b.records())
	add(fmt.Sprintf("1%% of guests vanish (%d)", vanished), "", gone)
	removed, n = b.untilRemoved(t, routes-vanished)
	add("... their records removed", fmt.Sprintf("%d cycles later", n), removed)
	return rows
}

// arpLatencies are the ARP latencies an idle cycle is run with. A cycle that
// proves every address takes routes/32 times the latency more, as the model of
// the table says, so the real window is run only up to the routes of the
// design, where it fits the time of the whole run; the larger size follows
// from the model.
func arpLatencies(routes int) []time.Duration {
	if routes <= 1000 {
		return []time.Duration{shortARP, realARP}
	}
	return nil
}

// untilRemoved runs cycles a grace apart until one deletes records, and
// returns it with the number of cycles it took. The zone must hold want
// records after it.
func (b *bench) untilRemoved(t *testing.T, want int) (result, int) {
	t.Helper()
	for n := 1; n <= 6; n++ {
		res := b.after(grace)
		require.Empty(t, res.state.Problems)
		if res.calls.dnsWrite > 0 {
			require.Equal(t, want, b.records())
			return res, n
		}
	}
	require.FailNow(t, "the records were not removed", "want %d, have %d", want, b.records())
	return result{}, 0
}

// TestDiskCost times what the store does for every file it writes, so that the
// files of a cycle can be priced for another disk: a cycle writes one for every
// binding it renews.
func TestDiskCost(t *testing.T) {
	const files = 200
	root := tempRoot(t)
	data := []byte(strings.Repeat("x", 300))
	began := time.Now()
	for i := range files {
		f, err := os.CreateTemp(root, ".file-*.tmp")
		require.NoError(t, err)
		_, err = f.Write(data)
		require.NoError(t, err)
		require.NoError(t, f.Sync())
		require.NoError(t, f.Close())
		require.NoError(t, os.Rename(f.Name(), filepath.Join(root, fmt.Sprintf("file-%d.json", i))))
	}
	fmt.Printf("disk: %s for a file written as the store does (write, fsync, rename)\n",
		(time.Since(began) / files).Round(10*time.Microsecond))
}

func printCycles(rows []row) {
	tab := newTable()
	tab.line("guests/routes\tcycle\twall\tCF calls\tdns r/w\ttunnel r/w\tother\tleast at 1000/5m\talloc MB\tallocs k\tpeak heap MB\tfiles written\tnote")
	for _, r := range rows {
		c := r.res.calls
		tab.line("%s\t%s\t%s\t%d\t%d/%d\t%d/%d\t%d\t%s\t%.0f\t%d\t%.0f\t%d\t%s",
			r.size, r.scenario, r.res.wall.Round(10*time.Millisecond), c.total(),
			c.dnsRead, c.dnsWrite, c.tunnelRead, c.tunnelWrite, c.other, budgetFloor(c.total()),
			float64(r.res.alloc)/1e6, r.res.mallocs/1000, float64(r.res.peak)/1e6, r.res.files, r.note)
	}
	tab.flush()
}
