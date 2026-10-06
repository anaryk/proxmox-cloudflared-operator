package doctor

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/upgrade"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

const (
	cloudflareBase = "https://api.cloudflare.com"
	// cloudflareProbe is a path anyone may read: what matters of the answer is
	// that there is one, and its Date.
	cloudflareProbe = "/client/v4/ips"

	aptMarkPath   = "/usr/bin/apt-mark"
	aptConfigPath = "/usr/bin/apt-config"

	// maxCommandOutput is how much of what a command prints is kept.
	maxCommandOutput = 64 << 10
)

// ApplianceProxmox is the part of the Proxmox client the doctor of an
// appliance asks; *pve.Client satisfies it.
type ApplianceProxmox interface {
	engine.Access
	GuestConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error)
	PendingConfig(ctx context.Context, node string, ref model.GuestRef) (map[string]string, error)
	Snapshots(ctx context.Context, node string, ref model.GuestRef) ([]pve.Snapshot, error)
	Replication(ctx context.Context) ([]pve.ReplicationJob, error)
	DatacenterFirewall(ctx context.Context) (pve.FirewallOptions, error)
	NodeFirewall(ctx context.Context, node string) (pve.FirewallOptions, error)
	NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error)
	VNets(ctx context.Context) ([]pve.VNet, error)
	LastVerifyError() error
}

// ApplianceHost is the ApplianceEnv of the daemon inside the container. What
// is left empty is read from the running system.
type ApplianceHost struct {
	// Host sets the deadline of every question and asks systemd and
	// cloudflared; Proxmox is the client of the API of the node and Install
	// what the install records of the appliance.
	Host    *HostEnv
	Proxmox ApplianceProxmox
	Install Self

	// Root is the root of the paths below /etc and /proc and /var that are
	// read; empty is /.
	Root string
	// StateDir is the mount of the state volume; empty is /var/lib/pco. Volume
	// checks it, as appliance.VolumeMounted does.
	StateDir string
	Volume   func(path, marker string) error
	// Netlink and NetNft are the network pco-net.service loads and its table.
	Netlink appnet.Netlink
	NetNft  egress.Nft
	// Probe connects to an address as pco-connector; nil runs pco egress
	// probe as that user.
	Probe func(ctx context.Context, addr string) error
	// CloudflareURL is the base URL of the Cloudflare API, empty for
	// Cloudflare; HTTP is the client that asks it, nil for a default one.
	CloudflareURL string
	HTTP          *http.Client
	// Run runs a command and returns what it printed; nil runs it.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// ManifestPath is the list of vetted versions of cloudflared; empty is the
	// one the package ships.
	ManifestPath string
	// Resolve reads the name servers and LookupHost asks them; nil is
	// /etc/resolv.conf and the resolver of Go.
	Resolve    func() ([]netip.Addr, error)
	LookupHost func(ctx context.Context, name string) ([]string, error)
	// Reading is called, with what is read, at the start of every read of the
	// container's own system; a test blocks in it as a stalled storage does.
	Reading func(what string)

	mu      sync.Mutex
	stalled map[string]int // by what is read: the reads given up on that have not returned
}

var _ ApplianceEnv = (*ApplianceHost)(nil)

func (h *ApplianceHost) timeout() time.Duration {
	if h.Host != nil {
		return h.Host.timeout()
	}
	return hostTimeout
}

func (h *ApplianceHost) within(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, h.timeout())
}

func (h *ApplianceHost) path(p string) string { return filepath.Join(h.Root, p) }

// local runs a read of the container's own system within the deadline of the
// host. A read that stalls, as one of a storage that does not answer does, ends
// the question with an error and is left to finish on its own; while it still
// runs, a read of the same thing fails at once, so that the doctors that follow
// do not pile up reads that wait on the same storage. The context of read ends
// when local is done with it.
func (h *ApplianceHost) local(what string, read func(ctx context.Context) error) error {
	h.mu.Lock()
	if h.stalled[what] > 0 {
		h.mu.Unlock()
		return fmt.Errorf("no answer: an earlier read of it has not returned yet: %w", context.DeadlineExceeded)
	}
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// ended and gaveUp are guarded by h.mu: a read that ends as the deadline
	// passes is answered, and one that does not is counted until it ends.
	var ended, gaveUp bool
	done := make(chan error, 1)
	go func() {
		defer func() {
			h.mu.Lock()
			ended = true
			if gaveUp {
				h.stalled[what]--
			}
			h.mu.Unlock()
		}()
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("%v", p)
			}
		}()
		if h.Reading != nil {
			h.Reading(what)
		}
		done <- read(ctx)
	}()
	timer := time.NewTimer(h.timeout())
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ended {
		return <-done
	}
	gaveUp = true
	if h.stalled == nil {
		h.stalled = map[string]int{}
	}
	h.stalled[what]++
	return fmt.Errorf("no answer within %s: %w", h.timeout(), context.DeadlineExceeded)
}

func (h *ApplianceHost) stateDir() string {
	if h.StateDir != "" {
		return h.StateDir
	}
	return store.ApplianceLocal
}

func (h *ApplianceHost) own() model.GuestRef {
	return model.GuestRef{Kind: model.KindLXC, VMID: h.Install.VMID}
}

func (h *ApplianceHost) Self() Self { return h.Install }

func (h *ApplianceHost) OwnConfig(ctx context.Context) (pve.GuestConfig, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return h.Proxmox.GuestConfig(ctx, h.Install.Node, h.own())
}

func (h *ApplianceHost) OwnPending(ctx context.Context) (map[string]string, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return h.Proxmox.PendingConfig(ctx, h.Install.Node, h.own())
}

func (h *ApplianceHost) UnitFileState(ctx context.Context, unit string) (string, error) {
	if h.Host == nil {
		ctx, cancel := h.within(ctx)
		defer cancel()
		return systemctlState(ctx, unit)
	}
	return h.Host.UnitFileState(ctx, unit)
}

func (h *ApplianceHost) Snapshots(ctx context.Context) ([]pve.Snapshot, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return h.Proxmox.Snapshots(ctx, h.Install.Node, h.own())
}

func (h *ApplianceHost) Replication(ctx context.Context) ([]pve.ReplicationJob, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return h.Proxmox.Replication(ctx)
}

func (h *ApplianceHost) OwnPermissions(ctx context.Context) (map[string][]string, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return h.Proxmox.Permissions(ctx, "", "/")
}

// AccessData reads the access control as the engine does, which fails when
// the token cannot see all of it.
func (h *ApplianceHost) AccessData(ctx context.Context) (access.Data, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return engine.ReadAccess(ctx, h.Proxmox)
}

func (h *ApplianceHost) DatacenterFirewall(ctx context.Context) (bool, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	o, err := h.Proxmox.DatacenterFirewall(ctx)
	return o.Enabled, err
}

func (h *ApplianceHost) NodeFirewall(ctx context.Context) (bool, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	o, err := h.Proxmox.NodeFirewall(ctx, h.Install.Node)
	return o.Enabled, err
}

func (h *ApplianceHost) VolumeMounted() error {
	check := h.Volume
	if check == nil {
		check = appliance.VolumeMounted
	}
	return h.local("the volume", func(context.Context) error { return check(h.stateDir(), store.VolumeMarker) })
}

func (h *ApplianceHost) VerifyError() error { return h.Proxmox.LastVerifyError() }

func (h *ApplianceHost) NetVerify(ctx context.Context) error {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return appnet.Verify(ctx, h.Netlink, h.NetNft)
}

func (h *ApplianceHost) NetLeaked(ctx context.Context) (uint64, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	c, err := appnet.Leaked(ctx, h.NetNft)
	return c.Packets, err
}

func (h *ApplianceHost) ProbeAsConnector(ctx context.Context, addr string) error {
	if h.Probe != nil {
		return h.Probe(ctx, addr)
	}
	var exe string
	var cred *syscall.Credential
	err := h.local("the user "+egress.ConnectorUser, func(context.Context) (err error) {
		exe, cred, err = probeTarget("")
		return err
	})
	if err != nil {
		return err
	}
	return runProbe(ctx, exe, addr, cred, probeTimeout)
}

// CloudflareDate asks the Cloudflare API for something anyone may read and
// returns the Date of the answer, whatever its status.
func (h *ApplianceHost) CloudflareDate(ctx context.Context) (time.Time, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	base := h.CloudflareURL
	if base == "" {
		base = cloudflareBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+cloudflareProbe, nil)
	if err != nil {
		return time.Time{}, err
	}
	req.Header.Set("User-Agent", "pco-doctor")
	client := h.HTTP
	if client == nil {
		client = &http.Client{Timeout: h.timeout()}
	}
	res, err := client.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4<<10))
	date, err := http.ParseTime(res.Header.Get("Date"))
	if err != nil {
		return time.Time{}, fmt.Errorf("it answered %s without a usable Date header", res.Status)
	}
	return date, nil
}

func (h *ApplianceHost) run(ctx context.Context, name string, args ...string) (string, error) {
	if h.Run != nil {
		return h.Run(ctx, name, args...)
	}
	return runCommand(ctx, name, args...)
}

// runCommand runs a command in the C locale, without a shell, and returns what
// it printed; on failure the error carries what it said.
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr capped
	stdout.max, stderr.max = maxCommandOutput, maxVersionOutput
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return stdout.String(), nil
}

func (h *ApplianceHost) Holds(ctx context.Context) ([]string, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	out, err := h.run(ctx, aptMarkPath, "showhold")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// UnattendedUpgrades says whether the unit is enabled and apt is told to run
// it every day.
func (h *ApplianceHost) UnattendedUpgrades(ctx context.Context) (bool, error) {
	state, err := h.UnitFileState(ctx, "unattended-upgrades.service")
	if err != nil {
		return false, err
	}
	if state != "enabled" && state != "enabled-runtime" {
		return false, nil
	}
	ctx, cancel := h.within(ctx)
	defer cancel()
	out, err := h.run(ctx, aptConfigPath, "dump", "APT::Periodic::Unattended-Upgrade")
	if err != nil {
		return false, err
	}
	return periodicOn(out), nil
}

// periodicOn reads what apt-config dump printed of APT::Periodic::Unattended-Upgrade:
// a line `APT::Periodic::Unattended-Upgrade "1";`. Any number above 0 is on.
func periodicOn(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found || key != "APT::Periodic::Unattended-Upgrade" {
			continue
		}
		n, err := strconv.Atoi(strings.Trim(strings.TrimSuffix(strings.TrimSpace(value), ";"), `"`))
		return err == nil && n > 0
	}
	return false
}

func (h *ApplianceHost) NodeNetwork(ctx context.Context) ([]pve.NodeIface, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return h.Proxmox.NodeNetwork(ctx, h.Install.Node)
}

// DiskUse is what is used of the filesystem that holds path, and its size, as
// df counts them: what root may not use is no part of the size.
func (h *ApplianceHost) DiskUse(path string) (used, total uint64, err error) {
	err = h.local("the use of "+path, func(context.Context) error {
		var st syscall.Statfs_t
		if err := syscall.Statfs(h.path(path), &st); err != nil {
			return err
		}
		size := uint64(st.Bsize)
		used = (st.Blocks - st.Bfree) * size
		total = used + st.Bavail*size
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return used, total, nil
}

// journalDirs are where the journal is kept: on disk, or in memory until the
// disk is there.
var journalDirs = []string{"/var/log/journal", "/run/log/journal"}

// JournalUse is the size of the files of the journal.
func (h *ApplianceHost) JournalUse(ctx context.Context) (uint64, error) {
	var total uint64
	err := h.local("the journal", func(reading context.Context) error {
		for _, dir := range journalDirs {
			err := filepath.WalkDir(h.path(dir), func(_ string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if err := cmp.Or(ctx.Err(), reading.Err()); err != nil {
					return err
				}
				if d.Type().IsRegular() {
					info, err := d.Info()
					if err != nil {
						return err
					}
					total += uint64(info.Size())
				}
				return nil
			})
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// cgroupDir is where the container sees its own cgroup, whose root is the
// cgroup the container is in.
const cgroupDir = "/sys/fs/cgroup"

// MemoryPressure reads the pressure of the container's own cgroup and what it
// uses of its limit, and where the cgroup does not report pressure, that of
// the node, which the container shares.
func (h *ApplianceHost) MemoryPressure() (MemoryState, error) {
	var m MemoryState
	err := h.local("memory", func(context.Context) error {
		own, ownErr := h.readPressure(cgroupDir + "/memory.pressure")
		if ownErr == nil {
			m = MemoryState{Some10: own, Own: true}
			m.Used, _ = h.readNumber(cgroupDir + "/memory.current")
			m.Limit, _ = h.readNumber(cgroupDir + "/memory.max")
			return nil
		}
		node, err := h.readPressure("/proc/pressure/memory")
		if err != nil {
			return err
		}
		m = MemoryState{Some10: node}
		return nil
	})
	return m, err
}

// readPressure reads the average of ten seconds of the tasks that wait for
// memory from a file of pressure.
func (h *ApplianceHost) readPressure(path string) (float64, error) {
	f, err := os.Open(h.path(path))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(io.LimitReader(f, 4<<10))
	for sc.Scan() {
		if some, found := pressureOf(sc.Text()); found {
			return some, nil
		}
	}
	return 0, fmt.Errorf("%s has no line of some tasks", path)
}

// readNumber reads a file that holds a number of bytes; max, which a cgroup
// without a limit says, and what cannot be read are 0.
func (h *ApplianceHost) readNumber(path string) (uint64, error) {
	b, err := os.ReadFile(h.path(path))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// pressureOf reads avg10 of a line "some avg10=0.00 avg60=0.00 avg300=0.00 total=0".
func pressureOf(line string) (float64, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "some" {
		return 0, false
	}
	for _, f := range fields[1:] {
		if v, found := strings.CutPrefix(f, "avg10="); found {
			n, err := strconv.ParseFloat(v, 64)
			return n, err == nil
		}
	}
	return 0, false
}

func (h *ApplianceHost) Versions(ctx context.Context) (pco, cloudflared, debian string, err error) {
	debian = "unknown"
	err = h.local("the version of Debian", func(context.Context) error {
		if b, err := os.ReadFile(h.path("/etc/debian_version")); err == nil {
			debian = strings.TrimSpace(string(b))
		}
		return nil
	})
	if err != nil {
		return "", "", "", err
	}
	line, err := h.cloudflaredLine(ctx)
	if err != nil {
		return "", "", "", err
	}
	cloudflared = line
	if m := cloudflaredVersion.FindString(line); m != "" {
		cloudflared = m
	}
	return version.Version, cloudflared, debian, nil
}

func (h *ApplianceHost) cloudflaredLine(ctx context.Context) (string, error) {
	if h.Host == nil {
		return "", errors.New("cloudflared is not asked")
	}
	return h.Host.CloudflaredVersion(ctx)
}

func (h *ApplianceHost) Manifest() (upgrade.Manifest, error) {
	path := h.ManifestPath
	if path == "" {
		path = h.path(upgrade.ShippedManifest)
	}
	var m upgrade.Manifest
	err := h.local("the list of vetted cloudflared versions", func(context.Context) (err error) {
		m, err = upgrade.LoadManifest(path)
		return err
	})
	return m, err
}

func (h *ApplianceHost) Resolvers() ([]netip.Addr, error) {
	read := h.Resolve
	if read == nil {
		read = egress.SystemResolvers
	}
	var servers []netip.Addr
	err := h.local("the name servers", func(context.Context) (err error) {
		servers, err = read()
		return err
	})
	return servers, err
}

func (h *ApplianceHost) Lookup(ctx context.Context, name string) error {
	ctx, cancel := h.within(ctx)
	defer cancel()
	lookup := h.LookupHost
	if lookup == nil {
		lookup = net.DefaultResolver.LookupHost
	}
	addrs, err := lookup(ctx, name)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return errors.New("it answered without an address")
	}
	return nil
}

func (h *ApplianceHost) EtcPVE() (bool, error) {
	var there bool
	err := h.local("/etc/pve", func(context.Context) error {
		_, err := os.Stat(h.path("/etc/pve"))
		there = err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
	return there, err
}

func (h *ApplianceHost) RmemMax() (int, error) {
	var n int
	err := h.local("net.core.rmem_max", func(context.Context) error {
		b, err := os.ReadFile(h.path("/proc/sys/net/core/rmem_max"))
		if err != nil {
			return err
		}
		n, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err
	})
	return n, err
}

func (h *ApplianceHost) VNets(ctx context.Context) ([]pve.VNet, error) {
	ctx, cancel := h.within(ctx)
	defer cancel()
	return h.Proxmox.VNets(ctx)
}
