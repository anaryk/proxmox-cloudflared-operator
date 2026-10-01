package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionCommand(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	require.NoError(t, cmd.Execute())
	require.Equal(t, "pco dev (none, unknown)\n", out.String())
}

func TestUnknownCommandFails(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"nope"})
	require.Error(t, cmd.Execute())
}
