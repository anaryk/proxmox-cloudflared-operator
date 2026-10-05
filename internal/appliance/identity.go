// Package appliance is what only the appliance has: it proves that the
// container it runs in is the one it was installed as, from facts a copy
// cannot share, binds the writer epoch to the start of the container, and maps
// the container's interfaces to the segments of the bridges they are on.
package appliance

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

const (
	// Pool is the pool of Proxmox the appliance is installed in.
	Pool = "pco"

	// IdentityFlag is the tmpfs file the connectors' start condition reads
	// (ruling 23): written on an OK verdict, removed on Copy, absent after
	// boot.
	IdentityFlag = "/run/pco-appliance/identity-ok"

	// macTolerance is how long a NIC that appeared or vanished is
	// tolerated, from the first fresh config that showed it.
	macTolerance = 60 * time.Second
	// uptimeSlack is how far the uptime of the container may be from the
	// one Proxmox reports of its VMID.
	uptimeSlack = 10 * time.Second

	// dummyLink is the dummy device of the service prefix, which is no NIC.
	dummyLink = "pco0"

	// WhyIncomplete is why a check of an incomplete snapshot is not OK.
	WhyIncomplete = "the inventory is incomplete; self-identification waits for a complete one"
)

// System is where the facts of the container are read: the roots of /proc
// and /sys. The zero value reads those of the running system.
type System struct {
	Proc string
	Sys  string
}

func (s System) proc(parts ...string) string {
	return filepath.Join(append([]string{cmp.Or(s.Proc, "/proc")}, parts...)...)
}

func (s System) sys(parts ...string) string {
	return filepath.Join(append([]string{cmp.Or(s.Sys, "/sys")}, parts...)...)
}

// NamedLink is an interface of the container with a MAC: every veth (which
// is every LXC NIC, named ethN@ifM inside) and anything else with a hardware
// address, except lo and the dummy pco0.
type NamedLink struct{ Name, MAC string }

// Links lists the links of the container, by name.
func Links() ([]NamedLink, error) { return System{}.Links() }

// Links is the package's Links on s, read from /sys/class/net. A link whose
// address cannot be read, as one that went away meanwhile, is left out.
func (s System) Links() ([]NamedLink, error) {
	dir := s.sys("class", "net")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("listing the links: %w", err)
	}
	var out []NamedLink
	for _, e := range entries {
		name := e.Name()
		if name == "lo" || name == dummyLink && !s.veth(name) {
			continue
		}
		mac, err := model.NormalizeMAC(s.read("class", "net", name, "address"))
		if err != nil || mac == "00:00:00:00:00:00" {
			continue
		}
		out = append(out, NamedLink{Name: name, MAC: mac})
	}
	slices.SortFunc(out, func(a, b NamedLink) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// veth reports whether a link has a peer: its iflink is not itself, as for
// the ethN@ifM of a container.
func (s System) veth(name string) bool {
	index, link := s.read("class", "net", name, "ifindex"), s.read("class", "net", name, "iflink")
	return index != "" && link != "" && index != link
}

// Uptime is the uptime of the container, the first field of /proc/uptime,
// which lxcfs answers for the container.
func (s System) Uptime() (time.Duration, error) {
	b, err := os.ReadFile(s.proc("uptime"))
	if err != nil {
		return 0, fmt.Errorf("reading the uptime: %w", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("%s is empty", s.proc("uptime"))
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || secs < 0 {
		return 0, fmt.Errorf("the uptime %q is not a number of seconds", fields[0])
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// Facts reads what the container can show about itself, with the mount of
// its state volume at volume.
func (s System) Facts(volume string) (Facts, error) {
	mount, err := s.MountSource(volume)
	if err != nil {
		return Facts{}, err
	}
	up, err := s.Uptime()
	if err != nil {
		return Facts{}, err
	}
	links, err := s.Links()
	if err != nil {
		return Facts{}, err
	}
	return Facts{Mount: mount, Uptime: up, Links: links}, nil
}

// Identity is what the appliance knows of itself from its install record.
type Identity struct {
	VMID int
	Node string
	MACs []string // normalised, sorted, as recorded at init
}

// Ref is the guest the appliance was installed as.
func (id Identity) Ref() model.GuestRef { return model.GuestRef{Kind: model.KindLXC, VMID: id.VMID} }

// Facts are what the running container can show about itself.
type Facts struct {
	Mount  Source        // of /var/lib/pco
	Uptime time.Duration // the container's /proc/uptime (lxcfs)
	Links  []NamedLink
}

// Verdict is the outcome of one self-identification.
type Verdict struct {
	OK      bool
	Why     string           // when !OK
	Copy    bool             // positive evidence of being a copy: stop serving (ruling 23)
	Copies  []model.GuestRef // running guests that carry one of our MACs while we are not proven, or in pool pco while we are
	Tenants []model.GuestRef // once proven: running guests outside the pool with one of our MACs; their routes are rejected
	Pending bool             // a NIC appeared or vanished within the last 60 s
}

// carrier is another running guest with a MAC of the appliance.
type carrier struct {
	ref  model.GuestRef
	mac  string
	pool string
}

// Check proves that this container is the recorded VMID by a fact a copy
// cannot share (the mount source names a volume of the VMID; a source that
// names another VMID is Copy at once; a source of no known form leaves it to
// the uptime, within 10 s of the VMID's uptime in uptimes, except while another
// running guest carries one of our MACs, when the uptime proves nothing (G1);
// neither proving it while uptimes was read is Copy), and that the links carry
// exactly the MACs of the VMID's NICs in snap (a difference is Pending for 60 s
// from the first fresh config that showed it; then Copy when the mount does not
// prove the VMID, and not OK without Copy when it does: recommended 1, Q2). A
// complete snapshot that does
// not list the VMID is Copy. While another running guest carries one of our
// MACs and the VMID is not proven, the verdict is not OK whatever its pool.
// Once proven, a running guest in pool pco with one of our MACs is a copy of
// us: the verdict is not OK (writes held, spec section 2) without Copy (we keep
// serving), and Copies names it; one outside the pool is a tenant.
// An incomplete snapshot or missing uptimes is not OK without Copy.
//
// firstDiff is kept by the caller from one check to the next: when the
// difference of the MACs was first seen, zero while there is none.
func (id Identity) Check(snap inventory.Snapshot, f Facts, uptimes map[model.GuestRef]time.Duration, now time.Time, firstDiff *time.Time) Verdict {
	self := id.Ref()
	if f.Mount.VMID != 0 && f.Mount.VMID != id.VMID {
		return Verdict{Copy: true, Why: fmt.Sprintf("the volume at /var/lib/pco is %s, a volume of VMID %d and not of %s: this container is a copy",
			f.Mount.Volume, f.Mount.VMID, self)}
	}
	if !snap.Complete {
		return Verdict{Why: WhyIncomplete}
	}
	own, listed := snap.Guest(self)
	if !listed {
		return Verdict{Copy: true, Why: fmt.Sprintf("Proxmox lists no %s: this container is not the one installed", self)}
	}
	if uptimes == nil {
		return Verdict{Why: "the uptimes of the guests could not be read; self-identification waits for them"}
	}
	links, config := linkMACs(f.Links), nicMACs(own)
	others := carriers(snap, self, slices.Concat(links, config))
	byMount := f.Mount.VMID == id.VMID
	if why := id.unproven(f, byMount, uptimes, others); why != "" {
		return Verdict{Copy: true, Why: why, Copies: refsOf(others)}
	}

	v := Verdict{OK: true}
	for _, c := range others {
		if c.pool == Pool || own.Pool != "" && c.pool == own.Pool {
			v.Copies = append(v.Copies, c.ref)
		} else {
			v.Tenants = append(v.Tenants, c.ref)
		}
	}
	if !slices.Equal(links, config) {
		if firstDiff.IsZero() {
			*firstDiff = now
		}
		mismatch := fmt.Sprintf("the links of this container carry %s, but %s in Proxmox has %s", macList(links), self, macList(config))
		switch {
		case now.Sub(*firstDiff) < macTolerance:
			v.Pending = true
		case byMount:
			// A NIC configured without a link must not cut the appliance off.
			v.OK, v.Why = false, mismatch+" for more than 60 s; writes are held until they match"
			return v
		default:
			return Verdict{Copy: true, Why: mismatch + " for more than 60 s, and nothing but the uptime proves this container is " + self.String()}
		}
	} else {
		*firstDiff = time.Time{}
	}
	if len(v.Copies) > 0 {
		v.OK, v.Pending = false, false
		v.Why = fmt.Sprintf("%s in pool %s carries a MAC of %s: a copy of the appliance runs; writes are held until it is gone",
			joinRefs(v.Copies), Pool, self)
	}
	return v
}

// unproven says why nothing proves that this container is the VMID, or is
// empty when something does: the mount, or else the uptime while no other
// guest carries our MAC.
func (id Identity) unproven(f Facts, byMount bool, uptimes map[model.GuestRef]time.Duration, others []carrier) string {
	if byMount {
		return ""
	}
	self := id.Ref()
	volume := fmt.Sprintf("the volume at /var/lib/pco (%s) has no form pco knows", f.Mount.Raw)
	if len(others) > 0 {
		return fmt.Sprintf("%s carries the MAC %s of this container and %s: nothing proves that this container is %s",
			others[0].ref, others[0].mac, volume, self)
	}
	up, ok := uptimes[self]
	switch {
	case !ok:
		return fmt.Sprintf("Proxmox reports no uptime of %s and %s: nothing proves that this container is %s", self, volume, self)
	case max(up-f.Uptime, f.Uptime-up) > uptimeSlack:
		return fmt.Sprintf("this container has been up for %s and %s for %s, and %s: nothing proves that this container is %s",
			f.Uptime.Round(time.Second), self, up.Round(time.Second), volume, self)
	}
	return ""
}

// carriers lists the running guests other than self that carry one of macs,
// by guest.
func carriers(snap inventory.Snapshot, self model.GuestRef, macs []string) []carrier {
	var out []carrier
	for _, g := range snap.Guests {
		if g.Ref == self || !g.Running || g.Template {
			continue
		}
		for _, nic := range g.NICs {
			if slices.Contains(macs, nic.MAC) {
				out = append(out, carrier{ref: g.Ref, mac: nic.MAC, pool: g.Pool})
				break
			}
		}
	}
	return out
}

func linkMACs(links []NamedLink) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.MAC)
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

func nicMACs(g model.Guest) []string {
	out := make([]string, 0, len(g.NICs))
	for _, n := range g.NICs {
		if n.MAC != "" {
			out = append(out, n.MAC)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

func refsOf(cs []carrier) []model.GuestRef {
	var out []model.GuestRef
	for _, c := range cs {
		out = append(out, c.ref)
	}
	return out
}

func joinRefs(refs []model.GuestRef) string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.String()
	}
	return strings.Join(names, ", ")
}

func macList(macs []string) string {
	if len(macs) == 0 {
		return "no MAC"
	}
	return strings.Join(macs, ", ")
}

// Segments maps the container's interface names to the segments of the VMID's
// NICs in the snapshot by MAC: eth0 -> {vmbr0, 10} for net0 with hwaddr=eth0's
// MAC, bridge=vmbr0, tag=10.
func (id Identity) Segments(snap inventory.Snapshot, links []NamedLink) map[string]resolve.Segment {
	out := map[string]resolve.Segment{}
	own, ok := snap.Guest(id.Ref())
	if !ok {
		return out
	}
	for _, l := range links {
		for _, nic := range own.NICs {
			if nic.MAC == l.MAC && nic.Bridge != "" {
				out[l.Name] = resolve.SegmentOf(nic)
				break
			}
		}
	}
	return out
}

// WriteFlag writes the identity flag at path, a file of root with mode 0644 in
// a directory with mode 0755.
func WriteFlag(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("making the directory of the identity flag: %w", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return fmt.Errorf("writing the identity flag: %w", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("writing the identity flag: %w", err)
	}
	return nil
}

// RemoveFlag removes the identity flag at path; one that is not there is
// removed already.
func RemoveFlag(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the identity flag: %w", err)
	}
	return nil
}
