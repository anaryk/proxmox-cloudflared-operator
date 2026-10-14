//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// place is where pco runs: on the node itself, in the appliance, which the
// node reaches with pct exec, or on another node of a cluster, which it
// reaches with ssh as root, as the nodes of a cluster reach each other. What concerns pco's side goes through it:
// the pco command line, the units of pco and its connectors, nftables, the
// neighbour table pco watches and the files under /var/lib/pco. What concerns
// the node, pct, pvesh, pveum and pveproxy, does not.
type place struct {
	// vmid is the appliance's, 0 on the node.
	vmid int
	// node and remote are the name and the address of another node of the
	// cluster; remote is empty for the node the suite runs on.
	node, remote string
	// pco is the path of pco there.
	pco string
	// env is what a pco command there is run with: the override of the
	// Cloudflare API in the appliance, which the daemon has from its
	// drop-in and the command line from here.
	env []string
}

// argv is the command line that runs name with args where pco runs. In the
// appliance pct exec passes none of the node's environment, and neither does
// ssh to another node.
func (p place) argv(name string, args ...string) []string {
	cmd := append([]string{name}, args...)
	if p.vmid != 0 {
		cmd = append([]string{"pct", "exec", strconv.Itoa(p.vmid), "--keep-env", "0", "--"}, cmd...)
	}
	if p.remote == "" {
		return cmd
	}
	return append(sshArgv(p.node, p.remote), shellLine(cmd))
}

// pcoArgv is the command line of a pco command where pco runs.
func (p place) pcoArgv(args ...string) []string {
	if p.local() || len(p.env) == 0 {
		return p.argv(p.pco, args...)
	}
	return p.argv("env", append(append(append([]string{}, p.env...), p.pco), args...)...)
}

// local says whether the place is the node the suite runs on itself.
func (p place) local() bool { return p.vmid == 0 && p.remote == "" }

// sshArgv is ssh to root on another node of the cluster. The host key is
// looked up under the node's name, which the cluster records it by, whatever
// address the node is reached at.
func sshArgv(node, addr string) []string {
	return []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4", "-o", "HostKeyAlias=" + node, "-o", "LogLevel=ERROR", "root@" + addr, "--"}
}

// shellLine is a command line as the remote shell reads it back into the
// same words.
func shellLine(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
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
	if s.at.local() {
		raw, err := os.ReadFile(path)
		return string(raw), err
	}
	return s.here("cat", path)
}
