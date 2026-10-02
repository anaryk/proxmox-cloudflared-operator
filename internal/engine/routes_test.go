package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A route that won its hostname carries the rule the tunnel of its account
// has for it, whether that serves it or answers 503; one that lost it carries
// none.
func TestARouteCarriesTheRuleOfItsHostname(t *testing.T) {
	e := contested(t)
	e.res.setUnreachable("api.example.com", "dial tcp 10.0.0.11:3000: connection refused")
	e.res.reject("bad.example.com", "address of this node")
	e.inv.set(snapshot(webOne, webTwo,
		guest(103, "api", "api.example.com -> :3000"),
		guest(104, "bad", "bad.example.com -> :80")))
	e.clock.advance(20 * time.Second)

	st := e.cycle()

	require.Equal(t, testAccount, wwwRoute(st, "qemu/101").Account)
	require.Equal(t, &planner.IngressRule{Hostname: "www.example.com", Service: "http://10.0.0.11:8080"}, wwwRoute(st, "qemu/101").Rule)
	loser := wwwRoute(st, "qemu/102")
	require.Empty(t, loser.Account)
	require.Nil(t, loser.Rule)
	unreachable := route(st, "api.example.com")
	require.Equal(t, planner.StateUnreachable, unreachable.State)
	require.Equal(t, &planner.IngressRule{Hostname: "api.example.com", Service: "http://10.0.0.11:3000"}, unreachable.Rule)
	rejected := route(st, "bad.example.com")
	require.Equal(t, testAccount, rejected.Account)
	require.Equal(t, &planner.IngressRule{Hostname: "bad.example.com", Service: "http_status:503"}, rejected.Rule)
}

func TestTheEngineSaysItsPollInterval(t *testing.T) {
	e := newEnv(t)
	require.Equal(t, 10*time.Second, e.eng.PollInterval())

	e.settings(func(s *store.Settings) { s.PollInterval = store.Duration(30 * time.Second) })
	e.cycle()

	require.Equal(t, 30*time.Second, e.eng.PollInterval())
}
