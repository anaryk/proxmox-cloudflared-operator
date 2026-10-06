package applianceinstall

import (
	"fmt"
	"regexp"
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

func description(vmid int, at time.Time) string {
	return fmt.Sprintf("%s, installed %s by pco appliance install", marker(vmid), at.UTC().Format(time.DateOnly))
}

var describedAs = regexp.MustCompile(`^pco appliance vm([1-9][0-9]{2,8}), installed [0-9-]+ by pco appliance install$`)

// describedVMID returns the VMID the description of a container names, when
// it is the description the installer writes: a restore to another VMID keeps
// the one of the container it was made from.
func describedVMID(desc string) (int, bool) {
	m := describedAs.FindStringSubmatch(strings.TrimSpace(desc))
	if m == nil {
		return 0, false
	}
	vmid, err := strconv.Atoi(m[1])
	return vmid, err == nil
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

// The privileges of the roles of a network grant, their only mark.
var grantRolePrivs = map[string]string{
	setup.RoleManaged: "VM.Config.Network",
	setup.RoleSDN:     "SDN.Use",
}

// isGrantRole reports whether a role is one of a network grant with exactly
// the privilege pco gives it.
func isGrantRole(id string, privs []string) bool {
	want, ok := grantRolePrivs[id]
	return ok && len(privs) == 1 && privs[0] == want
}
