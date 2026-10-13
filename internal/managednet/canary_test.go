package managednet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCanaryPortIsTheHighestUnpublishedBelow1024(t *testing.T) {
	require.Equal(t, uint16(1023), CanaryPort(nil, 0))
	require.Equal(t, uint16(1022), CanaryPort([]uint16{443, 1023}, 0))
	require.Equal(t, uint16(1023), CanaryPort([]uint16{443}, 1023), "the recorded port stays")
	require.Equal(t, uint16(1022), CanaryPort([]uint16{1023}, 1023), "a route that publishes it makes another one")
	require.Equal(t, uint16(900), CanaryPort([]uint16{443}, 900), "the recorded port stays while unpublished")
	require.Equal(t, uint16(1023), CanaryPort(nil, 68), "never a DHCP port")
	require.Equal(t, uint16(1023), CanaryPort(nil, 8080), "never above 1023")

	var all []uint16
	for p := uint16(1); p < 1024; p++ {
		if p != 67 && p != 68 {
			all = append(all, p)
		}
	}
	require.Zero(t, CanaryPort(all, 0))
}

// dialFunc answers the canary as a guest or the network would.
func dialFunc(conn net.Conn, err error) Dialer {
	return func(ctx context.Context, _ netip.AddrPort) (net.Conn, error) {
		if errors.Is(err, errSilence) {
			<-ctx.Done()
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", ctx.Err())}
		}
		return conn, err
	}
}

var errSilence = errors.New("silence")

func TestTheCanary(t *testing.T) {
	addr := netip.MustParseAddrPort("10.203.118.2:1023")
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	for _, c := range []struct {
		name    string
		dial    Dialer
		verdict string
		detail  string
	}{
		{"silence passes", dialFunc(nil, errSilence), CanarySilent, ""},
		{"a reset is an answer", dialFunc(nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), CanaryAnswered, "the connection was reset"},
		{"a SYN-ACK is an answer", dialFunc(client, nil), CanaryAnswered, "the connection was accepted"},
		{"a timeout of the socket is silence", dialFunc(nil, &net.OpError{Op: "dial", Err: timeoutErr{}}), CanarySilent, ""},
		{"the leg is gone", dialFunc(nil, errors.New("the leg of pconet is not up")), CanaryNotSent, "the leg of pconet is not up"},
		{"no route", dialFunc(nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}), CanaryNotSent, "dial: connect: network is unreachable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Canary(t.Context(), c.dial, addr, 20*time.Millisecond)
			require.Equal(t, CanaryResult{Port: 1023, Verdict: c.verdict, Detail: c.detail}, got)
		})
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got := Canary(ctx, dialFunc(nil, errSilence), addr, time.Second)
	require.Equal(t, CanaryNotSent, got.Verdict, "a cycle that ends proves nothing")
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
