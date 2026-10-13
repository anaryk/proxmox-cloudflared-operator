package managednet_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// The uninstall removes the rules whose comment starts as setup says; the
// appliance writes them so.
func TestTheRulesCarryTheCommentTheUninstallRemoves(t *testing.T) {
	require.Equal(t, setup.FirewallComment, managednet.FirewallCommentPrefix)
	require.True(t, strings.HasPrefix(managednet.FirewallComment("abc"), setup.FirewallComment))
}
