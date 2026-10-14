package pve

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// exitError is the error of a command that ran and failed, as exec.ExitError
// tells it.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return fmt.Sprintf("pvesh: exit status %d: %s", e.code, e.msg) }
func (e exitError) ExitCode() int { return e.code }

func exited(code int, msg string) error { return fmt.Errorf("running pvesh: %w", exitError{code, msg}) }

type fakeRunner struct {
	calls [][]string
	out   map[string]string
	err   map[string]error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	key := strings.Join(call, " ")
	return f.out[key], f.err[key]
}

func TestALocalClientReadsTheNodesOwnWithPvesh(t *testing.T) {
	run := &fakeRunner{out: map[string]string{
		"pvesh get /nodes/pve1/lxc/101/config --output-format json --current 1": `{"net0":"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01","digest":"abc"}`,
		"pvesh get /nodes/pve1/lxc/101/interfaces --output-format json":         `[{"name":"eth0","hwaddr":"bc:24:11:00:00:01","inet":"10.0.0.5/24"}]`,
	}}
	c, err := NewLocal(run, "pve1")
	require.NoError(t, err)

	cfg, err := c.GuestConfig(context.Background(), "pve1", model.GuestRef{Kind: model.KindLXC, VMID: 101})
	require.NoError(t, err)
	require.Equal(t, "abc", cfg.Digest)
	require.Contains(t, cfg.Values["net0"], "bridge=vmbr0")

	ifaces, err := c.LXCInterfaces(context.Background(), "pve1", 101)
	require.NoError(t, err)
	require.Len(t, ifaces, 1)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.0.5")}, ifaces[0].Addrs)
}

func TestALocalClientSaysWhatPveshSaid(t *testing.T) {
	run := &fakeRunner{err: map[string]error{
		"pvesh get /nodes/pve1/qemu/110/agent/network-get-interfaces --output-format json": exited(255, "No QEMU guest agent configured"),
		"pvesh get /nodes/pve1/network --output-format json":                               exited(2, "ipcc_send_rec failed"),
	}}
	c, err := NewLocal(run, "pve1")
	require.NoError(t, err)

	_, err = c.AgentInterfaces(context.Background(), "pve1", 110)
	require.ErrorIs(t, err, ErrAgentUnavailable)

	_, err = c.NodeNetwork(context.Background(), "pve1")
	require.ErrorContains(t, err, "ipcc_send_rec failed")
	require.False(t, Unreachable(err), "pvesh answered")
}

func TestALocalClientReadsNothingOfAnotherNode(t *testing.T) {
	run := &fakeRunner{}
	c, err := NewLocal(run, "pve1")
	require.NoError(t, err)

	_, err = c.NodeNetwork(context.Background(), "pve2")
	require.ErrorContains(t, err, "not of this node")
	_, err = c.ClusterStatus(context.Background())
	require.ErrorContains(t, err, "not of this node")
	require.Empty(t, run.calls)

	_, err = NewLocal(run, "../pve1")
	require.Error(t, err)
}

func TestUnreachable(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"pveproxy could not reach the node", fmt.Errorf("fetching: %w", &APIError{Status: 595, Message: "Connection refused"}), true},
		{"a gateway", &APIError{Status: 502, Message: "Bad Gateway"}, true},
		{"no connection", &url.Error{Op: "Get", URL: "https://127.0.0.1:8006", Err: refused}, true},
		{"no answer in time", fmt.Errorf("no answer within its share of the deadline: %w", context.DeadlineExceeded), true},
		{"a connection cut", &url.Error{Op: "Get", URL: "https://127.0.0.1:8006", Err: io.ErrUnexpectedEOF}, true},
		{"a rejected token", &APIError{Status: 401, Message: "invalid token"}, false},
		{"an answer of the handler", &APIError{Status: 500, Message: "does not exist"}, false},
		{"the caller gave up", fmt.Errorf("fetching: %w", context.Canceled), false},
		{"a certificate that does not verify", &url.Error{Op: "Get", URL: "https://127.0.0.1:8006", Err: x509.UnknownAuthorityError{}}, false},
		{"an answer that does not decode", fmt.Errorf("decoding response: %w", &json.SyntaxError{}), false},
		{"anything else", errors.New("invalid node name"), false},
	} {
		t.Run(tt.name, func(t *testing.T) { require.Equal(t, tt.want, Unreachable(tt.err)) })
	}
}

// A pvesh that did not run is not started again for every read: for a while
// each read says why the last one failed.
func TestAPveshThatDoesNotRunIsLeftAloneForAWhile(t *testing.T) {
	const key = "pvesh get /nodes/pve1/network --output-format json"
	run := &fakeRunner{err: map[string]error{key: errors.New("exec: \"pvesh\": executable file not found in $PATH")}}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	c, err := newLocal(run, "pve1", func() time.Time { return now })
	require.NoError(t, err)

	_, err = c.NodeNetwork(context.Background(), "pve1")
	require.ErrorContains(t, err, "executable file not found")
	_, err = c.NodeNetwork(context.Background(), "pve1")
	require.ErrorContains(t, err, "not run again until")
	require.ErrorContains(t, err, "executable file not found")
	require.Len(t, run.calls, 1)

	now = now.Add(pveshBackoff)
	run.err[key] = nil
	run.out = map[string]string{key: `[]`}
	_, err = c.NodeNetwork(context.Background(), "pve1")
	require.NoError(t, err)
	require.Len(t, run.calls, 2)
}

// A pvesh that ran and failed answered: the next read runs it again.
func TestAPveshThatAnsweredWithAnErrorIsAskedAgain(t *testing.T) {
	const key = "pvesh get /nodes/pve1/network --output-format json"
	run := &fakeRunner{err: map[string]error{key: exited(2, "ipcc_send_rec failed")}}
	c, err := NewLocal(run, "pve1")
	require.NoError(t, err)
	for range 2 {
		_, err = c.NodeNetwork(context.Background(), "pve1")
		require.ErrorContains(t, err, "ipcc_send_rec failed")
	}
	require.Len(t, run.calls, 2)
}

func TestTheRunnerOfPveshBoundsWhatItKeeps(t *testing.T) {
	run := execRunner{path: "/bin/sh", limit: 1024}
	out, err := run.Run(context.Background(), "pvesh", "-c", "printf '[1]'")
	require.NoError(t, err)
	require.Equal(t, "[1]", out)

	_, err = run.Run(context.Background(), "pvesh", "-c", "head -c 4096 /dev/zero")
	require.ErrorContains(t, err, "printed more than 1024 bytes")

	_, err = run.Run(context.Background(), "pvesh", "-c", "echo 'no such guest' >&2; exit 2")
	var exit interface{ ExitCode() int }
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 2, exit.ExitCode())
	require.ErrorContains(t, err, "no such guest")
}
