// Package doctor checks an installation of pco and diagnoses a route. It runs
// inside the daemon, from the state of the engine and the facts of the host an
// Env gives it, so that the command line needs nothing but the socket.
package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Level is how a check or a step fared.
type Level string

const (
	LevelOK   Level = "ok"
	LevelWarn Level = "warn"
	LevelFail Level = "fail"
)

// Finding is the outcome of one check.
type Finding struct {
	Check  string `json:"check"`
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"` // what to do about it; empty when ok
}

// Env abstracts the host facts doctor reads.
type Env interface {
	CloudflaredVersion(ctx context.Context) (string, error)
	UnitActive(ctx context.Context, unit string) (bool, error)
	CanDial(ctx context.Context, network, addr string) error
	PVEVersion(ctx context.Context) (string, error)
	// Store is nil when the store is mounted and set up.
	Store(ctx context.Context) error
	// NodeLock is nil when this daemon holds the lock of the node.
	NodeLock(ctx context.Context) error
	PollInterval() time.Duration
	Now() time.Time
}

const (
	// edge is where the connectors connect to Cloudflare.
	edge = "region1.v2.argotunnel.com:7844"
	// lateCycles and staleCycles are how many poll intervals old the last
	// cycle is late, and stale.
	lateCycles  = 3
	staleCycles = 6

	fixProblems = "pco status lists the problems that say why"
	fixNewToken = "add a new token with pco credential add, then remove this one"

	// notChecked is what a tunnel the last cycle did not check is, when the
	// state does not say why.
	notChecked = "not checked in the last cycle"
)

// minPVE is the oldest Proxmox VE release pco supports.
var minPVE = [2]int{8, 4}

// cloudflaredVersion finds YYYY.M.P in what cloudflared --version says.
var cloudflaredVersion = regexp.MustCompile(`\b(\d{4})\.(\d{1,2})\.(\d+)\b`)

// stateChecks are the checks that read nothing but the state, which says
// nothing before the first cycle.
var stateChecks = []string{"approval", "conflicts", "credentials", "inventory", "lost markers", "mode", "problems", "waiting", "writer"}

// Run checks the installation: the state the engine published and what env
// tells of the host. Every check has a finding, sorted by check.
func Run(ctx context.Context, st engine.State, env Env) []Finding {
	out := []Finding{
		checkCycle(st, env), checkCloudflared(ctx, env), checkOutbound(ctx, st, env),
		checkProxmox(ctx, env), checkStore(ctx, env), checkLock(ctx, env),
	}
	if st.At.IsZero() {
		for _, check := range stateChecks {
			out = append(out, warn(check, "not known until the first cycle", "wait for the first cycle"))
		}
	} else {
		out = append(out, checkMode(st), checkInventory(st), checkWriter(st), checkProblems(st), checkConflicts(st), checkLost(st), checkWaiting(st))
		out = append(out, checkCredentials(st, env.Now())...)
		out = append(out, checkApprovals(st)...)
	}
	out = append(out, checkTunnels(ctx, st, env)...)
	slices.SortStableFunc(out, compareChecks)
	return out
}

// compareChecks orders findings by check, and the checks of one kind, as
// "approval qemu/20" and "approval qemu/101", by what they are of: owners in
// their natural order.
func compareChecks(a, b Finding) int {
	kindA, ofA, _ := strings.Cut(a.Check, " ")
	kindB, ofB, _ := strings.Cut(b.Check, " ")
	return cmp.Or(strings.Compare(kindA, kindB), model.CompareOwners(ofA, ofB))
}

// Failed reports whether any finding is a failure.
func Failed(findings []Finding) bool {
	return slices.ContainsFunc(findings, func(f Finding) bool { return f.Level == LevelFail })
}

func ok(check, detail string) Finding { return Finding{Check: check, Level: LevelOK, Detail: detail} }

func warn(check, detail, fix string) Finding {
	return Finding{Check: check, Level: LevelWarn, Detail: detail, Fix: fix}
}

func fail(check, detail, fix string) Finding {
	return Finding{Check: check, Level: LevelFail, Detail: detail, Fix: fix}
}

func checkMode(st engine.State) Finding {
	if st.Mode == engine.ModeObserve {
		return warn("mode", "observe-only: nothing is changed at Cloudflare", "pco apply")
	}
	return ok("mode", st.Mode+": changes are applied")
}

func checkCycle(st engine.State, env Env) Finding {
	interval := env.PollInterval()
	if st.At.IsZero() {
		return warn("cycle", "no cycle has run yet", "wait for the first cycle; journalctl -u pco says why it does not come")
	}
	age := env.Now().Sub(st.FinishedAt).Round(time.Second)
	switch {
	case age > staleCycles*interval:
		return fail("cycle", fmt.Sprintf("the last cycle ran %s ago, more than six poll intervals of %s", age, interval),
			"journalctl -u pco says what holds the cycles up")
	case age > lateCycles*interval:
		return warn("cycle", fmt.Sprintf("the last cycle ran %s ago, more than three poll intervals of %s", age, interval),
			"journalctl -u pco says what holds the cycles up")
	}
	return ok("cycle", fmt.Sprintf("the last cycle ran %s ago", max(age, 0)))
}

// checkProblems fails while the last cycle reported problems: most of them
// hold the daemon, and none is in order.
func checkProblems(st engine.State) Finding {
	switch n := len(st.Problems); n {
	case 0:
		return ok("problems", "the last cycle found no problem")
	case 1:
		return fail("problems", "1 problem: "+st.Problems[0], "pco status")
	default:
		return fail("problems", fmt.Sprintf("%d problems; the first: %s", n, st.Problems[0]), "pco status")
	}
}

func checkInventory(st engine.State) Finding {
	if !st.Complete {
		return fail("inventory", "the inventory is incomplete: nothing is changed until it is complete", fixProblems)
	}
	return ok("inventory", "every guest is listed")
}

func checkWriter(st engine.State) Finding {
	switch st.WriterVerdict {
	case engine.VerdictStale:
		return fail("writer", "a newer generation of this install writes the tunnel configuration", "run pco setup --recover on the node that should write")
	case engine.VerdictForeign:
		return fail("writer", "another installation writes the tunnel configuration",
			"stop the other installation, or give this one an install of its own with pco setup")
	case engine.VerdictUnknown:
		return fail("writer", "leader.json could not be used", "pco setup --recover")
	}
	return ok("writer", "this daemon writes the tunnel configuration")
}

func checkCredentials(st engine.State, now time.Time) []Finding {
	if len(st.Credentials) == 0 {
		return []Finding{fail("credentials", "no Cloudflare credential", "pco credential add --label <label>")}
	}
	out := make([]Finding, 0, len(st.Credentials))
	for _, c := range st.Credentials {
		out = append(out, checkCredential(c, now))
	}
	return out
}

func checkCredential(c engine.CredentialView, now time.Time) Finding {
	check := "credential " + c.ID
	r := c.Report
	switch {
	case !c.Checked:
		return warn(check, "not checked yet", "pco credential check "+c.ID)
	case !r.Usable:
		return fail(check, "the token cannot be used: "+failedChecks(r), "grant what is missing, then pco credential check "+c.ID)
	case r.Token.ExpiresOn == nil:
		return ok(check, "usable")
	}
	expires := *r.Token.ExpiresOn
	at := expires.UTC().Format(time.RFC3339)
	switch left := expires.Sub(now); {
	case left <= 0:
		return fail(check, "the token expired at "+at, fixNewToken)
	case left < engine.ExpiryWarning:
		return warn(check, fmt.Sprintf("the token expires at %s, in %s", at, days(left)), fixNewToken)
	}
	return ok(check, "usable; the token expires "+at)
}

// failedChecks says what a check of a token found wrong.
func failedChecks(r credentials.Report) string {
	var out []string
	for _, c := range r.Checks {
		if c.OK {
			continue
		}
		what := string(c.Capability)
		if c.Scope != "" {
			what += " on " + c.Scope
		}
		if c.Detail != "" {
			what += ": " + c.Detail
		}
		out = append(out, what)
	}
	if len(out) == 0 {
		return "no active zone"
	}
	return strings.Join(out, "; ")
}

func days(d time.Duration) string {
	n := int(d / (24 * time.Hour))
	switch n {
	case 0:
		return "less than a day"
	case 1:
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}

func checkCloudflared(ctx context.Context, env Env) Finding {
	out, err := env.CloudflaredVersion(ctx)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return warn("cloudflared", "cloudflared --version did not answer in time", "run cloudflared --version by hand to see what holds it up")
	case err != nil:
		return fail("cloudflared", "cloudflared does not run: "+err.Error(), "install cloudflared from the package repository of Cloudflare")
	}
	m := cloudflaredVersion.FindStringSubmatch(out)
	if m == nil {
		return warn("cloudflared", fmt.Sprintf("cannot tell the version from %q", out), "update cloudflared")
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	version := m[1] + "." + m[2] + "." + m[3]
	released := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	if env.Now().After(released.AddDate(1, 0, 0)) {
		return warn("cloudflared", fmt.Sprintf("cloudflared %s is more than a year old", version), "update cloudflared")
	}
	return ok("cloudflared", "cloudflared "+version)
}

// checkTunnels has a finding for every tunnel the state shows and for the
// connector of every one that has an id.
func checkTunnels(ctx context.Context, st engine.State, env Env) []Finding {
	var out []Finding
	for _, t := range st.Tunnels {
		name := fmt.Sprintf("%s in account %s", t.Name, t.AccountID)
		check := "tunnel " + name
		switch {
		case t.Unchecked:
			out = append(out, warn(check, cmp.Or(t.Held, notChecked), fixProblems))
		case t.Held != "":
			out = append(out, warn(check, "left as it is: "+t.Held, fixProblems))
		case t.Unknown:
			out = append(out, warn(check, "its state is not known", fixProblems))
		case !t.Exists:
			out = append(out, warn(check, "it does not exist yet", "pco plan"))
		case !t.Verified:
			out = append(out, warn(check, "its configuration is not verified", "pco plan"))
		default:
			out = append(out, ok(check, fmt.Sprintf("configuration version %d is verified", t.Version)))
		}
		if t.ID != "" {
			out = append(out, checkConnector(ctx, st, env, name, t.ID))
		}
	}
	return out
}

func checkConnector(ctx context.Context, st engine.State, env Env, name, id string) Finding {
	check := "connector " + name
	unit := connector.UnitName(id)
	active, err := env.UnitActive(ctx, unit)
	switch {
	case err != nil:
		return warn(check, "systemd did not say whether it runs: "+err.Error(), "systemctl status "+unit)
	case !active:
		return fail(check, unit+" is not running", "systemctl status "+unit)
	}
	i := slices.IndexFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == id })
	switch {
	case i < 0:
		return fail(check, "the last cycle found no connector", fixProblems)
	case !st.Connectors[i].Ready:
		return fail(check, "not connected to Cloudflare", "journalctl -u "+unit)
	}
	return ok(check, st.Connectors[i].Text())
}

// checkOutbound tries the way out to Cloudflare over TCP. Connectors that are
// all connected may be over QUIC, so then a TCP that does not get through is
// only a warning.
func checkOutbound(ctx context.Context, st engine.State, env Env) Finding {
	err := env.CanDial(ctx, "tcp", edge)
	switch {
	case err == nil:
		return ok("outbound", edge+" answers over TCP")
	case allConnected(st.Connectors):
		return warn("outbound", edge+" cannot be reached over TCP: "+err.Error()+"; every connector is connected all the same, over QUIC perhaps",
			fixOutbound)
	}
	return fail("outbound", edge+" cannot be reached over TCP: "+err.Error(), fixOutbound)
}

const fixOutbound = "allow outbound TCP and UDP to port 7844"

func allConnected(conns []connector.Status) bool {
	return len(conns) > 0 && !slices.ContainsFunc(conns, func(c connector.Status) bool { return !c.Ready })
}

func checkConflicts(st engine.State) Finding {
	if len(st.Conflicts) == 0 {
		return ok("conflicts", "no record of someone else stands in the way")
	}
	names := make([]string, len(st.Conflicts))
	for i, c := range st.Conflicts {
		names[i] = c.Name
	}
	return warn("conflicts", fmt.Sprintf("%s of someone else %s in the way: %s",
		count(len(names), "record", "records"), verb(len(names), "stands", "stand"), strings.Join(names, ", ")), "pco adopt <name>")
}

func checkLost(st engine.State) Finding {
	if len(st.Lost) == 0 {
		return ok("lost markers", "no record of this install lost its marker")
	}
	return warn("lost markers", fmt.Sprintf("%s %s at the tunnel but lost the marker of this install: %s",
		count(len(st.Lost), "record", "records"), verb(len(st.Lost), "points", "point"), strings.Join(st.Lost, ", ")), "pco adopt <name>")
}

func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func verb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func checkProxmox(ctx context.Context, env Env) Finding {
	release, err := env.PVEVersion(ctx)
	if err != nil {
		return fail("proxmox", "the Proxmox API does not answer: "+err.Error(), "check the Proxmox API token of pco")
	}
	major, minor, parsed := majorMinor(release)
	switch {
	case !parsed:
		return warn("proxmox", fmt.Sprintf("cannot tell the version from %q", release), "pveversion says which it is")
	case major < minPVE[0] || major == minPVE[0] && minor < minPVE[1]:
		return fail("proxmox", fmt.Sprintf("Proxmox VE %d.%d is not supported", major, minor),
			fmt.Sprintf("upgrade to Proxmox VE %d.%d or later", minPVE[0], minPVE[1]))
	}
	return ok("proxmox", fmt.Sprintf("Proxmox VE %d.%d", major, minor))
}

func majorMinor(release string) (major, minor int, parsed bool) {
	a, b, found := strings.Cut(release, ".")
	if !found {
		return 0, 0, false
	}
	b, _, _ = strings.Cut(b, ".")
	major, errA := strconv.Atoi(a)
	minor, errB := strconv.Atoi(b)
	return major, minor, errA == nil && errB == nil
}

func checkStore(ctx context.Context, env Env) Finding {
	err := env.Store(ctx)
	switch {
	case err == nil:
		return ok("store", "the store is mounted and set up")
	case errors.Is(err, store.ErrNotMounted):
		return fail("store", err.Error(), "systemctl status pve-cluster")
	}
	return fail("store", err.Error(), "pco setup")
}

func checkLock(ctx context.Context, env Env) Finding {
	if err := env.NodeLock(ctx); err != nil {
		return fail("node lock", err.Error(), "systemctl restart pco, so that no second daemon can start")
	}
	return ok("node lock", "this daemon holds the lock of the node")
}

func checkWaiting(st engine.State) Finding {
	if len(st.Waiting) == 0 {
		return ok("waiting", "nothing waits for a confirmation")
	}
	details := make([]string, len(st.Waiting))
	for i, w := range st.Waiting {
		details[i] = w.Detail
	}
	return warn("waiting", fmt.Sprintf("%s %s for a confirmation: %s",
		count(len(details), "thing", "things"), verb(len(details), "waits", "wait"), strings.Join(details, "; ")),
		"pco apply --confirm-deletes")
}

func checkApprovals(st engine.State) []Finding {
	if len(st.Unapproved) == 0 {
		return []Finding{ok("approval", "no guest waits for approval")}
	}
	out := make([]Finding, 0, len(st.Unapproved))
	for _, g := range st.Unapproved {
		owner := g.String()
		who := engine.OwnerName(owner, &g.GuestView)
		out = append(out, warn("approval "+owner, who+" waits for approval", "pco guest approve "+owner))
	}
	return out
}
