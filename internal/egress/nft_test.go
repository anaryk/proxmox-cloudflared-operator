package egress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeNftScript stands in for nft: it writes its arguments and its input to
// files beside it and answers as its first argument asks.
const fakeNftScript = `#!/bin/sh
printf '%s\n' "$*" >> "$0.args"
printf 'LC_ALL=%s\n' "$LC_ALL" >> "$0.env"
case "$*" in
-f\ -)
	cat > "$0.stdin"
	if grep -q fail "$0.stdin"; then
		printf '%s\n' '/dev/stdin:1:1-4: Error: Could not process rule: Operation not permitted' '  fail' '  ^^^^' >&2
		exit 1
	fi ;;
"-j list table inet pco_egress")
	if [ -e "$0.missing" ]; then
		printf '%s\n' 'Error: No such file or directory' 'list table inet pco_egress' '                ^^^^^^^^^^' >&2
		exit 1
	fi
	if [ -e "$0.big" ]; then
		head -c 40000000 /dev/zero
		exit 0
	fi
	if [ -e "$0.denied" ]; then
		echo 'Error: Could not process rule: Operation not permitted' >&2
		exit 1
	fi
	printf '%s' '{"nftables": []}' ;;
esac
`

func newFakeNftBinary(t *testing.T) (nftCmd, string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "nft")
	require.NoError(t, os.WriteFile(bin, []byte(fakeNftScript), 0o755))
	return nftCmd{bin: bin}, bin
}

func TestNewNftRunsTheSystemBinary(t *testing.T) {
	require.Equal(t, nftCmd{bin: "/usr/sbin/nft"}, NewNft())
}

func TestApplyFeedsTheScriptToNftOnItsInput(t *testing.T) {
	n, bin := newFakeNftBinary(t)

	require.NoError(t, n.Apply(t.Context(), "add table inet pco_egress\n"))

	require.Equal(t, "-f -\n", readText(t, bin+".args"))
	require.Equal(t, "add table inet pco_egress\n", readText(t, bin+".stdin"))
	require.Equal(t, "LC_ALL=C\n", readText(t, bin+".env"), "the messages of nft are read, so they must not be translated")
}

func TestAFailedApplyCarriesWhatNftSaidOnOneLine(t *testing.T) {
	n, _ := newFakeNftBinary(t)

	err := n.Apply(t.Context(), "fail\n")

	require.ErrorContains(t, err, "nft -f -: exit status 1: /dev/stdin:1:1-4: Error: Could not process rule: Operation not permitted; fail; ^^^^")
	require.NotContains(t, err.Error(), "\n")
}

func TestListReturnsWhatNftPrints(t *testing.T) {
	n, bin := newFakeNftBinary(t)

	out, err := n.List(t.Context())

	require.NoError(t, err)
	require.Equal(t, `{"nftables": []}`, string(out))
	require.Equal(t, "-j list table inet pco_egress\n", readText(t, bin+".args"))
}

func TestListOfAMissingTableIsErrNotLoaded(t *testing.T) {
	n, bin := newFakeNftBinary(t)
	require.NoError(t, os.WriteFile(bin+".missing", nil, 0o644))

	_, err := n.List(t.Context())

	require.ErrorIs(t, err, ErrNotLoaded)
}

func TestListFailsOnOtherErrors(t *testing.T) {
	n, bin := newFakeNftBinary(t)
	require.NoError(t, os.WriteFile(bin+".denied", nil, 0o644))

	_, err := n.List(t.Context())

	require.ErrorContains(t, err, "Operation not permitted")
	require.NotErrorIs(t, err, ErrNotLoaded)
}

func TestListRefusesMoreOutputThanItKeeps(t *testing.T) {
	n, bin := newFakeNftBinary(t)
	require.NoError(t, os.WriteFile(bin+".big", nil, 0o644))

	_, err := n.List(t.Context())

	require.ErrorContains(t, err, "more than")
	require.ErrorIs(t, err, ErrUnreadable, "a listing cut short is none nft printed for the table")
}

func TestNftWithoutABinaryFails(t *testing.T) {
	n := nftCmd{bin: filepath.Join(t.TempDir(), "nft")}

	_, err := n.List(t.Context())

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotLoaded, "a missing nft is not a missing table")
}

func TestNftStopsWhenTheContextIsCancelled(t *testing.T) {
	n, bin := newFakeNftBinary(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := n.Apply(ctx, "add table inet pco_egress\n")

	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, bin+".args")
}

func TestCappedKeepsTheFirstBytes(t *testing.T) {
	c := &capped{max: 4}

	for _, p := range []string{"ab", "cde", "f"} {
		n, err := c.Write([]byte(p))
		require.NoError(t, err)
		require.Equal(t, len(p), n, "the writer never makes nft wait")
	}

	require.Equal(t, "abcd", c.buf.String())
	require.True(t, c.over)
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
