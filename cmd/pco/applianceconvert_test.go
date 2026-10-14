package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// These stop before the node or the store is touched.
func TestApplianceConvertNeedsTheVMIDOnTheNodeAndNoFlagInside(t *testing.T) {
	r := newRunner(t, filepath.Join(t.TempDir(), "pco", "pco.sock"))
	res := r.runReader(unreadable{t}, "appliance", "convert", "--yes")
	require.EqualError(t, res.err, "--vmid names the combined appliance to convert")

	profile := filepath.Join(t.TempDir(), "profile")
	require.NoError(t, os.WriteFile(profile, []byte("appliance\n"), 0o644))
	r = newRunner(t, filepath.Join(t.TempDir(), "pco", "pco.sock"))
	r.env.profileFile = profile
	res = r.runReader(unreadable{t}, "appliance", "convert", "--vmid", "120")
	require.EqualError(t, res.err, "inside the appliance, pco appliance convert takes no flags: run it on the node with --vmid")
}
