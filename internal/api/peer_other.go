//go:build !linux

package api

import (
	"context"
	"net"
)

// enforcePeers is false off Linux, where the peer credentials are not read:
// every connection is accepted. That is for development only; the daemon runs
// on Proxmox VE.
const enforcePeers = false

func connContext(ctx context.Context, _ net.Conn) context.Context { return ctx }
