package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/daemon"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

// inAppliance is a runner in the appliance lxc/9250, whose volume check says
// volume, and whose daemon does not run.
func inAppliance(t *testing.T, volume error) *runner {
	t.Helper()
	base := testutil.ShortDir(t)
	r := newRunner(t, filepath.Join(base, "run", "pco", "pco.sock"))
	r.env.profileFile = filepath.Join(base, "profile")
	require.NoError(t, os.WriteFile(r.env.profileFile, []byte("appliance\n"), 0o600))
	sys := appliance.System{Proc: filepath.Join(base, "proc"), Sys: filepath.Join(base, "sys")}
	require.NoError(t, os.MkdirAll(filepath.Join(sys.Proc, "self"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sys.Proc, "self", "mountinfo"), []byte(
		"674 500 0:74 / / rw,relatime shared:501 - zfs pcotestpool/subvol-9250-disk-0 rw\n"), 0o644))
	r.env.daemon.Appliance = daemon.ApplianceDeps{
		Volume: func(string, string) error { return volume },
		System: sys,
	}
	return r
}

// Failure mode 2: a restore left the volume without its marker, and the
// daemon does not run; status says what the daemon would.
func TestStatusInAnApplianceWithoutItsVolumeSaysWhatToDo(t *testing.T) {
	for _, tt := range []struct {
		name   string
		volume error
		want   string
	}{
		{name: "no marker", volume: fmt.Errorf("/var/lib/pco %w", appliance.ErrNoMarker),
			want: "pco: /var/lib/pco has no pco volume marker (restore, or a volume that is not pco's?): run pco appliance repair --vmid 9250 on the node\n"},
		{name: "no volume", volume: fmt.Errorf("/var/lib/pco %w", appliance.ErrNotMountPoint),
			want: "pco: /var/lib/pco is not a mount point of its own (a restore without the volume?): run pco appliance repair --vmid 9250 on the node\n"},
		{name: "not root", volume: &fs.PathError{Op: "lstat", Path: "/var/lib/pco/.volume", Err: fs.ErrPermission},
			want: "pco: cannot read /var/lib/pco: run pco status as root\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := inAppliance(t, tt.volume).run("", "status")

			var stderr bytes.Buffer
			require.Equal(t, 2, exitCode(res.err, &stderr))
			require.Equal(t, tt.want, stderr.String())
		})
	}
}

func TestStatusInAnApplianceWithItsVolumeSaysTheDaemonDoesNotRun(t *testing.T) {
	res := inAppliance(t, nil).run("", "status")

	var stderr bytes.Buffer
	require.Equal(t, 2, exitCode(res.err, &stderr))
	require.Contains(t, stderr.String(), "is it running?")
}

func TestStatusOnTheHostLooksForNoVolume(t *testing.T) {
	r := inAppliance(t, fmt.Errorf("/var/lib/pco %w", appliance.ErrNoMarker))
	r.env.profileFile = "/nonexistent/pco/profile"

	res := r.run("", "status")

	var stderr bytes.Buffer
	require.Equal(t, 2, exitCode(res.err, &stderr))
	require.Contains(t, stderr.String(), "is it running?")
	require.NotContains(t, stderr.String(), "marker")
}

func TestTheNodeIsNotGivenInTheAppliance(t *testing.T) {
	r := inAppliance(t, fmt.Errorf("/var/lib/pco %w", appliance.ErrNoMarker))
	base := testutil.ShortDir(t)
	dirs := []string{
		"--cluster-dir", filepath.Join(base, "cluster"),
		"--private-dir", filepath.Join(base, "private"),
		"--local-dir", filepath.Join(base, "local"),
	}

	res := r.run("", append([]string{"daemon", "--node", "pve1"}, dirs...)...)
	require.EqualError(t, res.err, "--node is not taken in the appliance: it runs as the node its install records")

	// Its default is no node given: the daemon goes on to its volume.
	res = r.run("", append([]string{"daemon"}, dirs...)...)
	var stderr bytes.Buffer
	require.Equal(t, appliance.ExitNoVolume, exitCode(res.err, &stderr))
	require.Contains(t, stderr.String(), "has no pco volume marker")
}

func TestTheApplianceGroupShowsItsHelp(t *testing.T) {
	res := newRunner(t, "/nonexistent/pco/pco.sock").run("", "appliance")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "Commands of the pco appliance")
}

func TestStatusShowsTheIdentityOfTheAppliance(t *testing.T) {
	for _, tt := range []struct {
		name string
		id   engine.IdentityView
		want string
	}{
		{name: "proven", id: engine.IdentityView{VMID: 120, Node: "pve1", OK: true}, want: "Identity:    ok (lxc/120 on pve1)\n"},
		{name: "a copy", id: engine.IdentityView{VMID: 120, Node: "pve1", Copy: true, Why: "the volume is another's"},
			want: "Identity:    copy: connectors stopped (not lxc/120 on pve1: the volume is another's)\n"},
		{name: "not proven", id: engine.IdentityView{VMID: 120, Node: "pve1", Why: "the uptimes could not be read"},
			want: "Identity:    not proven (lxc/120 on pve1): the uptimes could not be read\n"},
		{name: "exposed", id: engine.IdentityView{VMID: 120, Node: "pve1", OK: true, Exposed: []string{"alice@pve"}},
			want: "Identity:    serving nothing: alice@pve can reach into lxc/120 on pve1\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := healthyState()
			st.Profile, st.Identity = "appliance", &tt.id
			r, _ := daemonWith(t, st)

			res := r.run("", "status")

			require.Contains(t, res.out, tt.want)
		})
	}
}

func TestANoVolumeErrorExitsWithItsStatus(t *testing.T) {
	var stderr bytes.Buffer
	err := fmt.Errorf("running: %w", daemon.NoVolumeError{Line: "no volume"})

	require.Equal(t, 78, exitCode(err, &stderr))
	require.Equal(t, "pco: running: no volume\n", stderr.String())
	require.Equal(t, 1, exitCode(errors.New("other"), &stderr))
}
