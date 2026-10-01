//go:build !linux

package api

import (
	"context"
	"net"
)

// peerChecks is false off Linux, where the peer credentials are not read:
// every connection is accepted. That is for development only; the daemon runs
// on Proxmox VE, and Serve says so in the log.
const peerChecks = false

func connContext(ctx context.Context, _ net.Conn) context.Context { return ctx }
