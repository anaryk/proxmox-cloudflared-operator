package appliance

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeSystem is a /proc and a /sys in a temporary directory.
type fakeSystem struct {
	t   *testing.T
	sys System
}

func newFakeSystem(t *testing.T) *fakeSystem {
	t.Helper()
	base := t.TempDir()
	return &fakeSystem{t: t, sys: System{Proc: filepath.Join(base, "proc"), Sys: filepath.Join(base, "sys")}}
}

func (f *fakeSystem) write(path, content string) {
	f.t.Helper()
	require.NoError(f.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o644))
}

func (f *fakeSystem) proc(rel, content string)  { f.write(filepath.Join(f.sys.Proc, rel), content) }
func (f *fakeSystem) sysfs(rel, content string) { f.write(filepath.Join(f.sys.Sys, rel), content) }

// mountinfo makes the mounts those of a fixture.
func (f *fakeSystem) mountinfo(fixture string) {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "mountinfo-"+fixture))
	require.NoError(f.t, err)
	f.proc("self/mountinfo", string(b))
}

// link makes /sys/dev/block/<dev> point at a block device of that name.
func (f *fakeSystem) link(dev, name string) {
	f.t.Helper()
	dir := filepath.Join(f.sys.Sys, "dev", "block")
	require.NoError(f.t, os.MkdirAll(dir, 0o755))
	require.NoError(f.t, os.Symlink("../../devices/virtual/block/"+name, filepath.Join(dir, dev)))
}

func TestTheMountSourceNamesTheVMIDOfItsVolume(t *testing.T) {
	for _, tt := range []struct {
		name, fixture string
		sys           func(f *fakeSystem)
		raw, volume   string
		vmid          int
	}{
		{name: "zfs, lab A7(e)", fixture: "zfs", raw: "pcotestpool/subvol-9201-disk-1 /", volume: "pcotestpool/subvol-9201-disk-1", vmid: 9201},
		{name: "zfs from a unit", fixture: "zfs-in-unit", raw: "pcotestpool/subvol-9201-disk-1 /", volume: "pcotestpool/subvol-9201-disk-1", vmid: 9201},
		{name: "zfs of a clone, lab 40", fixture: "zfs-clone", raw: "pcotestpool/subvol-9210-disk-1 /", volume: "pcotestpool/subvol-9210-disk-1", vmid: 9210},
		{
			name: "a loop device on a directory storage, lab A7(e)", fixture: "loop",
			sys: func(f *fakeSystem) {
				f.sysfs("block/loop1/loop/backing_file", "/var/lib/vz/images/9202/vm-9202-disk-1.raw\n")
			},
			raw: "/dev/loop1 /", volume: "/var/lib/vz/images/9202/vm-9202-disk-1.raw", vmid: 9202,
		},
		{
			name: "a loop device from a unit", fixture: "loop-in-unit",
			sys: func(f *fakeSystem) {
				f.sysfs("block/loop1/loop/backing_file", "/var/lib/vz/images/9202/vm-9202-disk-1.raw\n")
			},
			raw: "/dev/loop1 /", volume: "/var/lib/vz/images/9202/vm-9202-disk-1.raw", vmid: 9202,
		},
		{
			name: "a loop device of another VMID", fixture: "loop",
			sys: func(f *fakeSystem) {
				f.sysfs("block/loop1/loop/backing_file", "/var/lib/vz/images/9295/vm-9295-disk-1.raw\n")
			},
			raw: "/dev/loop1 /", volume: "/var/lib/vz/images/9295/vm-9295-disk-1.raw", vmid: 9295,
		},
		{
			name: "a loop device whose file is in the directory of another VMID", fixture: "loop",
			sys: func(f *fakeSystem) {
				f.sysfs("block/loop1/loop/backing_file", "/var/lib/vz/images/9250/vm-9295-disk-1.raw\n")
			},
			raw: "/dev/loop1 /",
		},
		{name: "a loop device sysfs does not show", fixture: "loop", raw: "/dev/loop1 /"},
		{name: "lvm-thin by its mapper name", fixture: "lvm", raw: "/dev/mapper/pve-vm--9250--disk--1 /", volume: "pve-vm--9250--disk--1", vmid: 9250},
		{
			name: "device-mapper by dm-N", fixture: "dm",
			sys: func(f *fakeSystem) { f.sysfs("block/dm-3/dm/name", "pve-vm--9250--disk--1\n") },
			raw: "/dev/dm-3 /", volume: "pve-vm--9250--disk--1", vmid: 9250,
		},
		{
			name: "device-mapper of another VMID", fixture: "dm",
			sys: func(f *fakeSystem) { f.sysfs("block/dm-3/dm/name", "pve-vm--9295--disk--1\n") },
			raw: "/dev/dm-3 /", volume: "pve-vm--9295--disk--1", vmid: 9295,
		},
		{
			name: "a volume group with a dash in its name", fixture: "dm",
			sys: func(f *fakeSystem) { f.sysfs("block/dm-3/dm/name", "my--vg-vm--9250--disk--1\n") },
			raw: "/dev/dm-3 /", volume: "my--vg-vm--9250--disk--1", vmid: 9250,
		},
		{
			name: "lvm by the path of its volume group", fixture: "lvm-path",
			sys: func(f *fakeSystem) {
				f.link("253:4", "dm-4")
				f.sysfs("block/dm-4/dm/name", "pve-vm--9250--disk--1\n")
			},
			raw: "/dev/pve/vm-9250-disk-1 /", volume: "pve-vm--9250--disk--1", vmid: 9250,
		},
		{name: "a path whose device sysfs does not show", fixture: "lvm-path", raw: "/dev/pve/vm-9250-disk-1 /"},
		{
			name: "ceph rbd", fixture: "rbd",
			sys: func(f *fakeSystem) { f.sysfs("devices/rbd/1/name", "vm-9250-disk-1\n") },
			raw: "/dev/rbd1 /", volume: "vm-9250-disk-1", vmid: 9250,
		},
		{
			name: "ceph rbd of another VMID", fixture: "rbd",
			sys: func(f *fakeSystem) { f.sysfs("devices/rbd/1/name", "vm-9295-disk-1\n") },
			raw: "/dev/rbd1 /", volume: "vm-9295-disk-1", vmid: 9295,
		},
		{
			name: "a bind of a subvol directory", fixture: "bind",
			raw: "/dev/mapper/pve-root /var/lib/vz/images/9250/subvol-9250-disk-1.subvol", volume: "/var/lib/vz/images/9250/subvol-9250-disk-1.subvol", vmid: 9250,
		},
		{name: "a form pco does not know", fixture: "unknown", raw: "10.92.0.5:/export/pco /"},
		{name: "the mount on top, past an escaped mount point", fixture: "stacked", raw: "pcotestpool/subvol-9250-disk-1 /", volume: "pcotestpool/subvol-9250-disk-1", vmid: 9250},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeSystem(t)
			f.mountinfo(tt.fixture)
			if tt.sys != nil {
				tt.sys(f)
			}

			got, err := f.sys.MountSource("/var/lib/pco")

			require.NoError(t, err)
			require.Equal(t, Source{Raw: tt.raw, Volume: tt.volume, VMID: tt.vmid}, got)
		})
	}
}

func TestAPathNothingIsMountedAtHasNoSource(t *testing.T) {
	f := newFakeSystem(t)
	f.mountinfo("zfs")

	_, err := f.sys.MountSource("/var/lib/elsewhere")

	require.ErrorContains(t, err, "nothing is mounted at /var/lib/elsewhere")

	_, err = newFakeSystem(t).sys.MountSource("/var/lib/pco")
	require.ErrorContains(t, err, "reading the mounts")
}

func TestTheVMIDHintComesFromTheVolumeOrElseTheRootFilesystem(t *testing.T) {
	f := newFakeSystem(t)
	f.mountinfo("zfs")
	require.Equal(t, "9201", f.sys.VMIDHint("/var/lib/pco"))
	require.Equal(t, "9201", f.sys.VMIDHint("/var/lib/elsewhere"), "the root filesystem names it too")

	f.mountinfo("unknown")
	require.Equal(t, "<vmid>", f.sys.VMIDHint("/var/lib/pco"))
}

const (
	bootA = "6c3f2a8e-1b4d-4e0f-9a2b-7d5c8e1f3a6b"
	// statOfPID1 is /proc/1/stat of a container; its start is 123456789
	// ticks after the node booted.
	statOfPID1 = "1 (systemd) S 0 1 1 0 -1 4194560 12971 3058829 103 1393 41 87 22411 8399 20 0 1 0 123456789 23302144 2969 18446744073709551615 1 1 0 0 0 0 671173123 4096 1260 0 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
)

func TestTheIncarnationIsTheBootIDAndTheRawStartOfPID1(t *testing.T) {
	f := newFakeSystem(t)
	f.proc("sys/kernel/random/boot_id", bootA+"\n")
	f.proc("1/stat", statOfPID1)

	got, err := f.sys.Incarnation()
	require.NoError(t, err)
	require.Equal(t, bootA+"/123456789", got)

	// The node's boot time is never used: the start stays in ticks.
	f.proc("stat", "cpu  1 2 3 4\nbtime 1790000000\n")
	again, err := f.sys.Incarnation()
	require.NoError(t, err)
	require.Equal(t, got, again)
	f.proc("stat", "cpu  1 2 3 4\nbtime 1790999999\n")
	again, err = f.sys.Incarnation()
	require.NoError(t, err)
	require.Equal(t, got, again)
}

func TestTheStartOfPID1IsCountedFromTheEndOfItsName(t *testing.T) {
	stat := strings.Replace(statOfPID1, "(systemd)", "(my (odd) init)", 1)
	ticks, err := startTicks(stat)
	require.NoError(t, err)
	require.Equal(t, "123456789", ticks)
}

func TestAnIncarnationThatCannotBeReadIsAnError(t *testing.T) {
	for _, tt := range []struct {
		name, boot, stat, want string
	}{
		{name: "no boot id", stat: statOfPID1, want: "reading the boot id"},
		{name: "a boot id that is none", boot: "not-a-uuid\n", stat: statOfPID1, want: "is not one"},
		{name: "no stat", boot: bootA, want: "reading the start of PID 1"},
		{name: "a short stat", boot: bootA, stat: "1 (systemd) S 0 1\n", want: "fewer than 22 fields"},
		{name: "a start that is no number", boot: bootA, stat: strings.Replace(statOfPID1, "123456789", "12x", 1), want: "not a number"},
		{name: "no name", boot: bootA, stat: "1 systemd S\n", want: "no name"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeSystem(t)
			if tt.boot != "" {
				f.proc("sys/kernel/random/boot_id", tt.boot)
			}
			if tt.stat != "" {
				f.proc("1/stat", tt.stat)
			}
			_, err := f.sys.Incarnation()
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestVolumeMountedRefusesADirectoryOfItsParentsDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pco")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, store.VolumeMarker), nil, 0o600))

	err := VolumeMounted(dir, store.VolumeMarker)

	require.ErrorIs(t, err, ErrNotMountPoint, "the marker alone is no volume")
	require.Error(t, VolumeMounted(filepath.Join(t.TempDir(), "missing"), store.VolumeMarker))
}

func TestTheLinesOfAVolumeThatIsNotThereNameTheRepair(t *testing.T) {
	require.Equal(t, "/var/lib/pco has no pco volume marker (restore, or a volume that is not pco's?): run pco appliance repair --vmid 9250 on the node",
		VolumeLine("/var/lib/pco", ErrNoMarker, "9250"))
	require.Equal(t, "/var/lib/pco is not a mount point of its own (a restore without the volume?): run pco appliance repair --vmid 9250 on the node",
		VolumeLine("/var/lib/pco", ErrNotMountPoint, "9250"))
	require.Equal(t, "cannot read /var/lib/pco: boom", VolumeLine("/var/lib/pco", errors.New("boom"), "9250"))
	require.Equal(t, 78, ExitNoVolume)
}

func TestLinksAreTheNICsWithAHardwareAddress(t *testing.T) {
	f := newFakeSystem(t)
	for _, l := range []struct{ name, addr, index, link string }{
		{"lo", "00:00:00:00:00:00", "1", "1"},
		{"eth0", "BC:24:11:00:92:50", "2", "23"},
		{"eth1", "bc:24:11:00:92:51", "3", "24"},
		{"pco0", "6a:1f:00:aa:bb:cc", "4", "4"},
		{"tun0", "", "5", "5"},
		{"dummy1", "7a:00:00:00:00:01", "6", "6"},
	} {
		dir := "class/net/" + l.name + "/"
		f.sysfs(dir+"address", l.addr+"\n")
		f.sysfs(dir+"ifindex", l.index+"\n")
		f.sysfs(dir+"iflink", l.link+"\n")
	}

	links, err := f.sys.Links()

	require.NoError(t, err)
	require.Equal(t, []NamedLink{
		{Name: "dummy1", MAC: "7a:00:00:00:00:01"},
		{Name: "eth0", MAC: "bc:24:11:00:92:50"},
		{Name: "eth1", MAC: "bc:24:11:00:92:51"},
	}, links, "lo, the dummy pco0 and a link without an address are not NICs")

	f.sysfs("class/net/pco0/iflink", "25\n")
	links, err = f.sys.Links()
	require.NoError(t, err)
	require.Contains(t, links, NamedLink{Name: "pco0", MAC: "6a:1f:00:aa:bb:cc"}, "a veth named pco0 is a NIC")

	_, err = newFakeSystem(t).sys.Links()
	require.ErrorContains(t, err, "listing the links")
}

func TestTheUptimeIsTheContainers(t *testing.T) {
	f := newFakeSystem(t)
	f.proc("uptime", "3600.25 7000.00\n")
	up, err := f.sys.Uptime()
	require.NoError(t, err)
	require.Equal(t, 3600*time.Second+250*time.Millisecond, up)

	f.proc("uptime", "soon\n")
	_, err = f.sys.Uptime()
	require.Error(t, err)
}

const (
	ownVMID = 9250
	macA    = "bc:24:11:00:92:50"
	macB    = "bc:24:11:00:92:51"
)

var (
	t0   = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	self = model.GuestRef{Kind: model.KindLXC, VMID: ownVMID}
	me   = Identity{VMID: ownVMID, Node: "pve1", MACs: []string{macA}}
)

func lxc(vmid int, pool string, macs ...string) model.Guest {
	g := model.Guest{Ref: model.GuestRef{Kind: model.KindLXC, VMID: vmid}, Node: "pve1", Running: true, Pool: pool}
	for i, m := range macs {
		g.NICs = append(g.NICs, model.NIC{Index: i, MAC: m, Bridge: "vmbr0"})
	}
	return g
}

func snap(guests ...model.Guest) inventory.Snapshot {
	return inventory.Snapshot{Guests: guests, Complete: true}
}

var (
	byMount = Source{Raw: "pcotestpool/subvol-9250-disk-1 /", Volume: "pcotestpool/subvol-9250-disk-1", VMID: ownVMID}
	byOther = Source{Raw: "pcotestpool/subvol-9295-disk-1 /", Volume: "pcotestpool/subvol-9295-disk-1", VMID: 9295}
	unknown = Source{Raw: "10.92.0.5:/export/pco /"}
)

func facts(src Source, up time.Duration, macs ...string) Facts {
	f := Facts{Mount: src, Uptime: up}
	for i, m := range macs {
		f.Links = append(f.Links, NamedLink{Name: "eth" + string(rune('0'+i)), MAC: m})
	}
	return f
}

func upFor(d time.Duration) map[model.GuestRef]time.Duration {
	return map[model.GuestRef]time.Duration{self: d}
}

func TestCheckProvesTheVMIDByAFactACopyCannotShare(t *testing.T) {
	const hour = time.Hour
	for _, tt := range []struct {
		name    string
		snap    inventory.Snapshot
		facts   Facts
		uptimes map[model.GuestRef]time.Duration
		want    Verdict
		why     string
	}{
		{name: "proven by the mount", snap: snap(lxc(ownVMID, Pool, macA)), facts: facts(byMount, hour, macA), uptimes: upFor(5 * hour),
			want: Verdict{OK: true}},
		{name: "a mount that names another VMID is a copy whatever the uptime", snap: snap(lxc(ownVMID, Pool, macA)), facts: facts(byOther, hour, macA), uptimes: upFor(hour),
			want: Verdict{Copy: true}, why: "a volume of VMID 9295 and not of lxc/9250: this container is a copy"},
		{name: "a mount that names another VMID is a copy before any snapshot", snap: inventory.Snapshot{}, facts: facts(byOther, hour, macA),
			want: Verdict{Copy: true}, why: "pcotestpool/subvol-9295-disk-1"},
		{name: "an unknown form proven by the uptime", snap: snap(lxc(ownVMID, Pool, macA)), facts: facts(unknown, hour, macA), uptimes: upFor(hour + 9*time.Second),
			want: Verdict{OK: true}},
		{name: "an unknown form with the uptime off", snap: snap(lxc(ownVMID, Pool, macA)), facts: facts(unknown, hour, macA), uptimes: upFor(hour + 11*time.Second),
			want: Verdict{Copy: true}, why: "this container has been up for 1h0m0s and lxc/9250 for 1h0m11s"},
		{name: "an unknown form while Proxmox reports no uptime of the VMID", snap: snap(lxc(ownVMID, Pool, macA)), facts: facts(unknown, hour, macA), uptimes: map[model.GuestRef]time.Duration{},
			want: Verdict{Copy: true}, why: "Proxmox reports no uptime of lxc/9250"},
		{name: "uptimes missing", snap: snap(lxc(ownVMID, Pool, macA)), facts: facts(unknown, hour, macA),
			want: Verdict{}, why: "the uptimes of the guests could not be read"},
		{name: "uptimes missing with the mount proving it", snap: snap(lxc(ownVMID, Pool, macA)), facts: facts(byMount, hour, macA),
			want: Verdict{}, why: "the uptimes of the guests could not be read"},
		{name: "an incomplete snapshot", snap: inventory.Snapshot{Guests: []model.Guest{lxc(ownVMID, Pool, macA)}}, facts: facts(byMount, hour, macA), uptimes: upFor(hour),
			want: Verdict{}, why: "the inventory is incomplete"},
		{name: "a complete snapshot without the VMID", snap: snap(lxc(9295, Pool, macA)), facts: facts(byMount, hour, macA), uptimes: upFor(hour),
			want: Verdict{Copy: true, Copies: nil}, why: "Proxmox lists no lxc/9250"},
		{name: "G1: an unknown form within the uptime while another guest carries our MAC", snap: snap(lxc(ownVMID, Pool, macA), lxc(9295, "", macA)),
			facts: facts(unknown, hour, macA), uptimes: upFor(hour),
			want: Verdict{Copy: true, Copies: []model.GuestRef{{Kind: model.KindLXC, VMID: 9295}}}, why: "lxc/9295 carries the MAC bc:24:11:00:92:50 of this container"},
		{name: "an unproven duplicate MAC in the pool", snap: snap(lxc(ownVMID, Pool, macA), lxc(9295, Pool, macA)),
			facts: facts(unknown, hour, macA), uptimes: upFor(hour),
			want: Verdict{Copy: true, Copies: []model.GuestRef{{Kind: model.KindLXC, VMID: 9295}}}, why: "nothing proves that this container is lxc/9250"},
		{name: "a stopped guest with our MAC is no duplicate", snap: snap(lxc(ownVMID, Pool, macA), stopped(lxc(9295, Pool, macA))),
			facts: facts(unknown, hour, macA), uptimes: upFor(hour),
			want: Verdict{OK: true}},
		{name: "proven, a guest in the pool with our MAC is a copy of us", snap: snap(lxc(ownVMID, Pool, macA), lxc(9295, Pool, macA)),
			facts: facts(byMount, hour, macA), uptimes: upFor(hour),
			want: Verdict{Copies: []model.GuestRef{{Kind: model.KindLXC, VMID: 9295}}}, why: "lxc/9295 in pool pco carries a MAC of lxc/9250: a copy of the appliance runs"},
		{name: "proven, a guest outside the pool with our MAC is a tenant", snap: snap(lxc(ownVMID, Pool, macA), qemu(140, macA)),
			facts: facts(byMount, hour, macA), uptimes: upFor(hour),
			want: Verdict{OK: true, Tenants: []model.GuestRef{{Kind: model.KindQEMU, VMID: 140}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var first time.Time
			got := me.Check(tt.snap, tt.facts, tt.uptimes, t0, &first)

			require.Contains(t, got.Why, tt.why)
			got.Why = ""
			require.Equal(t, tt.want, got)
			if tt.want.OK {
				require.Empty(t, tt.why)
			}
		})
	}
}

func stopped(g model.Guest) model.Guest {
	g.Running = false
	return g
}

func qemu(vmid int, macs ...string) model.Guest {
	g := lxc(vmid, "", macs...)
	g.Ref.Kind = model.KindQEMU
	return g
}

func TestAMACMismatchIsPendingForAMinuteThenDecidedByTheMount(t *testing.T) {
	added := snap(lxc(ownVMID, Pool, macA, macB)) // net1 configured, no link for it yet
	for _, tt := range []struct {
		name   string
		mount  Source
		after  time.Duration
		want   Verdict
		reason string
	}{
		{name: "within the minute, proven by the mount", mount: byMount, after: 59 * time.Second, want: Verdict{OK: true, Pending: true}},
		{name: "within the minute, proven by the uptime", mount: unknown, after: 59 * time.Second, want: Verdict{OK: true, Pending: true}},
		{name: "failure mode 5: past the minute, proven by the mount", mount: byMount, after: 61 * time.Second, want: Verdict{},
			reason: "the links of this container carry bc:24:11:00:92:50, but lxc/9250 in Proxmox has bc:24:11:00:92:50, bc:24:11:00:92:51 for more than 60 s; writes are held"},
		{name: "past the minute, proven by the uptime only", mount: unknown, after: 61 * time.Second, want: Verdict{Copy: true},
			reason: "for more than 60 s, and nothing but the uptime proves this container is lxc/9250"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var first time.Time
			v := me.Check(added, facts(tt.mount, time.Hour, macA), upFor(time.Hour), t0, &first)
			require.Equal(t, Verdict{OK: true, Pending: true}, v)
			require.Equal(t, t0, first, "the first config that showed the difference")

			v = me.Check(added, facts(tt.mount, time.Hour+tt.after, macA), upFor(time.Hour+tt.after), t0.Add(tt.after), &first)

			require.Contains(t, v.Why, tt.reason)
			v.Why = ""
			require.Equal(t, tt.want, v)
		})
	}
}

func TestMACsThatMatchAgainForgetTheMismatch(t *testing.T) {
	var first time.Time
	me.Check(snap(lxc(ownVMID, Pool, macA, macB)), facts(byMount, time.Hour, macA), upFor(time.Hour), t0, &first)
	require.Equal(t, t0, first)

	v := me.Check(snap(lxc(ownVMID, Pool, macA)), facts(byMount, time.Hour, macA), upFor(time.Hour), t0.Add(30*time.Second), &first)
	require.Equal(t, Verdict{OK: true}, v)
	require.True(t, first.IsZero())

	// A NIC that vanished counts from when it was first seen gone.
	v = me.Check(snap(lxc(ownVMID, Pool, macA)), facts(byMount, time.Hour, macA, macB), upFor(time.Hour), t0.Add(2*time.Minute), &first)
	require.Equal(t, Verdict{OK: true, Pending: true}, v)
	require.Equal(t, t0.Add(2*time.Minute), first)
}

func TestSegmentsMapTheLinksToTheBridgesOfTheirNICs(t *testing.T) {
	own := lxc(ownVMID, Pool)
	own.NICs = []model.NIC{
		{Index: 0, MAC: macA, Bridge: "vmbr0", VLAN: 10},
		{Index: 1, MAC: macB, Bridge: "vmbr1"},
	}
	links := []NamedLink{{Name: "eth0", MAC: macA}, {Name: "eth1", MAC: macB}, {Name: "eth2", MAC: "bc:24:11:00:00:99"}}

	got := me.Segments(snap(own, lxc(9295, "", "bc:24:11:00:00:99")), links)

	require.Equal(t, map[string]resolve.Segment{
		"eth0": {Bridge: "vmbr0", VLAN: 10},
		"eth1": {Bridge: "vmbr1"},
	}, got)
	require.Empty(t, me.Segments(snap(), links), "nothing before the appliance is listed")
}

// hostProber is the prober of the container: its interfaces are on no
// segment of their own.
type hostProber struct{ resolve.Prober }

func (hostProber) Interfaces(context.Context) ([]resolve.HostIface, error) {
	return []resolve.HostIface{
		{Name: "eth0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.92.0.150/24")}},
		{Name: "pco0", Segment: resolve.Segment{Bridge: "nonsense"}},
	}, nil
}

func TestTheProberPutsTheInterfacesOnTheirSegments(t *testing.T) {
	segments := map[string]resolve.Segment{"eth0": {Bridge: "vmbr1", VLAN: 20}}
	p := NewProber(hostProber{}, func() map[string]resolve.Segment { return segments })

	got, err := p.Interfaces(t.Context())

	require.NoError(t, err)
	require.Equal(t, []resolve.HostIface{
		{Name: "eth0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.92.0.150/24")}, Segment: resolve.Segment{Bridge: "vmbr1", VLAN: 20}},
		{Name: "pco0"},
	}, got)
}

func TestTheGatewaysAreThoseOfTheDefaultRoutes(t *testing.T) {
	f := newFakeSystem(t)
	f.proc("net/route", "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
		"eth0\t00000000\t01005C0A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"+ // default via 10.92.0.1
		"eth0\t00005C0A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"+ // 10.92.0.0/24, on the link
		"eth1\t00000000\t01015C0A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"+ // default via 10.92.1.1, a second one
		"eth1\t00000000\t00000000\t0001\t0\t0\t200\t00000000\t0\t0\t0\n") // default on the link, no gateway

	got, err := f.sys.Gateways()

	require.NoError(t, err)
	require.Equal(t, []netip.Addr{netip.MustParseAddr("10.92.0.1"), netip.MustParseAddr("10.92.1.1")}, got)

	_, err = newFakeSystem(t).sys.Gateways()
	require.Error(t, err)
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	base := t.TempDir()
	st, err := store.Open(store.Paths{Cluster: filepath.Join(base, "cluster"), Private: filepath.Join(base, "private"), Local: filepath.Join(base, "local"), Durable: true})
	require.NoError(t, err)
	require.NoError(t, st.Init())
	return st
}

// zeros draws the nonce aaaaaaaa.
func zeros() *bytes.Reader { return bytes.NewReader(make([]byte, 64)) }

func TestTheEpochIsKeptWithinAnIncarnationAndDrawnAnewAfterIt(t *testing.T) {
	const incA, incB = bootA + "/100", bootA + "/200"
	stored := planner.Writer{InstallID: "abc123", Generation: 5, Nonce: "n5", Incarnation: incA}
	for _, tt := range []struct {
		name        string
		incarnation string
		want        planner.Writer
		kept        bool
	}{
		{name: "the same incarnation keeps it", incarnation: incA, want: stored, kept: true},
		{name: "another incarnation draws generation 6", incarnation: incB,
			want: planner.Writer{InstallID: "abc123", Generation: 6, Nonce: "aaaaaaaa", Incarnation: incB}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			require.NoError(t, st.SaveWriter(stored))

			got, kept, err := EpochAtStart(st, "abc123", tt.incarnation, zeros())

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.kept, kept)
			onDisk, found, err := st.Writer()
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, tt.want, onDisk, "saved before it is returned")
		})
	}
}

func TestAnEpochFromAHostWriterIsDrawnAnew(t *testing.T) {
	st := newStore(t)
	require.NoError(t, st.SaveWriter(planner.Writer{InstallID: "abc123", Generation: 2, Nonce: "n2"}))

	got, kept, err := EpochAtStart(st, "abc123", bootA+"/1", zeros())

	require.NoError(t, err)
	require.False(t, kept)
	require.Equal(t, 3, got.Generation)
}

func TestNoEpochIsDrawnOverWhatIsNotThisInstallsLeaderJSON(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stored *planner.Writer
		inc    string
		want   string
	}{
		{name: "missing", want: "leader.json is missing; run pco appliance recover"},
		{name: "another install", stored: &planner.Writer{InstallID: "other1", Generation: 4, Nonce: "n4"}, inc: bootA + "/1",
			want: "leader.json names install other1, but this is install abc123; run pco appliance recover"},
		{name: "no incarnation", stored: &planner.Writer{InstallID: "abc123", Generation: 4, Nonce: "n4"},
			want: "the incarnation of this container is not known"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := newStore(t)
			if tt.stored != nil {
				require.NoError(t, st.SaveWriter(*tt.stored))
			}
			inc := tt.inc
			if inc == "" && tt.stored == nil {
				inc = bootA + "/1"
			}

			_, _, err := EpochAtStart(st, "abc123", inc, zeros())

			require.EqualError(t, err, tt.want)
			w, found, rerr := st.Writer()
			require.NoError(t, rerr)
			if tt.stored != nil {
				require.Equal(t, *tt.stored, w, "nothing written")
			} else {
				require.False(t, found)
			}
		})
	}
}

func TestTheIdentityFlagIsWrittenAndRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pco-appliance", "identity-ok")
	require.Equal(t, "/run/pco-appliance/identity-ok", IdentityFlag)

	require.NoError(t, WriteFlag(path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), dir.Mode().Perm())

	require.NoError(t, RemoveFlag(path))
	require.NoFileExists(t, path)
	require.NoError(t, RemoveFlag(path), "a flag that is gone is removed")
}
