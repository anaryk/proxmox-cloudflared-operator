package applianceinstall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// maxManifest is the most of a manifest pulled from an appliance that is
// read.
const maxManifest = 64 << 10

var (
	nodeName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	tagName  = regexp.MustCompile(`^[a-z0-9_][a-z0-9_+.-]*$`)
)

// readManifest pulls the manifest from the running appliance and keeps what
// has the shapes the installer writes, as anything in the container could
// have written it; what it cannot read is reported
// and nil returned, as the marks find everything without it. otherVMID lets a
// repair take the manifest of the container a restore was made from.
func (r *run) readManifest(ctx context.Context, vmid int, otherVMID bool) *setup.Manifest {
	raw, err := r.pullManifest(ctx, vmid)
	if err != nil {
		r.ask.Warn("the manifest of lxc/%d cannot be read (%v): the marks of the objects say what is pco's", vmid, err)
		return nil
	}
	m, problems := checkManifest(raw, vmid, otherVMID)
	for _, p := range problems {
		r.ask.Warn("the manifest of lxc/%d: %s; ignored", vmid, p)
	}
	return m
}

// pullManifest copies the manifest out of the container into the run's
// directory under /run and reads at most maxManifest of it.
func (r *run) pullManifest(ctx context.Context, vmid int) ([]byte, error) {
	if err := r.makeTemp(); err != nil {
		return nil, err
	}
	defer r.removeTemp()
	path := filepath.Join(r.tmp, "manifest.json")
	if _, err := r.r.Run(ctx, "pct", "pull", strconv.Itoa(vmid), manifestFile, path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	switch {
	case err != nil:
		return nil, err
	case len(b) > maxManifest:
		return nil, fmt.Errorf("it is larger than %d KiB", maxManifest>>10)
	}
	return b, nil
}

// checkManifest keeps of a manifest what has the fixed shapes of an
// appliance's: the objects of pco@pve, PCO, pool pco, the token of this
// appliance, the gate tags, a template volume of pco and the network grants.
// Anything else is reported. A manifest of another appliance is no manifest
// of this one, unless otherVMID says the container was restored from it.
func checkManifest(raw []byte, vmid int, otherVMID bool) (*setup.Manifest, []string) {
	var problems []string
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, []string{"it is not a JSON object"}
	}
	known := []string{"node", "installedAt", "createdRole", "createdUser", "createdToken", "grantedACL", "registeredTags", "appliance",
		"installedCloudflared", "addedAptSource", "addedKeyring", "webEnabled", "webEnv", "webCert", "webTLS"}
	for _, k := range sortedKeys(fields) {
		if !slices.Contains(known, k) {
			problems = append(problems, fmt.Sprintf("it holds %q, which no manifest has", k))
		}
	}
	var m setup.Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return nil, append(problems, fmt.Sprintf("it cannot be read: %v", err))
	}
	if m.Appliance == nil {
		return nil, append(problems, "it is not the manifest of an appliance")
	}
	a := m.Appliance
	if a.VMID != vmid {
		if !otherVMID {
			return nil, append(problems, fmt.Sprintf("it is the manifest of lxc/%d, not of lxc/%d", a.VMID, vmid))
		}
		old := "/vms/" + strconv.Itoa(a.VMID)
		problems = append(problems, fmt.Sprintf("it is the manifest of lxc/%d, which lxc/%d was restored from: its token, its grants "+
			"and its NoAccess lines on %s are that one's", a.VMID, vmid, old))
		a.VMID, a.Token, a.Grants, m.CreatedToken = vmid, tokenID(vmid), nil, false
		a.NoAccess = slices.DeleteFunc(a.NoAccess, func(n setup.NoAccessLine) bool { return n.Path == old })
	}
	if m.InstalledCloudflared || m.AddedAptSource || m.AddedKeyring || m.WebEnabled || m.WebEnv || m.WebCert != "" || len(m.WebTLS) > 0 {
		problems = append(problems, "it names packages, files or units on the node, which an appliance's installer never makes")
		m.InstalledCloudflared, m.AddedAptSource, m.AddedKeyring, m.WebEnabled, m.WebEnv, m.WebCert, m.WebTLS = false, false, false, false, false, "", nil
	}
	if m.Node != "" && !nodeName.MatchString(m.Node) {
		problems = append(problems, fmt.Sprintf("node %q", m.Node))
		m.Node = ""
	}
	if a.Node != "" && !nodeName.MatchString(a.Node) {
		problems = append(problems, fmt.Sprintf("node %q", a.Node))
		a.Node = ""
	}
	if a.Token != tokenID(vmid) {
		problems = append(problems, fmt.Sprintf("token %q is not the token of lxc/%d", a.Token, vmid))
		a.Token, m.CreatedToken = tokenID(vmid), false
	}
	if a.Pool != poolID {
		problems = append(problems, fmt.Sprintf("pool %q is not pool %s", a.Pool, poolID))
		a.Pool, a.CreatedPool = poolID, false
	}
	if a.Template != "" && !isTemplateVolume(a.Template) {
		problems = append(problems, fmt.Sprintf("volume %q is not a template of pco", a.Template))
		a.Template = ""
	}
	a.Grants = slices.DeleteFunc(a.Grants, func(g setup.NetworkGrant) bool {
		if err := g.Check(); err != nil {
			problems = append(problems, "a network grant: "+err.Error())
			return true
		}
		return false
	})
	a.NoAccess = slices.DeleteFunc(a.NoAccess, func(n setup.NoAccessLine) bool {
		if err := checkNoAccess(n, vmid); err != nil {
			problems = append(problems, "a NoAccess line: "+err.Error())
			return true
		}
		return false
	})
	m.RegisteredTags = slices.DeleteFunc(m.RegisteredTags, func(t string) bool {
		if !tagName.MatchString(t) {
			problems = append(problems, fmt.Sprintf("tag %q is no tag", t))
			return true
		}
		return false
	})
	return &m, problems
}

// principalID is the id of a Proxmox user, user@realm, or of its token,
// user@realm!name.
var principalID = regexp.MustCompile(`^[^\s:/!]+@[A-Za-z][A-Za-z0-9._-]*(![A-Za-z][A-Za-z0-9._-]*)?$`)

// checkNoAccess refuses a NoAccess line of another shape than the installer
// adds for the appliance vmid: removing a NoAccess line gives back what it
// took, so one the admin added must never pass for the installer's.
func checkNoAccess(n setup.NoAccessLine, vmid int) error {
	paths := noAccessPaths(vmid)
	user, _, _ := strings.Cut(n.Principal, "!")
	switch {
	case n.Role != roleNoAccess:
		return fmt.Errorf("role %q on %s: the installer adds %s only", n.Role, n.Path, roleNoAccess)
	case !slices.Contains(paths, n.Path):
		return fmt.Errorf("path %q: the installer adds %s on %s and %s only", n.Path, roleNoAccess,
			strings.Join(paths[:len(paths)-1], ", "), paths[len(paths)-1])
	case !principalID.MatchString(n.Principal):
		return fmt.Errorf("principal %q is no user or token", n.Principal)
	case user == "root@pam" || user == setup.UserID:
		return fmt.Errorf("principal %q is never one the installer denies", n.Principal)
	}
	return nil
}

// manifestFromMarks rebuilds the manifest of an appliance whose volume lost
// it, from the objects in Proxmox that carry pco's marks.
func (r *run) manifestFromMarks(ctx context.Context, vmid int) (setup.Manifest, error) {
	m := setup.Manifest{Node: r.node, InstalledAt: r.now(), Appliance: &setup.ApplianceManifest{
		VMID: vmid, Node: r.node, Token: tokenID(vmid), Pool: poolID,
	}}
	us, err := users(ctx, r.r)
	if err != nil {
		return m, err
	}
	m.CreatedUser = slices.ContainsFunc(us, func(u userEntry) bool { return u.ID == setup.UserID && u.Comment == setup.UserComment })
	rs, err := roles(ctx, r.r)
	if err != nil {
		return m, err
	}
	if role, ok := findRole(rs, setup.RoleID); ok {
		m.CreatedRole = setup.IsPCORole(role.Privs)
	}
	acl, err := aclLines(ctx, r.r)
	if err != nil {
		return m, err
	}
	m.GrantedACL = slices.Contains(acl, aclLine{Path: "/", Type: "user", UGID: setup.UserID, Role: setup.RoleID})
	above := noAccessPaths(vmid)
	above = above[:len(above)-1]
	for _, l := range acl {
		if l.Role == roleNoAccess && slices.Contains(above, l.Path) {
			r.ask.Warn("NoAccess for %s on %s carries no mark of the installer: the manifest rebuilt from the marks leaves it out, "+
				"and uninstall leaves it", l.UGID, l.Path)
		}
		if l.Type != "token" || l.UGID != tokenID(vmid) || l.Role != setup.RoleSDN {
			continue
		}
		if g, ok := grantOfPath(l.Path); ok {
			m.Appliance.Grants = append(m.Appliance.Grants, g)
		}
	}
	ps, err := pools(ctx, r.r)
	if err != nil {
		return m, err
	}
	m.Appliance.CreatedPool = slices.ContainsFunc(ps, func(p poolEntry) bool { return p.ID == poolID && p.Comment == poolComment })
	found, err := r.findTemplates(ctx, nil)
	if err != nil {
		return m, err
	}
	for _, t := range found {
		if strings.HasSuffix(t, "/"+templateName(r.h.version, r.arch)) {
			m.Appliance.Template = t
		}
	}
	return m, nil
}

// grantOfPath reads a network grant from its ACL path.
func grantOfPath(path string) (setup.NetworkGrant, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/sdn/zones/"), "/")
	if !strings.HasPrefix(path, "/sdn/zones/") || len(parts) < 2 || len(parts) > 3 {
		return setup.NetworkGrant{}, false
	}
	g := setup.NetworkGrant{Zone: parts[0], VNet: parts[1]}
	if len(parts) == 3 {
		vlan, err := strconv.Atoi(parts[2])
		if err != nil || vlan == 0 {
			return setup.NetworkGrant{}, false
		}
		g.VLAN = vlan
	}
	return g, g.Check() == nil
}
