//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// place is where pco runs: on the node itself, or in the appliance, which
// the node reaches with pct exec. What concerns pco's side goes through it:
// the pco command line, the units of pco and its connectors, nftables, the
// neighbour table pco watches and the files under /var/lib/pco. What concerns
// the node, pct, pvesh, pveum and pveproxy, does not.
type place struct {
	// vmid is the appliance's, 0 on the node.
	vmid int
	// pco is the path of pco there.
	pco string
	// env is what a pco command there is run with: the override of the
	// Cloudflare API in the appliance, which the daemon has from its
	// drop-in and the command line from here.
	env []string
}

// argv is the command line that runs name with args where pco runs. In the
// appliance pct exec passes none of the node's environment.
func (p place) argv(name string, args ...string) []string {
	if p.vmid == 0 {
		return append([]string{name}, args...)
	}
	return append([]string{"pct", "exec", strconv.Itoa(p.vmid), "--keep-env", "0", "--", name}, args...)
}

// pcoArgv is the command line of a pco command where pco runs.
func (p place) pcoArgv(args ...string) []string {
	if p.vmid == 0 || len(p.env) == 0 {
		return p.argv(p.pco, args...)
	}
	return p.argv("env", append(append(append([]string{}, p.env...), p.pco), args...)...)
}

// here runs a command where pco runs and returns its stdout.
func (s *suite) here(name string, args ...string) (string, error) {
	argv := s.at.argv(name, args...)
	return s.run(argv[0], argv[1:]...)
}

func (s *suite) hereMust(t testing.TB, name string, args ...string) string {
	t.Helper()
	out, err := s.here(name, args...)
	require.NoError(t, err)
	return out
}

// pcoOut runs a pco command where pco runs and returns its stdout.
func (s *suite) pcoOut(args ...string) (string, error) {
	argv := s.at.pcoArgv(args...)
	return s.run(argv[0], argv[1:]...)
}

func (s *suite) pcoMust(t testing.TB, args ...string) string {
	t.Helper()
	out, err := s.pcoOut(args...)
	require.NoError(t, err)
	return out
}

// pcoCombined runs a pco command where pco runs and returns stdout and
// stderr together.
func (s *suite) pcoCombined(args ...string) (string, error) {
	argv := s.at.pcoArgv(args...)
	return s.combined(argv[0], argv[1:]...)
}

// hereCommand is a command where pco runs that the caller starts and
// stops, as a monitor that runs alongside a scenario.
func (s *suite) hereCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	argv := s.at.argv(name, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = s.childEnv()
	return cmd
}

// readHere reads a file where pco runs.
func (s *suite) readHere(path string) (string, error) {
	if s.at.vmid == 0 {
		raw, err := os.ReadFile(path)
		return string(raw), err
	}
	return s.here("cat", path)
}
