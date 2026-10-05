package appliance

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Source is what the mount of a path says about the volume behind it.
type Source struct {
	Raw  string // the source and root fields of mountinfo, as read
	VMID int    // the VMID the volume name carries; 0 when the form is not known
	// Volume is the name the VMID was read from: the dataset, the
	// device-mapper or RBD name, the backing file or the root of a bind. It is
	// empty when the form is not known.
	Volume string
}

// The names Proxmox gives the volumes of a guest, by storage. A device-mapper
// name doubles the dashes of the logical volume's name.
var (
	zfsName  = regexp.MustCompile(`(?:^|/)subvol-([1-9][0-9]*)-disk-[0-9]+$`)
	dmName   = regexp.MustCompile(`-vm--([1-9][0-9]*)--disk--[0-9]+$`)
	rbdName  = regexp.MustCompile(`^vm-([1-9][0-9]*)-disk-[0-9]+$`)
	loopFile = regexp.MustCompile(`/images/([1-9][0-9]*)/vm-([1-9][0-9]*)-disk-[0-9]+\.raw$`)
	bindRoot = regexp.MustCompile(`/images/([1-9][0-9]*)/subvol-([1-9][0-9]*)-disk-[0-9]+\.subvol$`)

	loopDev = regexp.MustCompile(`^loop[0-9]+$`)
	rbdDev  = regexp.MustCompile(`^rbd([0-9]+)$`)
	dmDev   = regexp.MustCompile(`^dm-[0-9]+$`)
)

// MountSource reads the mount at path from /proc/self/mountinfo and names the
// VMID of its volume by the forms of ruling 8: a ZFS dataset
// ".../subvol-<vmid>-disk-N"; a device-mapper name "<vg>-vm--<vmid>--disk--N"
// (a /dev/dm-N source resolved through /sys/block/dm-N/dm/name); a Ceph RBD
// device /dev/rbdN whose /sys/devices/rbd/N/name is "vm-<vmid>-disk-N"; a loop
// device resolved through /sys/block/loopN/loop/backing_file to
// ".../images/<vmid>/vm-<vmid>-disk-N.raw"; a bind mount whose root field is
// ".../images/<vmid>/subvol-<vmid>-disk-N.subvol".
func MountSource(path string) (Source, error) { return System{}.MountSource(path) }

// MountSource is the package's MountSource on s. A source given by another
// device path, as /dev/<vg>/<lv>, is resolved through the device number in
// /sys/dev/block.
func (s System) MountSource(path string) (Source, error) {
	m, err := s.mountAt(path)
	if err != nil {
		return Source{}, err
	}
	src := Source{Raw: m.source + " " + m.root}
	src.Volume, src.VMID = s.volumeOf(m)
	return src, nil
}

// mount is a line of mountinfo, with its escapes undone.
type mount struct {
	dev, root, point, fstype, source string
}

// mountAt returns the last mount at path, the one on top.
func (s System) mountAt(path string) (mount, error) {
	file := s.proc("self", "mountinfo")
	f, err := os.Open(file)
	if err != nil {
		return mount{}, fmt.Errorf("reading the mounts: %w", err)
	}
	defer func() { _ = f.Close() }()
	var found *mount
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		m, ok := parseMount(sc.Text())
		if ok && m.point == filepath.Clean(path) {
			found = &m
		}
	}
	if err := sc.Err(); err != nil {
		return mount{}, fmt.Errorf("reading %s: %w", file, err)
	}
	if found == nil {
		return mount{}, fmt.Errorf("nothing is mounted at %s", path)
	}
	return *found, nil
}

// parseMount reads one line of mountinfo: id, parent, major:minor, root,
// mount point, options, optional fields up to a "-", then the type, the
// source and the options of the filesystem.
func parseMount(line string) (mount, bool) {
	f := strings.Fields(line)
	if len(f) < 10 {
		return mount{}, false
	}
	sep := -1
	for i := 6; i < len(f); i++ {
		if f[i] == "-" {
			sep = i
			break
		}
	}
	if sep < 0 || len(f) < sep+3 {
		return mount{}, false
	}
	return mount{dev: f[2], root: unescape(f[3]), point: unescape(f[4]), fstype: f[sep+1], source: unescape(f[sep+2])}, true
}

// unescape undoes the octal escapes mountinfo writes for a space, a tab, a
// newline and a backslash.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// volumeOf names the volume behind a mount and the VMID its name carries.
func (s System) volumeOf(m mount) (string, int) {
	if m.root != "/" {
		return matched(bindRoot, m.root)
	}
	if !strings.HasPrefix(m.source, "/dev/") {
		return matched(zfsName, m.source)
	}
	dev := strings.TrimPrefix(m.source, "/dev/")
	if name, ok := strings.CutPrefix(dev, "mapper/"); ok {
		return matched(dmName, name)
	}
	if !loopDev.MatchString(dev) && !rbdDev.MatchString(dev) && !dmDev.MatchString(dev) {
		// Another path to the device, as /dev/<vg>/<lv>: the kernel's name
		// of the device is where its number points in sysfs.
		link, err := os.Readlink(s.sys("dev", "block", m.dev))
		if err != nil {
			return "", 0
		}
		dev = filepath.Base(link)
	}
	switch {
	case dmDev.MatchString(dev):
		return matched(dmName, s.read("block", dev, "dm", "name"))
	case loopDev.MatchString(dev):
		return matched(loopFile, s.read("block", dev, "loop", "backing_file"))
	case rbdDev.MatchString(dev):
		return matched(rbdName, s.read("devices", "rbd", rbdDev.FindStringSubmatch(dev)[1], "name"))
	}
	return "", 0
}

// matched returns name and the VMID re matches in it. A pattern that names
// the VMID twice, as the directory and the file of an image, must name the
// same one twice.
func matched(re *regexp.Regexp, name string) (string, int) {
	sub := re.FindStringSubmatch(name)
	if sub == nil {
		return "", 0
	}
	vmid, err := strconv.Atoi(sub[1])
	if err != nil {
		return "", 0
	}
	for _, other := range sub[2:] {
		if other != sub[1] {
			return "", 0
		}
	}
	return name, vmid
}

// read returns the trimmed content of a file below /sys, or empty when it
// cannot be read.
func (s System) read(parts ...string) string {
	b, err := os.ReadFile(s.sys(parts...))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
