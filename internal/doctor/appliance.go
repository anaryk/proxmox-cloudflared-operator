package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/upgrade"
)

// Self is what the install records of the appliance, which its checks name in
// what they say and in what to do.
type Self struct {
	VMID       int
	Node       string
	Address    string // host:port of the Proxmox API
	ServerName string // the name its certificate is verified under
	User       string // the user of pco's token, pco@pve
	Token      string // the token of the appliance, pco@pve!vm<vmid>
}

// ErrRefused is what ProbeAsConnector returns for a connection that was
// refused, as the egress filter refuses what it does not allow.
var ErrRefused = errors.New("connection refused")

// ApplianceEnv abstracts what the doctor reads of an appliance besides what
// every profile has: its container in Proxmox, its volume, its access control
// and its network. A Run on the state of an appliance has one through the Env
// (see HostEnv.Appliance); every call is made within the deadline the
// implementation sets.
type ApplianceEnv interface {
	Self() Self
	// OwnConfig is the configuration the container runs with, OwnPending the
	// one it will have after its next start.
	OwnConfig(ctx context.Context) (pve.GuestConfig, error)
	OwnPending(ctx context.Context) (map[string]string, error)
	// UnitFileState is the word systemctl is-enabled prints: enabled,
	// disabled, masked, static, and not-found for a unit there is none of.
	UnitFileState(ctx context.Context, unit string) (string, error)
	Snapshots(ctx context.Context) ([]pve.Snapshot, error)
	Replication(ctx context.Context) ([]pve.ReplicationJob, error)
	// OwnPermissions are the privileges of pco's token by path.
	OwnPermissions(ctx context.Context) (map[string][]string, error)
	AccessData(ctx context.Context) (access.Data, error)
	DatacenterFirewall(ctx context.Context) (bool, error)
	NodeFirewall(ctx context.Context) (bool, error)
	// VolumeMounted is nil when /var/lib/pco is a mount of its own with the
	// marker of the volume.
	VolumeMounted() error
	// VerifyError is why the certificate of the Proxmox API did not verify on
	// the last request; nil when it did.
	VerifyError() error
	// NetVerify checks what pco-net.service loads, NetLeaked counts what the
	// table of the service prefix rejected.
	NetVerify(ctx context.Context) error
	NetLeaked(ctx context.Context) (uint64, error)
	// ProbeAsConnector connects over TCP to addr as the user pco-connector,
	// which the egress filter confines: nil for a connection, ErrRefused for
	// one that was refused.
	ProbeAsConnector(ctx context.Context, addr string) error
	// CloudflareDate is the Date header api.cloudflare.com answers with.
	CloudflareDate(ctx context.Context) (time.Time, error)
	// Holds are the packages held by apt.
	Holds(ctx context.Context) ([]string, error)
	UnattendedUpgrades(ctx context.Context) (bool, error)
	NodeNetwork(ctx context.Context) ([]pve.NodeIface, error)
	DiskUse(path string) (used, total uint64, err error)
	JournalUse(ctx context.Context) (uint64, error)
	// VNets are the SDN vnets with their zones, which the access control
	// names a vnet by.
	VNets(ctx context.Context) ([]pve.VNet, error)
	// MemoryPressure is what the kernel says of memory pressure, and of the
	// container's own use; an error that is fs.ErrNotExist when it says
	// nothing of pressure.
	MemoryPressure() (MemoryState, error)
	// Versions are those of pco, of cloudflared and of Debian.
	Versions(ctx context.Context) (pco, cloudflared, debian string, err error)
	// Manifest is the list of vetted cloudflared versions the package of pco
	// ships.
	Manifest() (upgrade.Manifest, error)
	// Resolvers are the name servers of the container, Lookup asks them for a
	// name.
	Resolvers() ([]netip.Addr, error)
	Lookup(ctx context.Context, name string) error
	// EtcPVE says whether /etc/pve exists in the container; it is an error
	// when that is not answered, as a mount that stalls does not.
	EtcPVE() (bool, error)
	// RmemMax is net.core.rmem_max.
	RmemMax() (int, error)
}

// MemoryState is memory pressure as the kernel reports it. Some10 is the share
// of the last 10 seconds, in percent, in which some task waited for memory:
// the container's own when Own, else the node's, which the container shares.
// Used and Limit are the use of the container's memory and its limit, 0 when
// there is none or the cgroup does not say.
type MemoryState struct {
	Some10 float64
	Own    bool
	Used   uint64
	Limit  uint64
}

// applianceHost is an Env that has the part of an appliance.
type applianceHost interface {
	Appliance() ApplianceEnv
}

// applianceOf is the part of the appliance of env, or nil on a host and for an
// Env that has none. A state that does not say yet which profile it is of is
// taken for an appliance's when env has the part: only the daemon of an
// appliance gives it.
func applianceOf(st engine.State, env Env) ApplianceEnv {
	h, ok := env.(applianceHost)
	if !ok || st.Profile == store.ProfileHost {
		return nil
	}
	return h.Appliance()
}

// repairFix is what puts the volume or the state of the appliance back, on
// the node.
func repairFix(vmid int) string { return RepairFix(strconv.Itoa(vmid)) }

// RepairFix is repairFix for a VMID that is told as text, "<vmid>" when it is
// not known.
func RepairFix(vmid string) string {
	return "run pco appliance repair --vmid " + vmid + " on the node"
}

const (
	stateDir = store.ApplianceLocal

	wantFeatures = "nesting=1"
	// snapshotTolerated is how long a snapshot taken before an upgrade stands.
	snapshotTolerated = 7 * 24 * time.Hour
	// manifestStale is how old the list of vetted cloudflared versions may be.
	manifestStale = 90 * 24 * time.Hour
	skewWarn      = 30 * time.Second
	skewFail      = 5 * time.Minute
	journalWarn   = 64 << 20
	pressureWarn  = 10.0
	rmemWanted    = 7500000

	fixAPI    = "the api and proxmox checks say why Proxmox does not answer"
	fixOnNode = "on the node: "

	networkHint = "a card there is on the segment of the node's own services"
)

var (
	// preUpgrade is the name of the snapshot an admin takes before an upgrade.
	preUpgrade = regexp.MustCompile(`^` + regexp.QuoteMeta(upgrade.SnapshotPrefix) + `(\d{8})$`)
	// mountPoint is the name of a mount point of the container: mp0, mp1, ...
	mountPoint = regexp.MustCompile(`^mp\d+$`)
)

// applianceRun is one run of the checks of the appliance. What several checks
// read is read once, and only when a check asks.
type applianceRun struct {
	ctx  context.Context
	st   engine.State
	env  Env
	app  ApplianceEnv
	self Self
	now  time.Time

	config  func() (pve.GuestConfig, error)
	pending func() (map[string]string, error)
	data    func() (access.Data, error)
	date    func() (time.Time, error)
	release func() (string, error)
	major   func() (int, error)
}

func newApplianceRun(ctx context.Context, st engine.State, env Env, app ApplianceEnv) *applianceRun {
	r := &applianceRun{
		ctx: ctx, st: st, env: env, app: app, self: app.Self(), now: env.Now(),
		config:  sync.OnceValues(func() (pve.GuestConfig, error) { return app.OwnConfig(ctx) }),
		pending: sync.OnceValues(func() (map[string]string, error) { return app.OwnPending(ctx) }),
		data:    sync.OnceValues(func() (access.Data, error) { return app.AccessData(ctx) }),
		date:    sync.OnceValues(func() (time.Time, error) { return app.CloudflareDate(ctx) }),
	}
	r.release = sync.OnceValues(func() (string, error) { return env.PVEVersion(ctx) })
	r.major = func() (int, error) {
		release, err := r.release()
		if err != nil {
			return 0, err
		}
		major, _, parsed := majorMinor(release)
		if !parsed {
			return 0, fmt.Errorf("cannot tell the release from %q", release)
		}
		return major, nil
	}
	return r
}

// applianceRuns are the checks of the appliance, each with the name of its
// findings: a fixed key.
var applianceRuns = []struct {
	name string
	run  func(*applianceRun) []Finding
}{
	{"access", one((*applianceRun).checkAccess)},
	{"api", one((*applianceRun).checkAPI)},
	{"clock", one((*applianceRun).checkClock)},
	{"cloudflare api", one((*applianceRun).checkCloudflareAPI)},
	{"cmode", one((*applianceRun).checkCmode)},
	{"disk", one((*applianceRun).checkDisk)},
	{"dns", one((*applianceRun).checkDNS)},
	{"egress probe", one((*applianceRun).checkEgressProbe)},
	{"epoch", (*applianceRun).checkEpoch},
	{"etc-pve", one((*applianceRun).checkEtcPVE)},
	{"features", one((*applianceRun).checkFeatures)},
	{"firewall", one((*applianceRun).checkFirewall)},
	{"holds", one((*applianceRun).checkHolds)},
	{"identity", one((*applianceRun).checkIdentity)},
	{"journal", one((*applianceRun).checkJournal)},
	{"memory", one((*applianceRun).checkMemory)},
	{"net", one((*applianceRun).checkNet)},
	{"network grants", one((*applianceRun).checkNetworkGrants)},
	{"protection", one((*applianceRun).checkProtection)},
	{"quic buffer", one((*applianceRun).checkQUIC)},
	{"segment access", one((*applianceRun).checkSegmentAccess)},
	{"snapshots", one((*applianceRun).checkSnapshots)},
	{"token", one((*applianceRun).checkToken)},
	{"unattended-upgrades", one((*applianceRun).checkUnattended)},
	{"versions", one((*applianceRun).checkVersions)},
	{"volume", one((*applianceRun).checkVolume)},
}

func one(check func(*applianceRun) Finding) func(*applianceRun) []Finding {
	return func(r *applianceRun) []Finding { return []Finding{check(r)} }
}

// checksBound is how long the checks of the appliance may take in all. A read
// that never returns, as one of a storage that stalls, ends in a failure of the
// check that made it, and does not hold the doctor.
var checksBound = 30 * time.Second

const fixStalled = "look for a mount or a storage of the container that does not answer: pct config <vmid> on the node lists them"

// findings runs the checks of the appliance at once, so that a node that
// answers slowly costs the time of its slowest question and not the sum of
// them. A check that panics is a failure of its own, and cannot take the daemon
// down; one that has not ended within checksBound is a failure, and is left to
// end on its own.
func (r *applianceRun) findings() []Finding {
	type result struct {
		findings []Finding
		stopped  error
	}
	done := make([]chan result, len(applianceRuns))
	for i, c := range applianceRuns {
		done[i] = make(chan result, 1)
		go func() {
			var res result
			defer func() {
				if p := recover(); p != nil {
					res.stopped = fmt.Errorf("%v", p)
				}
				done[i] <- res
			}()
			res.findings = c.run(r)
		}()
	}
	timer := time.NewTimer(checksBound)
	defer timer.Stop()
	late := false
	var out []Finding
	for i, c := range applianceRuns {
		var res result
		got := false
		if !late {
			select {
			case res, got = <-done[i]:
			case <-timer.C:
				late = true
			}
		}
		if late {
			select {
			case res, got = <-done[i]:
			default:
			}
		}
		switch {
		case !got:
			out = append(out, fail(c.name, fmt.Sprintf("no answer within %s: something it reads may be stalled", checksBound), r.stalledFix()))
		case res.stopped != nil:
			out = append(out, fail(c.name, "the check stopped on an internal error: "+res.stopped.Error(), "journalctl -u pco may say more; report it"))
		default:
			out = append(out, res.findings...)
		}
	}
	return out
}

// stalledFix is what to do about a check that did not end, with the VMID of
// the appliance.
func (r *applianceRun) stalledFix() string {
	return strings.ReplaceAll(fixStalled, "<vmid>", strconv.Itoa(r.self.VMID))
}

// together runs the functions at once and waits for all of them. A function
// that panics has its panic returned as an error.
func together(fns ...func()) []error {
	stopped := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if p := recover(); p != nil {
					stopped[i] = fmt.Errorf("%v", p)
				}
			}()
			fn()
		}()
	}
	wg.Wait()
	return stopped
}

// unread is the finding of a check that could not read what it checks.
func unread(check, what string, err error) Finding {
	return warn(check, fmt.Sprintf("%s could not be read: %v", what, err), fixAPI)
}

// shown is a value of the configuration as a sentence says it.
func shown(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

// specOption reads an option of a value of the configuration such as
// "name=eth0,bridge=vmbr1,tag=20".
func specOption(spec, key string) string {
	for _, part := range strings.Split(spec, ",") {
		if k, v, found := strings.Cut(part, "="); found && k == key {
			return v
		}
	}
	return ""
}

// withOption sets an option of a value of the configuration, in place when it
// is there and at the end when it is not.
func withOption(spec, key, value string) string {
	parts := strings.Split(spec, ",")
	for i, part := range parts {
		if k, _, found := strings.Cut(part, "="); found && k == key {
			parts[i] = key + "=" + value
			return strings.Join(parts, ",")
		}
	}
	return spec + "," + key + "=" + value
}

// sameFeatures says whether a value of features holds the same features as want.
func sameFeatures(v, want string) bool {
	norm := func(s string) []string {
		var out []string
		for _, f := range strings.Split(s, ",") {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(v), norm(want))
}

// list says a list of words: "a", "a and b", "a, b and c".
func list(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

func worse(a, b Level) Level {
	rank := map[Level]int{LevelOK: 0, LevelWarn: 1, LevelFail: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// iec says a number of bytes.
func iec(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ago says how long ago in the unit that fits.
func ago(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Hour:
		return count(int(d/time.Minute), "minute", "minutes")
	case d < 48*time.Hour:
		return count(int(d/time.Hour), "hour", "hours")
	}
	return count(int(d/(24*time.Hour)), "day", "days")
}

func (r *applianceRun) checkFeatures() Finding {
	const check = "features"
	cfg, err := r.config()
	if err != nil {
		return unread(check, "the configuration of the container", err)
	}
	pending, err := r.pending()
	if err != nil {
		return unread(check, "the pending configuration of the container", err)
	}
	now, next := cfg.Values["features"], pending["features"]
	nowOK, nextOK := sameFeatures(now, wantFeatures), sameFeatures(next, wantFeatures)
	switch {
	case !nowOK && !nextOK:
		return fail(check, fmt.Sprintf("the features are %s now and %s at the next start; only %s is allowed", shown(now), shown(next), wantFeatures),
			fmt.Sprintf("%spct set %d --features %s, then restart the container", fixOnNode, r.self.VMID, wantFeatures))
	case !nowOK:
		return fail(check, fmt.Sprintf("the features are %s now; the pending configuration has %s, which applies at the next start of the container", shown(now), wantFeatures),
			"restart the container")
	case !nextOK:
		return fail(check, fmt.Sprintf("the features are %s now, but the pending configuration has %s, which applies at the next start of the container", wantFeatures, shown(next)),
			fmt.Sprintf("%spct set %d --revert features", fixOnNode, r.self.VMID))
	}
	return ok(check, wantFeatures+", in the current and in the pending configuration")
}

func (r *applianceRun) checkProtection() Finding {
	const check = "protection"
	cfg, err := r.config()
	if err != nil {
		return unread(check, "the configuration of the container", err)
	}
	if cfg.Values["protection"] != "1" {
		return fail(check, "the container is not protected: it can be destroyed, with the volume of its state, by one command",
			fmt.Sprintf("%spct set %d --protection 1", fixOnNode, r.self.VMID))
	}
	return ok(check, "the container is protected against removal")
}

// checkCmode looks at the console mode now and at the next start: any one of
// the VM.Config privileges sets a pending shell, which a restart makes a root
// console without a password.
func (r *applianceRun) checkCmode() Finding {
	const check = "cmode"
	cfg, err := r.config()
	if err != nil {
		return unread(check, "the configuration of the container", err)
	}
	pending, err := r.pending()
	if err != nil {
		return unread(check, "the pending configuration of the container", err)
	}
	allowed := func(v string) bool { return v == "" || v == "tty" }
	name := func(v string) string {
		if v == "" {
			return "tty"
		}
		return v
	}
	now, next := cfg.Values["cmode"], pending["cmode"]
	switch {
	case !allowed(now) && !allowed(next):
		return fail(check, fmt.Sprintf("the console mode is %s now and %s at the next start; the console of the container is then a root shell without a password",
			name(now), name(next)),
			fmt.Sprintf("%spct set %d --cmode tty, then restart the container", fixOnNode, r.self.VMID))
	case !allowed(now):
		return fail(check, fmt.Sprintf("the console mode is %s now; the pending configuration has %s, which applies at the next start of the container", name(now), name(next)),
			"restart the container")
	case !allowed(next):
		return fail(check, fmt.Sprintf("the console mode is %s now, but the pending configuration has %s: the next start of the container makes its console "+
			"a root shell without a password, and any one VM.Config privilege sets it", name(now), name(next)),
			fmt.Sprintf("%spct set %d --revert cmode", fixOnNode, r.self.VMID))
	}
	return ok(check, "the console is a login on a tty, now and at the next start")
}

// checkVolume looks at the state volume: mounted on its own with its marker,
// as mp0 at /var/lib/pco, and kept out of backups.
func (r *applianceRun) checkVolume() Finding {
	const check = "volume"
	repair := repairFix(r.self.VMID)
	if err := r.app.VolumeMounted(); err != nil {
		detail := "cannot read " + stateDir + ": " + err.Error()
		switch {
		case errors.Is(err, appliance.ErrNoMarker):
			detail = err.Error() + " (a restore, or a volume that is not pco's?)"
		case errors.Is(err, appliance.ErrNotMountPoint):
			detail = err.Error() + " (a restore without the volume?)"
		}
		return fail(check, detail, repair)
	}
	cfg, err := r.config()
	if err != nil {
		return unread(check, "the configuration of the container", err)
	}
	mp0, found := cfg.Values["mp0"]
	switch {
	case !found:
		return fail(check, "the container has no mp0: the volume of its state is not part of it", repair)
	case specOption(mp0, "mp") != stateDir:
		return fail(check, fmt.Sprintf("mp0 is mounted at %s, not at %s", shown(specOption(mp0, "mp")), stateDir), repair)
	case specOption(mp0, "backup") != "0":
		return fail(check, "mp0 is not kept out of backups: a backup of the container would carry the Cloudflare credentials and the keys of the appliance",
			fmt.Sprintf("%spct set %d --mp0 %s", fixOnNode, r.self.VMID, withOption(mp0, "backup", "0")))
	}
	return ok(check, "mp0 is a mount of its own at "+stateDir+" with the volume marker, and is kept out of backups")
}

// snapshotKept is a snapshot that is not tolerated, as a sentence names it.
type snapshotKept struct{ name, shown string }

// checkSnapshots warns of what carries a copy of the state volume, and with it
// the credentials: a snapshot, bar the one an admin takes before an upgrade
// for a week, and a replication job.
func (r *applianceRun) checkSnapshots() Finding {
	const check = "snapshots"
	snaps, err := r.app.Snapshots(r.ctx)
	if err != nil {
		return unread(check, "the snapshots of the container", err)
	}
	jobs, err := r.app.Replication(r.ctx)
	if err != nil {
		return unread(check, "the replication jobs", err)
	}
	var kept []snapshotKept
	var tolerated []string
	var lastAge time.Duration
	for _, s := range snaps {
		date, named := preUpgradeDate(s.Name)
		if !named {
			kept = append(kept, snapshotKept{s.Name, s.Name})
			continue
		}
		taken := s.Time
		if taken.IsZero() {
			taken = date
		}
		age := r.now.Sub(taken)
		if age >= snapshotTolerated {
			kept = append(kept, snapshotKept{s.Name, fmt.Sprintf("%s (%s old: a snapshot before an upgrade is kept for 7 days)", s.Name, days(age))})
			continue
		}
		tolerated = append(tolerated, s.Name)
		lastAge = age
	}
	var copying []pve.ReplicationJob
	for _, j := range jobs {
		if j.Guest.VMID == r.self.VMID {
			copying = append(copying, j)
		}
	}
	if len(kept) == 0 && len(copying) == 0 {
		if len(tolerated) == 0 {
			return ok(check, "the container has no snapshot, and no replication job copies it")
		}
		if len(tolerated) == 1 {
			return ok(check, fmt.Sprintf("the only snapshot is %s, taken before an upgrade, %s ago, and no replication job copies the container",
				tolerated[0], ago(lastAge)))
		}
		return ok(check, fmt.Sprintf("the only snapshots are %s, taken before upgrades in the last 7 days, and no replication job copies the container",
			list(tolerated)))
	}
	var details, fixes []string
	if len(kept) > 0 {
		names := make([]string, len(kept))
		for i, k := range kept {
			names[i] = k.shown
			fixes = append(fixes, fmt.Sprintf("pct delsnapshot %d %s", r.self.VMID, k.name))
		}
		details = append(details, fmt.Sprintf("%s of the container %s a copy of the volume of its state, with the Cloudflare credentials and the keys: %s",
			count(len(kept), "snapshot", "snapshots"), verb(len(kept), "holds", "hold"), strings.Join(names, ", ")))
	}
	for _, j := range copying {
		details = append(details, fmt.Sprintf("the replication job %s copies the container to %s", j.ID, j.Target))
		fixes = append(fixes, "pvesr delete "+j.ID)
	}
	return warn(check, strings.Join(details, "; "), fixOnNode+strings.Join(fixes, "; "))
}

// preUpgradeDate reads the day out of the name of a snapshot taken before an
// upgrade.
func preUpgradeDate(name string) (time.Time, bool) {
	m := preUpgrade.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	day, err := time.Parse("20060102", m[1])
	return day, err == nil
}

// checkIdentity says what the last self-identification of the appliance found.
func (r *applianceRun) checkIdentity() Finding {
	const check = "identity"
	id := r.st.Identity
	switch {
	case r.st.At.IsZero():
		return warn(check, "not known until the first cycle", "wait for the first cycle")
	case id == nil:
		return warn(check, "no self-identification has run yet", fixProblems)
	case id.Copy:
		return fail(check, id.Why+"; the connectors are stopped and the egress filter is empty",
			"stop this container if it is a copy; a restored container becomes the appliance with pco appliance repair --vmid <its vmid> on the node, once the original is gone")
	case !id.OK:
		return fail(check, cmp.Or(id.Why, "the self-identification did not pass"), fixProblems)
	}
	detail := fmt.Sprintf("this container is lxc/%d on %s", id.VMID, id.Node)
	if !id.CheckedAt.IsZero() {
		detail += fmt.Sprintf(", checked %s ago", max(r.now.Sub(id.CheckedAt), 0).Round(time.Second))
	}
	return ok(check, detail)
}

// checkToken compares what pco's token holds on / with what role PCO gives on
// the release of Proxmox: nothing less, and nothing more.
func (r *applianceRun) checkToken() Finding {
	const check = "token"
	perms, err := r.app.OwnPermissions(r.ctx)
	if err != nil {
		return fail(check, "the permissions of the token could not be read: "+err.Error(),
			fmt.Sprintf("check that the token %s exists and is not expired; %s", r.self.Token, repairFix(r.self.VMID)))
	}
	major, err := r.major()
	if err != nil {
		return warn(check, "which privileges role PCO gives could not be told: "+err.Error(), fixAPI)
	}
	want := slices.Sorted(slices.Values(setup.RolePrivileges(major)))
	held := slices.Sorted(slices.Values(perms["/"]))
	var missing, extra []string
	for _, p := range want {
		if !slices.Contains(held, p) {
			missing = append(missing, p)
		}
	}
	for _, p := range held {
		if !slices.Contains(want, p) {
			extra = append(extra, p)
		}
	}
	release := fmt.Sprintf("role PCO on Proxmox VE %d", major)
	switch {
	case len(missing) > 0:
		detail := fmt.Sprintf("the token lacks %s of %s", list(missing), release)
		if len(extra) > 0 {
			detail += fmt.Sprintf(", and holds %s besides", list(extra))
		}
		return fail(check, detail, fmt.Sprintf("%spveum role modify PCO --privs %s, and grant it on / to the user and the token: "+
			"pveum acl modify / --users %s --roles PCO; pveum acl modify / --tokens %s --roles PCO",
			fixOnNode, strings.Join(want, ","), r.self.User, r.self.Token))
	case len(extra) > 0:
		return warn(check, fmt.Sprintf("the token holds %s besides the privileges of %s", list(extra), release), r.dropExtra())
	}
	return ok(check, fmt.Sprintf("the token holds exactly the privileges of %s: %s", release, strings.Join(want, ", ")))
}

// dropExtra is what takes from pco's user and token what role PCO does not give
// on /: the lines of the access control that say it, where the doctor can read
// them, and else how to find them.
func (r *applianceRun) dropExtra() string {
	if d, err := r.data(); err == nil {
		var cmds []string
		for _, e := range d.ACL {
			if e.Path == "/" && e.Type == "user" && e.UGID == r.self.User && e.RoleID != setup.RoleID {
				cmds = append(cmds, fmt.Sprintf("pveum acl delete / --roles %s --users %s", e.RoleID, e.UGID))
			}
		}
		for _, e := range d.ACL {
			if e.Path == "/" && e.Type == "token" && e.UGID == r.self.Token && e.RoleID != setup.RoleID {
				cmds = append(cmds, fmt.Sprintf("pveum acl delete / --roles %s --tokens %s", e.RoleID, e.UGID))
			}
		}
		if len(cmds) > 0 {
			return fixOnNode + strings.Join(cmds, "; ")
		}
	}
	return fmt.Sprintf("%spveum acl list shows what %s and its token hold on /; delete what role PCO does not give with "+
		"pveum acl delete / --roles <role> --users %s, and the same with --tokens %s", fixOnNode, r.self.User, r.self.User, r.self.Token)
}

// checkAccess fails while a principal other than an admin and pco's own can
// reach into the appliance: the connectors are stopped for as long.
func (r *applianceRun) checkAccess() Finding {
	const check = "access"
	d, err := r.data()
	if err != nil {
		return fail(check, fmt.Sprintf("the access control of Proxmox cannot be read: %v; no connector starts until it is read, "+
			"and nothing says that no principal can reach into the appliance", err),
			fmt.Sprintf("check that the token %s holds role PCO on / (pveum acl list)", r.self.Token))
	}
	refused := access.Refused(d, r.self.VMID, appliance.Pool, r.self.User)
	if len(refused) == 0 {
		return ok(check, "no principal other than an admin and pco's own can reach into the appliance")
	}
	var details, cmds, notes []string
	for _, p := range refused {
		details = append(details, fmt.Sprintf("%s holds %s on the appliance lxc/%d", p.ID, strings.Join(p.Privs, ", "), r.self.VMID))
		flag, who := "--users", p.ID
		if user, name, isToken := strings.Cut(p.ID, "!"); isToken {
			if tokenSeparates(d, user, name) {
				flag = "--tokens"
			} else {
				who = user
				notes = append(notes, fmt.Sprintf("%s is not privilege-separated: it holds the roles of %s, so the command names the user", p.ID, user))
			}
		}
		for _, path := range p.At {
			if cmd := fmt.Sprintf("pveum acl modify %s %s %s --roles NoAccess", path, flag, who); !slices.Contains(cmds, cmd) {
				cmds = append(cmds, cmd)
			}
		}
	}
	fix := strings.Join(cmds, "; ")
	if len(notes) > 0 {
		fix += " (" + strings.Join(notes, "; ") + ")"
	}
	return fail(check, strings.Join(details, "; ")+"; the connectors are stopped while a principal other than an admin can reach into it", fix)
}

// tokenSeparates says whether a token of a user separates its privileges; one
// that is not known is taken to, as Proxmox makes them.
func tokenSeparates(d access.Data, user, name string) bool {
	for _, u := range d.Users {
		if u.ID == user {
			separates, known := u.Tokens[name]
			return separates || !known
		}
	}
	return true
}

// own says whether a principal is pco's user or one of its tokens.
func (s Self) own(principal string) bool {
	return s.User != "" && (principal == s.User || strings.HasPrefix(principal, s.User+"!"))
}

// checkSegmentAccess warns of the non-admin principals who may use the segment
// of the appliance's first card: whoever puts a guest on it with the MAC of the
// appliance cuts off its inbound traffic.
func (r *applianceRun) checkSegmentAccess() Finding {
	const check = "segment access"
	cfg, err := r.config()
	if err != nil {
		return unread(check, "the configuration of the container", err)
	}
	net0 := cfg.Values["net0"]
	bridge := specOption(net0, "bridge")
	if bridge == "" {
		return warn(check, "the configuration of the container has no net0: the segment of the appliance is not known", repairFix(r.self.VMID))
	}
	d, err := r.data()
	if err != nil {
		return warn(check, "who may use the segment of the appliance is not known: the access control of Proxmox cannot be read: "+err.Error(),
			"the access check says why")
	}
	suffix := "/" + bridge
	if vlan := specOption(net0, "tag"); vlan != "" {
		suffix += "/" + vlan
	}
	// A plain Linux bridge is in the zone localnetwork; a vnet is in its own.
	vnets, err := r.app.VNets(r.ctx)
	if err != nil {
		return unread(check, "the zone of the segment of the appliance", err)
	}
	zone := "localnetwork"
	if i := slices.IndexFunc(vnets, func(v pve.VNet) bool { return v.Name == bridge }); i >= 0 {
		zone = vnets[i].Zone
	}
	path := "/sdn/zones/" + zone + suffix
	admins := access.Admins(d)
	var names []string
	for _, p := range access.Effective(d, path, []string{"SDN.Use"}) {
		if !slices.Contains(admins, p.ID) && !r.self.own(p.ID) {
			names = append(names, p.ID)
		}
	}
	if len(names) == 0 {
		return ok(check, fmt.Sprintf("no principal other than an admin and pco's own may use %s, the segment of net0", path))
	}
	return warn(check, fmt.Sprintf("%s %s SDN.Use on %s, the segment of net0 of the appliance: "+
		"a guest put on it with the MAC of the appliance cuts off its inbound traffic", strings.Join(names, ", "), verb(len(names), "holds", "hold"), path),
		fmt.Sprintf("put the appliance on a segment only admins may use, or take SDN.Use on %s from the principals named", path))
}

// checkAPI says whether the endpoint of Proxmox answers under the server name
// its certificate is verified under.
func (r *applianceRun) checkAPI() Finding {
	const check = "api"
	_, err := r.release()
	s := r.self
	switch verr := r.app.VerifyError(); {
	case verr != nil:
		return fail(check, fmt.Sprintf("the certificate of %s no longer verifies under %s (the cluster CA or the pveproxy certificate changed?): %v",
			s.Address, s.ServerName, verr), repairFix(s.VMID))
	case err != nil:
		return fail(check, fmt.Sprintf("the API at %s does not answer under %s: %v", s.Address, s.ServerName, err),
			"check that pveproxy runs on the node and that the appliance reaches it on port 8006")
	}
	return ok(check, fmt.Sprintf("%s answers, and its certificate verifies under %s", s.Address, s.ServerName))
}

func (r *applianceRun) checkFirewall() Finding {
	const check = "firewall"
	datacenter, err := r.app.DatacenterFirewall(r.ctx)
	if err != nil {
		return unread(check, "the firewall options", err)
	}
	node, err := r.app.NodeFirewall(r.ctx)
	if err != nil {
		return unread(check, "the firewall options", err)
	}
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	return ok(check, fmt.Sprintf("the datacenter firewall is %s; the firewall of node %s is %s", onOff(datacenter), r.self.Node, onOff(node)))
}

const fixEdge = "the appliance needs a way out to Cloudflare on port 7844, TCP and UDP; pco egress show shows the table"

// checkEgressProbe tries the egress filter as the user it confines: the edge
// of Cloudflare must connect and the API of Proxmox must be refused. A probe
// as root would prove nothing about the filter.
func (r *applianceRun) checkEgressProbe() Finding {
	const check = "egress probe"
	api := r.self.Address
	if api == "" {
		return warn(check, "the install records no address of the Proxmox API to probe", repairFix(r.self.VMID))
	}
	var edgeErr, apiErr error
	stopped := together(
		func() { edgeErr = r.app.ProbeAsConnector(r.ctx, edge) },
		func() { apiErr = r.app.ProbeAsConnector(r.ctx, api) },
	)
	if stopped[0] != nil {
		edgeErr = fmt.Errorf("the probe stopped on an internal error: %w", stopped[0])
	}
	if stopped[1] != nil {
		apiErr = fmt.Errorf("the probe stopped on an internal error: %w", stopped[1])
	}
	if errors.Is(edgeErr, egress.ErrNoConnectorUser) || errors.Is(apiErr, egress.ErrNoConnectorUser) {
		return fail(check, "the user "+egress.ConnectorUser+" does not exist: no connector starts, and the egress filter cannot be tried",
			"install the package again or run systemd-sysusers")
	}
	var details, fixes []string
	level := LevelOK
	if edgeErr != nil {
		level = LevelFail
		details = append(details, fmt.Sprintf("as pco-connector, %s does not connect: %v; the connectors cannot reach Cloudflare", edge, edgeErr))
		fixes = append(fixes, fixEdge)
	}
	switch {
	case apiErr == nil:
		level = LevelFail
		details = append(details, fmt.Sprintf("as pco-connector, %s connects: the egress filter does not confine the connectors", api))
		fixes = append(fixes, fixLocalTable)
	case !errors.Is(apiErr, ErrRefused):
		level = LevelFail
		details = append(details, fmt.Sprintf("as pco-connector, %s is not refused, as the egress filter refuses it, but fails otherwise: %v", api, apiErr))
		fixes = append(fixes, fixLocalTable)
	default:
		// The node answers a connection to a port nothing listens on with the
		// same refusal, so a refusal proves the filter only while the API
		// answers to pco itself.
		if _, err := r.release(); err != nil {
			level = worse(level, LevelWarn)
			details = append(details, fmt.Sprintf("as pco-connector, %s is refused, but the API does not answer for pco either: %v; "+
				"the refusal says nothing of the egress filter", api, err))
			fixes = append(fixes, fixAPI)
		}
	}
	if len(details) > 0 {
		return Finding{Check: check, Level: level, Detail: strings.Join(details, "; "), Fix: strings.Join(fixes, "; ")}
	}
	return ok(check, fmt.Sprintf("as pco-connector, %s connects and %s is refused", edge, api))
}

func (r *applianceRun) checkCloudflareAPI() Finding {
	const check = "cloudflare api"
	if _, err := r.date(); err != nil {
		return warn(check, "api.cloudflare.com does not answer for pco: "+err.Error(),
			"let the appliance reach api.cloudflare.com on port 443: every change at Cloudflare needs it")
	}
	return ok(check, "api.cloudflare.com answers")
}

// checkClock compares the clock with the Date of Cloudflare's answer.
func (r *applianceRun) checkClock() Finding {
	const check = "clock"
	date, err := r.date()
	if err != nil {
		return warn(check, "the clock was not compared with Cloudflare's: api.cloudflare.com does not answer: "+err.Error(), "see the cloudflare api check")
	}
	skew := r.now.Sub(date).Round(time.Second)
	off := max(skew, -skew)
	if off < skewWarn {
		return ok(check, fmt.Sprintf("the clock is within %s of Cloudflare's", off))
	}
	side := "ahead of"
	if skew < 0 {
		side = "behind"
	}
	detail := fmt.Sprintf("the clock is %s %s Cloudflare's", off, side)
	fix := "set the clock of the node, which the container shares: check its time synchronisation (chrony or systemd-timesyncd)"
	if off >= skewFail {
		return fail(check, detail, fix)
	}
	return warn(check, detail, fix)
}

func (r *applianceRun) checkDNS() Finding {
	const check = "dns"
	servers, err := r.app.Resolvers()
	switch {
	case err != nil:
		return fail(check, "the name servers cannot be read: "+err.Error(), "check /etc/resolv.conf in the appliance")
	case len(servers) == 0:
		return fail(check, "no name server is configured: the connectors cannot resolve the names of Cloudflare's edge",
			fmt.Sprintf("%spct set %d --nameserver <address>, then restart the container", fixOnNode, r.self.VMID))
	}
	names := make([]string, len(servers))
	for i, s := range servers {
		names[i] = s.String()
	}
	host, _, _ := net.SplitHostPort(edge)
	if err := r.app.Lookup(r.ctx, host); err != nil {
		return fail(check, fmt.Sprintf("the name servers (%s) do not answer for %s: %v; the connectors cannot resolve the edge", strings.Join(names, ", "), host, err),
			fmt.Sprintf("let the appliance reach its name servers; pct config %d on the node shows what it was given", r.self.VMID))
	}
	return ok(check, fmt.Sprintf("%s (%s) answer%s for %s", count(len(names), "name server", "name servers"), strings.Join(names, ", "), verb(len(names), "s", ""), host))
}

func (r *applianceRun) checkHolds() Finding {
	const check = "holds"
	held, err := r.app.Holds(r.ctx)
	if err != nil {
		return warn(check, "the held packages could not be read: "+err.Error(), "apt-mark showhold")
	}
	var missing []string
	for _, pkg := range []string{"pco", "cloudflared"} {
		if !slices.Contains(held, pkg) {
			missing = append(missing, pkg)
		}
	}
	switch len(missing) {
	case 0:
		return ok(check, "pco and cloudflared are held")
	case 1:
		return warn(check, missing[0]+" is not held: apt or unattended-upgrades may replace it", "apt-mark hold "+missing[0])
	}
	return warn(check, "pco and cloudflared are not held: apt or unattended-upgrades may replace them", "apt-mark hold pco cloudflared")
}

func (r *applianceRun) checkUnattended() Finding {
	const check = "unattended-upgrades"
	set, err := r.app.UnattendedUpgrades(r.ctx)
	switch {
	case err != nil:
		return warn(check, "whether unattended-upgrades is set up could not be read: "+err.Error(), "systemctl status unattended-upgrades.service")
	case !set:
		return warn(check, "unattended-upgrades is not enabled and configured: the security updates of Debian are not installed on their own",
			"systemctl enable unattended-upgrades.service, and keep APT::Periodic::Unattended-Upgrade \"1\" in /etc/apt/apt.conf.d")
	}
	return ok(check, "unattended-upgrades is enabled and installs the security updates of Debian")
}

// checkDisk looks at the root disk and the state volume: 80 percent warns and
// 90 fails.
func (r *applianceRun) checkDisk() Finding {
	const check = "disk"
	vols := []struct{ path, grow string }{
		{"/", fmt.Sprintf("free space on /: apt-get clean, or grow it on the node: pct resize %d rootfs +1G", r.self.VMID)},
		{stateDir, fmt.Sprintf("grow %s on the node: pct resize %d mp0 +1G", stateDir, r.self.VMID)},
	}
	level := LevelOK
	var details, fixes []string
	for _, v := range vols {
		used, total, err := r.app.DiskUse(v.path)
		if err != nil || total == 0 {
			if err == nil {
				err = errors.New("it has no size")
			}
			details = append(details, fmt.Sprintf("the use of %s could not be read: %v", v.path, err))
			level = worse(level, LevelWarn)
			if !slices.Contains(fixes, "df -h") {
				fixes = append(fixes, "df -h")
			}
			continue
		}
		details = append(details, fmt.Sprintf("%s is %d%% full (%s of %s)", v.path, used*100/total, iec(used), iec(total)))
		switch {
		case used*100 >= total*90:
			level = worse(level, LevelFail)
			fixes = append(fixes, v.grow)
		case used*100 >= total*80:
			level = worse(level, LevelWarn)
			fixes = append(fixes, v.grow)
		}
	}
	return Finding{Check: check, Level: level, Detail: strings.Join(details, "; "), Fix: strings.Join(fixes, "; ")}
}

func (r *applianceRun) checkJournal() Finding {
	const check = "journal"
	used, err := r.app.JournalUse(r.ctx)
	switch {
	case err != nil:
		return warn(check, "the size of the journal could not be read: "+err.Error(), "journalctl --disk-usage")
	case used >= journalWarn:
		return warn(check, fmt.Sprintf("the journal takes %s, where the appliance keeps it to 64 MiB", iec(used)),
			"journalctl --vacuum-size=32M, and look in journalctl -p warning for what writes so much")
	}
	return ok(check, "the journal takes "+iec(used))
}

func (r *applianceRun) checkMemory() Finding {
	const check = "memory"
	m, err := r.app.MemoryPressure()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ok(check, "the kernel reports no memory pressure to this container")
	case err != nil:
		return warn(check, "memory pressure could not be read: "+err.Error(), "cat /proc/pressure/memory")
	case m.Own && m.Some10 > pressureWarn:
		more := "<megabytes>"
		if cfg, err := r.config(); err == nil {
			if mb, err := strconv.Atoi(cfg.Values["memory"]); err == nil && mb > 0 {
				more = strconv.Itoa(2 * mb)
			}
		}
		return warn(check, fmt.Sprintf("memory pressure of the container is %.1f%%: tasks of the appliance waited for memory for that share of the last 10 seconds", m.Some10),
			fmt.Sprintf("%spct set %d --memory %s", fixOnNode, r.self.VMID, more))
	case !m.Own && m.Some10 > pressureWarn:
		return warn(check, fmt.Sprintf("memory pressure of the node is %.1f%%, and the container's own is not reported: the node is short of memory, "+
			"which the appliance shares; more memory for the container would not change that", m.Some10),
			fixOnNode+"cat /proc/pressure/memory, and free -m for who uses it")
	case !m.Own:
		return ok(check, fmt.Sprintf("memory pressure of the node is %.1f%%; the container's own is not reported", m.Some10))
	}
	use := ""
	switch {
	case m.Limit > 0:
		use = fmt.Sprintf(", with %s of %s in use", iec(m.Used), iec(m.Limit))
	case m.Used > 0:
		use = fmt.Sprintf(", with %s in use", iec(m.Used))
	}
	return ok(check, fmt.Sprintf("memory pressure of the container is %.1f%%%s", m.Some10, use))
}

// checkVersions says what runs, and warns of what is old, denied or stale: a
// cloudflared that is more than ten months old or that the list of vetted
// versions denies, and a list that is more than 90 days old.
func (r *applianceRun) checkVersions() Finding {
	const check = "versions"
	pco, cloudflared, debian, err := r.app.Versions(r.ctx)
	if err != nil {
		return warn(check, "the versions could not be read: "+err.Error(), "pco version; cloudflared --version")
	}
	level := LevelOK
	var notes, fixes []string
	note := func(l Level, text, fix string) {
		level = worse(level, l)
		notes = append(notes, text)
		if fix != "" && !slices.Contains(fixes, fix) {
			fixes = append(fixes, fix)
		}
	}
	if cloudflaredOld(cloudflared, r.now) {
		note(LevelWarn, fmt.Sprintf("cloudflared %s is more than ten months old", cloudflared), "pco upgrade cloudflared")
	}
	m, err := r.app.Manifest()
	switch {
	case err != nil:
		note(LevelWarn, "the list of vetted cloudflared versions could not be read: "+err.Error(), "pco upgrade pco installs a release with its list")
	default:
		if reason, denied := m.Denied(cloudflared); denied {
			note(LevelFail, fmt.Sprintf("cloudflared %s is denied by the list of vetted versions: %s", cloudflared, reason), "pco upgrade cloudflared")
		}
		if r.now.Sub(m.Updated) > manifestStale {
			note(LevelWarn, fmt.Sprintf("the list of vetted cloudflared versions is from %s; a newer pco release ships a newer one", m.Updated.Format(time.DateOnly)),
				"pco upgrade --check says whether a newer release is out")
		}
	}
	detail := fmt.Sprintf("pco %s, cloudflared %s, Debian %s", pco, cloudflared, debian)
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	return Finding{Check: check, Level: level, Detail: detail, Fix: strings.Join(fixes, "; ")}
}

// checkNet verifies what pco-net.service loads, and counts what the table of
// the service prefix rejected.
func (r *applianceRun) checkNet() Finding {
	const check = "net"
	if err := r.app.NetVerify(r.ctx); err != nil {
		return fail(check, "what pco-net.service loads is not in place: "+err.Error(), "pco net show says what differs; the daemon loads it again within 30 seconds")
	}
	n, err := r.app.NetLeaked(r.ctx)
	switch {
	case err != nil:
		return warn(check, "what the service prefix rejected could not be counted: "+err.Error(), "pco net show")
	case n > 0:
		return warn(check, fmt.Sprintf("%s sent to the service prefix without a mapping and rejected since the table was loaded",
			count(int(n), "packet was", "packets were")),
			"pco net show shows the count: a few after a route was withdrawn are usual, a count that grows is not")
	}
	return ok(check, "the service prefix is kept in place, and nothing was sent to it without a mapping")
}

// checkEtcPVE fails when the container has /etc/pve: a bind mount of the
// cluster filesystem, which with the node's www-data group mapped in reads the
// TLS keys of the node. A /etc/pve that does not answer is a mount that stalls,
// which is as much one.
func (r *applianceRun) checkEtcPVE() Finding {
	const check = "etc-pve"
	there, err := r.app.EtcPVE()
	switch {
	case err != nil:
		return fail(check, fmt.Sprintf("/etc/pve did not answer: %v; something is mounted there that stalls, "+
			"a bind mount of the cluster filesystem in the container perhaps", err), r.etcPVEFix())
	case !there:
		return ok(check, "/etc/pve is not in the container")
	}
	return fail(check, "/etc/pve exists in the container: a bind mount of the cluster filesystem is in the container; remove it", r.etcPVEFix())
}

// etcPVEFix is how to remove the bind mount of /etc/pve, by the name of the
// mount point when the configuration shows it.
func (r *applianceRun) etcPVEFix() string {
	if cfg, err := r.config(); err == nil {
		for _, key := range slices.Sorted(maps.Keys(cfg.Values)) {
			if mountPoint.MatchString(key) && specOption(cfg.Values[key], "mp") == "/etc/pve" {
				return fmt.Sprintf("%spct set %d --delete %s; with the node's www-data group mapped in, "+
					"a bind mount of /etc/pve lets the container read the TLS keys of the node", fixOnNode, r.self.VMID, key)
			}
		}
	}
	return fmt.Sprintf("remove the mount point whose mp is /etc/pve (pct config %d on the node shows it, pct set %d --delete mpN removes it): "+
		"with the node's www-data group mapped in, it lets the container read the TLS keys of the node", r.self.VMID, r.self.VMID)
}

// checkQUIC reports the receive buffer of the node that cloudflared wants for
// QUIC: it is the node's setting, so the line to set it is the admin's choice.
func (r *applianceRun) checkQUIC() Finding {
	const check = "quic buffer"
	n, err := r.app.RmemMax()
	switch {
	case err != nil:
		return warn(check, "net.core.rmem_max could not be read: "+err.Error(), "sysctl net.core.rmem_max")
	case n < rmemWanted:
		return warn(check, fmt.Sprintf("net.core.rmem_max is %d, below the %d bytes cloudflared asks for its QUIC connections; "+
			"it is a setting of the node, which a container cannot change", n, rmemWanted),
			"if you want it, on the node: echo net.core.rmem_max=7500000 >> /etc/sysctl.d/90-pco.conf, then sysctl --system")
	}
	return ok(check, "net.core.rmem_max is "+strconv.Itoa(n))
}

// checkEpoch tells of an epoch this process drew after a container start; it
// says nothing when the stored one was kept.
func (r *applianceRun) checkEpoch() []Finding {
	if r.st.EpochDrawnAt.IsZero() {
		return nil
	}
	return []Finding{ok("epoch", fmt.Sprintf("a new epoch was drawn at %s after a container start; the state is the volume's: "+
		"after a rollback, approvals, acknowledged segments and credentials made after the snapshot are gone", r.st.EpochDrawnAt.UTC().Format(time.RFC3339)))}
}

// checkNetworkGrants lists the networks pco's user and token may put cards on,
// and warns of a grant of a whole zone and of a bridge that carries an address
// of the node.
func (r *applianceRun) checkNetworkGrants() Finding {
	const check = "network grants"
	d, err := r.data()
	if err != nil {
		return warn(check, fmt.Sprintf("what %s may put cards on is not known: the access control of Proxmox cannot be read: %v", r.self.User, err),
			"the access check says why")
	}
	var paths []string
	for _, e := range d.ACL {
		if (e.UGID == r.self.User || e.UGID == r.self.Token) && (e.Path == "/sdn" || strings.HasPrefix(e.Path, "/sdn/")) && !slices.Contains(paths, e.Path) {
			paths = append(paths, e.Path)
		}
	}
	// The token holds what its user holds as well, so a grant to the user
	// alone puts no card anywhere.
	paths = slices.DeleteFunc(paths, func(path string) bool { return !slices.Contains(access.Privileges(d, r.self.Token, path), "SDN.Use") })
	slices.Sort(paths)
	who := r.self.User + " and its token"
	if len(paths) == 0 {
		return ok(check, who+" may put no card on a network but their own")
	}
	detail := fmt.Sprintf("%s may put cards on %s", who, strings.Join(paths, ", "))
	level := LevelOK
	var cmds []string
	var bridges []string
	zoneWide := false
	for _, path := range paths {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) <= 3 {
			zoneWide, level = true, LevelWarn
			detail += fmt.Sprintf("; %s is a whole zone: every bridge or vnet of it, present and future", path)
			for _, e := range d.ACL {
				if e.Path != path {
					continue
				}
				switch {
				case e.Type == "user" && e.UGID == r.self.User:
					cmds = append(cmds, fmt.Sprintf("pveum acl delete %s --roles %s --users %s", path, e.RoleID, e.UGID))
				case e.Type == "token" && e.UGID == r.self.Token:
					cmds = append(cmds, fmt.Sprintf("pveum acl delete %s --roles %s --tokens %s", path, e.RoleID, e.UGID))
				}
			}
			continue
		}
		bridges = append(bridges, path)
	}
	fix := ""
	if len(bridges) > 0 {
		ifaces, err := r.app.NodeNetwork(r.ctx)
		if err != nil {
			detail += fmt.Sprintf("; the addresses of the node could not be read to compare them: %v", err)
			return Finding{Check: check, Level: LevelWarn, Detail: detail, Fix: fixAPI}
		}
		for _, path := range bridges {
			parts := strings.Split(strings.Trim(path, "/"), "/")
			bridge := parts[3]
			i := slices.IndexFunc(ifaces, func(n pve.NodeIface) bool { return n.Name == bridge && len(n.Addrs) > 0 })
			if i < 0 {
				continue
			}
			level = LevelWarn
			addrs := make([]string, len(ifaces[i].Addrs))
			for j, a := range ifaces[i].Addrs {
				addrs[j] = a.String()
			}
			detail += fmt.Sprintf("; the bridge %s carries %s, an address of the node: %s", bridge, strings.Join(addrs, ", "), networkHint)
			cmd := fmt.Sprintf("pco appliance revoke-network --vmid %d --bridge %s", r.self.VMID, bridge)
			if len(parts) > 4 {
				cmd += " --vlan " + parts[4]
			}
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) > 0 {
		fix = fixOnNode + strings.Join(cmds, "; ")
		if zoneWide {
			fix += "; then grant the networks one by one with pco appliance grant-network"
		}
	}
	return Finding{Check: check, Level: level, Detail: detail, Fix: fix}
}

// checkNftablesMasked expects nftables.service masked in the appliance: its
// start loads a ruleset that flushes both tables of pco.
func checkNftablesMasked(ctx context.Context, app ApplianceEnv) Finding {
	state, err := app.UnitFileState(ctx, nftablesUnit)
	switch {
	case err != nil:
		return warn("nftables", "systemd did not say whether "+nftablesUnit+" is masked: "+err.Error(), "systemctl is-enabled "+nftablesUnit)
	case state == "masked":
		return ok("nftables", nftablesUnit+" is masked")
	case state == notFound:
		return ok("nftables", nftablesUnit+" is not installed")
	case state == "enabled" || state == "enabled-runtime":
		return fail("nftables", nftablesUnit+" is enabled: when it starts, its ruleset flushes both tables of pco, "+
			"and the connectors are not confined until the daemon loads them again", "systemctl mask "+nftablesUnit)
	}
	return warn("nftables", fmt.Sprintf("%s is %s, not masked: a package or an admin that starts or enables it flushes both tables of pco", nftablesUnit, state),
		"systemctl mask "+nftablesUnit)
}
