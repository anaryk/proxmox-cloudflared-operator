package managednet

import (
	"context"
	"fmt"
	"os/exec"
)

// iptablesSave is where Proxmox nodes have iptables-save.
const iptablesSave = "/usr/sbin/iptables-save"

// ReadLiveChains reads the filter table of the node's kernel through
// iptables-save, which needs root: the rules pve-firewall installed, not
// those it was configured with.
func ReadLiveChains(ctx context.Context) (LiveChains, error) {
	out, err := exec.CommandContext(ctx, iptablesSave, "-t", "filter").Output()
	if err != nil {
		return nil, fmt.Errorf("running iptables-save: %w", err)
	}
	return ParseIPTablesSave(string(out)), nil
}
