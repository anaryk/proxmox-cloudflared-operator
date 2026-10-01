package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

var takeover = planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "n2"}

// dnsCalls counts the calls the DNS reconciler makes.
func dnsCalls(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, "Record") {
			n++
		}
	}
	return n
}

func TestStaleWriterVerdictStopsBeforeDNS(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	var once sync.Once
	e.useAPI(testToken, hookedAPI{API: e.cf, before: func(method string) {
		if method == "FindTunnel" {
			// Another writer takes over while the tunnel run looks.
			once.Do(func() { require.NoError(t, e.store.SaveWriter(takeover)) })
		}
	}})

	st := e.cycle()

	require.Equal(t, "stale", st.WriterVerdict)
	require.Contains(t, strings.Join(st.Problems, "\n"), "this writer is stale and stops")
	require.Empty(t, e.writes(), "nothing written after the takeover")
	require.Zero(t, dnsCalls(e.cf.Calls()), "DNS is not looked at")
	require.Empty(t, e.conn.ensures())
	require.Empty(t, e.conn.prunes())
	require.Contains(t, e.eng.Events(time.Time{}), Event{At: t0, Level: "error", Kind: "writer", Subject: "leader.json", Message: "writer verdict is stale"})

	e.clock.advance(10 * time.Second)
	st = e.cycle()

	require.Equal(t, "ok", st.WriterVerdict, "the next cycle writes as the new writer")
	require.Empty(t, st.Problems)
	rules := e.rules()
	require.Equal(t, planner.SentinelHostname(takeover), rules[len(rules)-2].Hostname)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}

func TestWriterReplacedAfterTheTunnelRunStopsDNS(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	var once sync.Once
	e.conn.onEnsure = func() { once.Do(func() { require.NoError(t, e.store.SaveWriter(takeover)) }) }

	st := e.cycle()

	require.Equal(t, "stale", st.WriterVerdict)
	require.Len(t, e.tunnels(), 1, "the tunnel run finished before the takeover")
	require.Empty(t, e.records(), "no DNS write")
	for _, a := range st.Actions {
		if a.Kind == "create-record" {
			require.False(t, a.Applied)
		}
	}

	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Equal(t, "ok", st.WriterVerdict)
	require.Empty(t, st.Problems)
	rules := e.rules()
	require.Equal(t, planner.SentinelHostname(takeover), rules[len(rules)-2].Hostname)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}

func TestForeignWriterStopsBeforeConnectorsAndDNS(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	other := planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "other"}
	e.cf.SeedTunnel(testAccount, tunnelName, []planner.IngressRule{
		{Hostname: planner.SentinelHostname(other), Service: "http_status:404"},
		{Service: "http_status:404"},
	})

	st := e.cycle()

	require.Equal(t, "foreign", st.WriterVerdict)
	require.Empty(t, e.writes())
	require.Zero(t, dnsCalls(e.cf.Calls()))
	require.Empty(t, e.conn.ensures(), "no connector for a tunnel another installation writes")
	require.Empty(t, e.conn.prunes())
}
