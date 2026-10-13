//go:build linux

package gateway

import (
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"
)

// NewHostAt returns the host backend of a gateway namespace pinned under
// name, beside hostNS, which plays the host. A node that a test builds of
// namespaces of its own runs the gateway with it, nothing of the machine's.
func NewHostAt(name string, hostNS netns.NsHandle, hostNft Nft, log zerolog.Logger) Backend {
	return newHost(newLinuxSystem(name, hostNS), NewNamespaceNft(name), hostNft, log, time.Now)
}
