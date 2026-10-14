package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// What the managed step said of writes handed an agent is said once in an
// epoch, and forgotten with it.
func TestWhatIsSaidOfAWriteIsForgottenWithItsEpoch(t *testing.T) {
	var p pveWrites
	one := agentproto.Epoch{InstallID: "i", Generation: 1, Nonce: "a"}
	two := agentproto.Epoch{InstallID: "i", Generation: 2, Nonce: "b"}

	p.note(nil, one)
	require.True(t, p.sayOnce("qemu/104 written"))
	require.False(t, p.sayOnce("qemu/104 written"))
	p.note(nil, one)
	require.False(t, p.sayOnce("qemu/104 written"), "the same epoch")

	p.note(nil, two)
	require.Len(t, p.said, 0, "nothing of the epoch before is kept")
	require.True(t, p.sayOnce("qemu/104 written"))
}
