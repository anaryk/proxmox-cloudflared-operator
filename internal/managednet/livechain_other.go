//go:build !linux

package managednet

import (
	"context"
	"errors"
)

// ReadLiveChains reads the node's live firewall rules, which only a Proxmox
// node, a Linux system, has.
func ReadLiveChains(context.Context) (LiveChains, error) {
	return nil, errors.New("the live firewall rules are read on a Proxmox node only")
}
