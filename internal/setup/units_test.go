package setup

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheUnitOfTheNamespaceIsRunAndWaitedFor(t *testing.T) {
	var events []string
	r := &fakeRunner{t: t, events: &events, script: []call{
		{line: "systemctl start -- pco-netns.service"},
		{line: "systemctl enable --now -- pco-netns.service"},
		{line: "systemctl disable --now -- pco-netns.service"},
	}}
	u := NewNamespaceUnit(r)

	require.NoError(t, u.Start(t.Context()))
	require.NoError(t, u.Enable(t.Context()))
	require.NoError(t, u.Disable(t.Context()))

	require.Empty(t, r.script, "every command ran, none with --no-block: the namespace is up when Start and Enable return")
}

func TestAUnitCommandThatFailsSaysSo(t *testing.T) {
	var events []string
	r := &fakeRunner{t: t, events: &events, script: []call{
		{line: "systemctl enable --now -- pco-netns.service", err: errors.New("exit status 1: Access denied")},
	}}

	err := NewNamespaceUnit(r).Enable(t.Context())

	require.EqualError(t, err, "systemctl enable --now pco-netns.service: exit status 1: Access denied")
}
