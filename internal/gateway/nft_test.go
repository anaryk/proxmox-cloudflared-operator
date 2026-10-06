package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeIPScript stands in for ip netns exec <name> nft: it writes its
// arguments and its input beside it and answers as the files beside it say.
const fakeIPScript = `#!/bin/sh
printf '%s\n' "$*" >> "$0.args"
if [ -e "$0.nonamespace" ]; then
	echo 'Cannot open network namespace "pco": No such file or directory' >&2
	exit 1
fi
case "$*" in
*"-f -")
	cat > "$0.stdin" ;;
*"-j list table inet pco_gw")
	if [ -e "$0.missing" ]; then
		printf '%s\n' 'Error: No such file or directory' 'list table inet pco_gw' >&2
		exit 1
	fi
	printf '%s' '{"nftables": []}' ;;
esac
`

func fakeIP(t *testing.T) (nftCmd, string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ip")
	require.NoError(t, os.WriteFile(bin, []byte(fakeIPScript), 0o755))
	return nftCmd{argv: []string{bin, "netns", "exec", "pco", nftPath}}, bin
}

func TestTheNftOfTheNamespaceRunsThroughIPNetnsExec(t *testing.T) {
	require.Equal(t, nftCmd{argv: []string{"/usr/sbin/ip", "netns", "exec", "pco", "/usr/sbin/nft"}}, NewNamespaceNft("pco"))
	require.Equal(t, nftCmd{argv: []string{"/usr/sbin/nft"}}, NewHostNft())

	n, bin := fakeIP(t)
	require.NoError(t, n.Apply(t.Context(), "add table inet pco_gw\n"))
	_, err := n.List(t.Context(), gwTable)
	require.NoError(t, err)
	args, err := os.ReadFile(bin + ".args")
	require.NoError(t, err)
	require.Equal(t, "netns exec pco /usr/sbin/nft -f -\nnetns exec pco /usr/sbin/nft -j list table inet pco_gw\n", string(args))
	stdin, err := os.ReadFile(bin + ".stdin")
	require.NoError(t, err)
	require.Equal(t, "add table inet pco_gw\n", string(stdin))
}

// A table that is not there is ErrNotLoaded; a namespace that is not there
// is not, although ip says it in the same words.
func TestAMissingTableIsNotAMissingNamespace(t *testing.T) {
	n, bin := fakeIP(t)
	require.NoError(t, os.WriteFile(bin+".missing", nil, 0o644))
	_, err := n.List(t.Context(), gwTable)
	require.ErrorIs(t, err, ErrNotLoaded)

	require.NoError(t, os.WriteFile(bin+".nonamespace", nil, 0o644))
	_, err = n.List(t.Context(), gwTable)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotLoaded)
	require.True(t, strings.Contains(err.Error(), "Cannot open network namespace"), err.Error())
}
