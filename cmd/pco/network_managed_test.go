package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// runManaged runs pco network managed with args, as the command will hang
// below pco network, on a daemon at socket.
func runManaged(t *testing.T, e env, socket string, args ...string) result {
	t.Helper()
	a := &app{env: e, socket: socket}
	cmd := newNetworkManagedCmd(a)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

func applianceMachine(t *testing.T) env {
	e := testEnv()
	e.profileFile = filepath.Join(t.TempDir(), "profile")
	require.NoError(t, os.WriteFile(e.profileFile, []byte("appliance\n"), 0o600))
	return e
}

// Inside the appliance the node made the vnet and granted it; the daemon
// takes the network into its store.
func TestNetworkManagedEnableInTheApplianceAsksTheDaemon(t *testing.T) {
	socket := serveRaw(t, map[string]rawReply{"POST /v1/managed/enable": {status: http.StatusOK, body: `{}`}})

	res := runManaged(t, applianceMachine(t), socket, "enable")

	require.NoError(t, res.err)
	require.Equal(t, "The managed network is enabled: the appliance puts its card on pconet in its next cycle.\n", res.out)
}

func TestNetworkManagedEnableInTheApplianceSaysWhatTheDaemonSays(t *testing.T) {
	socket := serveRaw(t, map[string]rawReply{"POST /v1/managed/enable": {status: http.StatusConflict,
		body: `{"error":"the vnet pconet is not there: run pco appliance managed enable on the node first","code":"refused"}`}})

	res := runManaged(t, applianceMachine(t), socket, "enable")

	require.EqualError(t, res.err, "the vnet pconet is not there: run pco appliance managed enable on the node first")
}

// A daemon of an older version does not know the request, which is said.
func TestNetworkManagedEnableOnAnOlderDaemon(t *testing.T) {
	socket := serveRaw(t, map[string]rawReply{})

	res := runManaged(t, applianceMachine(t), socket, "enable")

	require.ErrorContains(t, res.err, "the daemon does not know this command")
}

// In the appliance the node applies the SDN, so whether to apply the
// changes of others is the node's to say; --yes asks nothing there either.
func TestNetworkManagedEnableInTheApplianceRefusesTheFlagOfTheNode(t *testing.T) {
	res := runManaged(t, applianceMachine(t), "/nonexistent/pco/pco.sock", "enable", "--confirm-sdn-apply")

	require.EqualError(t, res.err, "--confirm-sdn-apply is for a host: in the appliance the node makes and applies the SDN "+
		"with pco appliance managed enable")

	socket := serveRaw(t, map[string]rawReply{"POST /v1/managed/enable": {status: http.StatusOK, body: `{}`}})
	require.NoError(t, runManaged(t, applianceMachine(t), socket, "enable", "--yes").err)
}

// On a host the SDN apply asks, which needs a terminal or --yes.
func TestNetworkManagedEnableOnTheHostNeedsATerminal(t *testing.T) {
	res := runManaged(t, testEnv(), "/nonexistent/pco/pco.sock", "enable")

	require.ErrorIs(t, res.err, errNoTerminal)
}

func TestNetworkManagedEnableRefusesJSON(t *testing.T) {
	e := testEnv()
	a := &app{env: e, json: true}
	cmd := newNetworkManagedCmd(a)
	cmd.SetArgs([]string{"enable"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "--json has no meaning")
}

func TestNetworkManagedCommandsAreDocumented(t *testing.T) {
	cmd := newNetworkManagedCmd(&app{env: testEnv()})
	for _, c := range append([]*cobra.Command{cmd}, cmd.Commands()...) {
		require.NotEmpty(t, c.Long, c.Name())
		require.NotEmpty(t, c.Example, c.Name())
		lines := strings.Split(c.Example, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "pco ") {
				require.True(t, strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#"), "%s: %q has no comment above it", c.Name(), line)
			}
		}
	}
	require.Equal(t, "managed", cmd.Name())
	enable, _, err := cmd.Find([]string{"enable"})
	require.NoError(t, err)
	require.NotNil(t, enable.Flags().Lookup("confirm-sdn-apply"))
	require.NotNil(t, enable.Flags().Lookup("yes"))
}
