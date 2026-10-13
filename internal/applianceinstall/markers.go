package applianceinstall

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// Every object the installer makes carries a mark by which uninstall finds it
// and tells it from what an admin made: the container its description, the
// token its comment, the pool its comment, the user the comment of pco setup.
// Roles have no comment in Proxmox: theirs is their name and exactly the
// privileges pco gives them.
const (
	poolID      = appliance.Pool
	poolComment = "pco appliances"

	ctHostname = "pco"
	ctTags     = "pco-appliance;pco-combined"
	stateDir   = "/var/lib/pco"

	markerFile    = stateDir + "/.volume"
	caFile        = stateDir + "/" + appliance.CAFile
	bootstrapFile = stateDir + "/bootstrap.json"
	manifestFile  = stateDir + "/manifest.json"
	stateMeta     = stateDir + "/cluster/meta"

	// hostInstall is the install record of a host install of pco.
	hostInstall = "pco/meta/install.json"
)

// marker is the mark of the appliance vmid, in the comment of its token and at
// the start of the description of its container.
func marker(vmid int) string { return fmt.Sprintf("pco appliance vm%d", vmid) }

// description is what the installer writes into the description of the
// container of the appliance vmid: the mark, and a line for each NoAccess line
// it added to keep a principal out of it. That is the node's own record of
// them, which the appliance cannot change, as the privileges of its token only
// audit. The manifest inside the container names the lines too, and a
// compromised appliance could name others.
func description(vmid int, at time.Time, lines []setup.NoAccessLine) string {
	d := fmt.Sprintf("%s, installed %s by pco appliance install", marker(vmid), at.UTC().Format(time.DateOnly))
	for _, l := range lines {
		d += fmt.Sprintf("\nNoAccess for %s on %s", l.Principal, l.Path)
	}
	return d
}

var (
	describedAs   = regexp.MustCompile(`^pco appliance vm([1-9][0-9]{2,8}), installed [0-9-]+ by pco appliance install$`)
	describedLine = regexp.MustCompile(`^NoAccess for (\S+) on (/\S*)$`)
)

// The lines of the description that record, on the node, that pco appliance
// managed enable made the SDN zone and the vnet of the managed network for
// the appliance. The manifest inside the container notes them too, and
// uninstall removes them only when both say so: a compromised appliance
// could name a zone of the admin's as pco's. The description can be set by
// whoever may change the options of the container, so each line carries a
// note of that object that the node keeps as well, where only root writes
// (sdnRecord).
const (
	madeZoneLine = "made the SDN zone " + setup.ManagedZone
	madeVnetLine = "made the SDN vnet " + setup.ManagedVnet
	noteSep      = ", note "
)

var madeNote = regexp.MustCompile(`^[0-9a-f]{32}$`)

// described is what the description of a container of the installer's says.
type described struct {
	vmid  int // the VMID its mark names
	lines []setup.NoAccessLine
	// zone and vnet say that pco appliance managed enable made the SDN zone
	// and the vnet of the managed network for the appliance, under the notes
	// zoneNote and vnetNote; a line of a release before has none.
	zone, vnet         bool
	zoneNote, vnetNote string
}

// parseDescription reads the description of a container, when it is the one
// the installer writes: the VMID its mark names, the NoAccess lines it
// records and the SDN objects made for it. A restore to another VMID keeps
// the description of the container it was made from.
func parseDescription(desc string) (described, bool) {
	var d described
	first, rest, _ := strings.Cut(strings.TrimSpace(desc), "\n")
	m := describedAs.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return described{}, false
	}
	vmid, err := strconv.Atoi(m[1])
	if err != nil {
		return described{}, false
	}
	d.vmid = vmid
	for line := range strings.Lines(rest) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		made, note, noted := strings.Cut(line, noteSep)
		if noted && !madeNote.MatchString(note) {
			return described{}, false
		}
		switch made {
		case madeZoneLine:
			d.zone, d.zoneNote = true, note
			continue
		case madeVnetLine:
			d.vnet, d.vnetNote = true, note
			continue
		}
		l := describedLine.FindStringSubmatch(line)
		if l == nil {
			return described{}, false
		}
		n := setup.NoAccessLine{Principal: l[1], Path: l[2], Role: roleNoAccess}
		if checkNoAccess(n, vmid) != nil {
			return described{}, false
		}
		d.lines = append(d.lines, n)
	}
	return d, true
}

// withSDNNotes is the description desc, of the installer's, with the notes
// of the SDN zone and the vnet as rec has them: a line for each object it
// holds a note of. Its first line and its NoAccess lines stay as they are.
func withSDNNotes(desc string, rec sdnRecord) (string, bool) {
	d, ok := parseDescription(desc)
	if !ok {
		return "", false
	}
	out := firstLine(desc)
	for _, l := range d.lines {
		out += fmt.Sprintf("\nNoAccess for %s on %s", l.Principal, l.Path)
	}
	if rec.Zone != "" {
		out += "\n" + madeZoneLine + noteSep + rec.Zone
	}
	if rec.Vnet != "" {
		out += "\n" + madeVnetLine + noteSep + rec.Vnet
	}
	return out, true
}

func tokenName(vmid int) string { return "vm" + strconv.Itoa(vmid) }

// tokenID is the API token of the appliance vmid.
func tokenID(vmid int) string { return setup.UserID + "!" + tokenName(vmid) }

var otherToken = regexp.MustCompile(`^vm([1-9][0-9]{2,8})$`)

// applianceOf returns the VMID of the appliance a token of pco@pve is named
// after.
func applianceOf(name string) (int, bool) {
	m := otherToken.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	vmid, err := strconv.Atoi(m[1])
	return vmid, err == nil
}

// templateName is the file of the template of a release.
func templateName(version, arch string) string {
	return fmt.Sprintf("pco-appliance_%s_%s.tar.zst", version, arch)
}

var templateVolume = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_.-]*):vztmpl/(pco-appliance_[A-Za-z0-9.~+-]+_(amd64|arm64)\.tar\.zst)$`)

// isTemplateVolume reports whether a volume is a template of pco.
func isTemplateVolume(volid string) bool { return templateVolume.MatchString(volid) }

// roleNoAccess is the role of the lines that keep a principal out of the
// appliance. A line of the access control list carries no mark: the
// manifest of the appliance is what says the installer added one.
const roleNoAccess = "NoAccess"

// noAccessPaths are the paths the installer adds NoAccess on for the
// appliance vmid: the container's own, and those above it where a principal
// holds what reaches into it.
func noAccessPaths(vmid int) []string {
	return []string{"/", "/vms", "/pool", "/pool/" + poolID, "/vms/" + strconv.Itoa(vmid)}
}

// The privileges of the roles of a network grant, their only mark. The role
// of the network reads the vnet as well (SDN.Audit), so that the appliance
// can tell whether it is VLAN-aware.
var grantRolePrivs = map[string]string{
	setup.RoleManaged: "VM.Config.Network",
	setup.RoleSDN:     "SDN.Use,SDN.Audit",
}

// earlierRolePrivs are the privileges a release before gave a grant role; a
// grant brings such a role up to date.
var earlierRolePrivs = map[string]string{
	setup.RoleSDN: "SDN.Use",
}

// isGrantRole reports whether a role is one of a network grant with exactly
// the privileges pco gives it, now or in a release before.
func isGrantRole(id string, privs []string) bool {
	want, ok := grantRolePrivs[id]
	if !ok {
		return false
	}
	have := strings.Join(slices.Sorted(slices.Values(privs)), ",")
	return have == sortedPrivs(want) || have == sortedPrivs(earlierRolePrivs[id])
}

// isEarlierGrantRole reports whether a role is one of a network grant as a
// release before made it, to bring up to date.
func isEarlierGrantRole(id string, privs []string) bool {
	want, ok := earlierRolePrivs[id]
	return ok && strings.Join(slices.Sorted(slices.Values(privs)), ",") == sortedPrivs(want)
}

func sortedPrivs(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(slices.Sorted(slices.Values(strings.Split(s, ","))), ",")
}
