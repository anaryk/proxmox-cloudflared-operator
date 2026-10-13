package managednet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"time"
)

// CanaryTimeout is how long the canary waits for an answer. Silence is what
// the IN DROP of pco's rules gives.
const CanaryTimeout = time.Second

// The DHCP ports, which the canary never probes: pco's rules let the leg's
// DHCP replies in.
const (
	dhcpServerPort = 67
	dhcpClientPort = 68
)

// CanaryPort is the port the canary probes a card on: the one chosen before
// while no route of the guest publishes it, and otherwise the highest port
// below 1024 that no route publishes, the DHCP ports left out. Zero when
// every one is published.
func CanaryPort(published []uint16, prev uint16) uint16 {
	usable := func(p uint16) bool {
		return p > 0 && p < 1024 && p != dhcpServerPort && p != dhcpClientPort && !slices.Contains(published, p)
	}
	if usable(prev) {
		return prev
	}
	for p := uint16(1023); p > 0; p-- {
		if usable(p) {
			return p
		}
	}
	return 0
}

// The verdicts of a canary.
const (
	// CanarySilent: nothing answered within CanaryTimeout, as the IN DROP of
	// pco's rules makes it.
	CanarySilent = "silent"
	// CanaryAnswered: the guest answered, with a SYN-ACK or a reset: the
	// IN DROP is not in force on the card.
	CanaryAnswered = "answered"
	// CanaryNotSent: the probe could not be made, which proves nothing.
	CanaryNotSent = "not sent"
)

// CanaryResult is what one probe of a card found.
type CanaryResult struct {
	Port    uint16
	Verdict string
	Detail  string // the answer, or why the probe was not sent
}

// Dialer connects to an address the way the canary must: from the leg of
// the managed network, with its mark.
type Dialer func(ctx context.Context, addr netip.AddrPort) (net.Conn, error)

// Canary probes the card at addr with a TCP SYN from the leg, through dial,
// and waits at most timeout: silence passes, an accepted connection or a
// reset is the guest answering where pco's rules drop, and anything else
// leaves the card unproven. The firewall configuration and the state of the
// firewall service are not evidence that the rules are installed in the
// node's kernel; an answer here is.
func Canary(ctx context.Context, dial Dialer, addr netip.AddrPort, timeout time.Duration) CanaryResult {
	res := CanaryResult{Port: addr.Port()}
	probe, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := dial(probe, addr)
	var netErr net.Error
	switch {
	case err == nil:
		_ = conn.Close()
		res.Verdict, res.Detail = CanaryAnswered, "the connection was accepted"
	case errors.Is(err, syscall.ECONNREFUSED):
		res.Verdict, res.Detail = CanaryAnswered, "the connection was reset"
	case ctx.Err() != nil:
		res.Verdict, res.Detail = CanaryNotSent, ctx.Err().Error()
	case probe.Err() != nil, errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		res.Verdict = CanarySilent
	default:
		res.Verdict, res.Detail = CanaryNotSent, err.Error()
	}
	return res
}
