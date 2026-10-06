package main

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func someManualRoutes() []engine.ManualRouteView {
	return []engine.ManualRouteView{
		{
			ID: "status", Rev: 2, Hostname: "status.example.com",
			Target: engine.ManualTarget{Kind: "address", Scheme: "http", Addr: netip.MustParseAddr("10.0.5.20"), Port: 9000},
		},
		{
			ID: "wiki", Rev: 1, Hostname: "wiki.example.com",
			Target:  engine.ManualTarget{Kind: "guest", Guest: "qemu/101", Scheme: "https", Port: 8443},
			Options: model.RouteOptions{NoTLSVerify: true, Via: "net1"},
		},
	}
}

func daemonWithManualRoutes(t *testing.T) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: healthyState(), manual: someManualRoutes()}
	return newRunner(t, serveFake(t, e)), e
}

func TestRouteManualListGolden(t *testing.T) {
	r, e := daemonWithManualRoutes(t)

	res := r.run("", "route", "manual", "list")

	require.NoError(t, res.err)
	requireGolden(t, "route_manual_list.golden", res.out)
	require.Empty(t, e.called())

	r, _ = daemonWith(t, healthyState())
	res = r.run("", "route", "manual", "list")
	require.NoError(t, res.err)
	require.Equal(t, "No manual routes.\n", res.out)
}

func TestRouteManualListJSONIsWhatTheDaemonSent(t *testing.T) {
	const raw = `[{"id":"status","rev":2,"hostname":"status.example.com","target":{"kind":"address","scheme":"http","addr":"10.0.5.20","port":9000},"options":{}}]`
	r := newRunner(t, serveRaw(t, map[string]rawReply{"GET /v1/routes/manual": {200, raw}}))

	res := r.run("", "--json", "route", "manual", "list")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, raw), res.out)
}

func TestRouteManualAdd(t *testing.T) {
	for _, tt := range []struct {
		name, call, out string
		args            []string
	}{
		{
			"to an address", `add manual {"id":"status","rev":0,"hostname":"status.example.com",` +
				`"target":{"kind":"address","scheme":"http","addr":"10.0.5.20","port":9000},"options":{}}`,
			"Made manual route manual/status: status.example.com -> http://10.0.5.20:9000.\n",
			[]string{"status.example.com", "--address", "10.0.5.20", "--port", "9000", "--id", "status"},
		},
		{
			"to a guest, with options", `add manual {"id":"","rev":0,"hostname":"wiki.example.com",` +
				`"target":{"kind":"guest","guest":"qemu/101","scheme":"https","port":8443},` +
				`"options":{"noTLSVerify":true,"hostHeader":"wiki","sni":"wiki.example.com","via":"net1"}}`,
			"Made manual route manual/1a2b3c4d: wiki.example.com -> https://qemu/101:8443 " +
				"(no-tls-verify, host-header=wiki, sni=wiki.example.com, via=net1).\n",
			[]string{"wiki.example.com", "--guest", "qemu/101", "--port", "8443", "--https", "--no-tls-verify",
				"--host-header", "wiki", "--sni", "wiki.example.com", "--via", "net1"},
		},
		{
			"to a service of the node", `add manual {"id":"","rev":0,"hostname":"pve.example.com",` +
				`"target":{"kind":"address","scheme":"https","addr":"10.0.0.2","port":8006},"options":{"noTLSVerify":true,"allowNode":true}}`,
			"Made manual route manual/1a2b3c4d: pve.example.com -> https://10.0.0.2:8006 (no-tls-verify, allow-node).\n",
			[]string{"pve.example.com", "--address", "10.0.0.2", "--port", "8006", "--https", "--no-tls-verify", "--allow-node"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, healthyState())

			res := r.run("", append([]string{"route", "manual", "add"}, tt.args...)...)

			require.NoError(t, res.err)
			require.Equal(t, []string{tt.call}, e.called())
			require.Equal(t, tt.out+"It is published from the next cycle; pco routes shows how it fares.\n", res.out)
		})
	}
}

func TestRouteManualAddJSON(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "--json", "route", "manual", "add", "status.example.com", "--address", "10.0.5.20", "--port", "9000", "--id", "status")

	require.NoError(t, res.err)
	require.Equal(t, indented(t, `{"id":"status","rev":1,"hostname":"status.example.com",`+
		`"target":{"kind":"address","scheme":"http","addr":"10.0.5.20","port":9000},"options":{}}`), res.out)
}

func TestRouteManualAddNeedsOneTargetAndAPort(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no target", []string{"--port", "80"}, "name the target with --guest or --address, one of them"},
		{"two targets", []string{"--guest", "qemu/101", "--address", "10.0.5.20", "--port", "80"}, "name the target with --guest or --address, one of them"},
		{"no port", []string{"--guest", "qemu/101"}, "--port is needed: the port of the service, from 1 to 65535"},
		{"an address that is none", []string{"--address", "nas", "--port", "80"}, `--address "nas": want an IPv4 address`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, healthyState())

			res := r.run("", append([]string{"route", "manual", "add", "x.example.com"}, tt.args...)...)

			require.EqualError(t, res.err, tt.want)
			require.Empty(t, e.called())
		})
	}
}

func TestRouteManualAddRefusedByTheDaemon(t *testing.T) {
	r, e := daemonWith(t, healthyState())
	e.configErr = &engine.FieldError{Field: "target.addr",
		Err: errors.New("target.addr 192.168.1.10: not inside the manualCIDRs of the settings (10.0.5.0/24)")}

	res := r.run("", "route", "manual", "add", "x.example.com", "--address", "192.168.1.10", "--port", "80")

	require.EqualError(t, res.err, "target.addr 192.168.1.10: not inside the manualCIDRs of the settings (10.0.5.0/24)")
	require.Empty(t, res.out)
}

func TestRouteManualRemove(t *testing.T) {
	const shown = "manual/status publishes status.example.com -> http://10.0.5.20:9000 (revision 2).\n" +
		"Removing it stops publishing status.example.com from the next cycle.\n"

	t.Run("with --yes", func(t *testing.T) {
		r, e := daemonWithManualRoutes(t)

		res := r.runReader(unreadable{t}, "route", "manual", "remove", "status", "--yes")

		require.NoError(t, res.err)
		require.Equal(t, shown+"Removed manual route manual/status.\n", res.out)
		require.Equal(t, []string{"remove manual status rev=2"}, e.called(), "at the revision shown")
	})

	t.Run("answered at the terminal", func(t *testing.T) {
		r, e := daemonWithManualRoutes(t)

		res := r.tty().run("y\n", "route", "manual", "remove", "status")

		require.NoError(t, res.err)
		require.Equal(t, "Remove manual route manual/status? [y/N] ", res.errOut)
		require.Equal(t, []string{"remove manual status rev=2"}, e.called())
	})

	t.Run("not confirmed", func(t *testing.T) {
		r, e := daemonWithManualRoutes(t)

		res := r.tty().run("n\n", "route", "manual", "remove", "status")

		require.ErrorIs(t, res.err, errAborted)
		require.Equal(t, shown, res.out)
		require.Empty(t, e.called())
	})

	t.Run("without a terminal", func(t *testing.T) {
		r, e := daemonWithManualRoutes(t)

		res := r.run("y\n", "route", "manual", "remove", "status")

		require.ErrorIs(t, res.err, errNoTerminal)
		require.Empty(t, e.called())
	})

	t.Run("a route that is not there", func(t *testing.T) {
		r, e := daemonWithManualRoutes(t)

		res := r.run("", "route", "manual", "remove", "nope", "--yes")

		require.EqualError(t, res.err, "there is no manual route manual/nope: pco route manual list shows them")
		require.Empty(t, e.called())
	})

	t.Run("one that changed since", func(t *testing.T) {
		r, e := daemonWithManualRoutes(t)
		e.configErr = fmt.Errorf("%w: the manual route manual/status changed since it was read at revision 2; read it again", engine.ErrRefused)

		res := r.run("", "route", "manual", "remove", "status", "--yes")

		require.EqualError(t, res.err, "refused: the manual route manual/status changed since it was read at revision 2; read it again")
	})
}
