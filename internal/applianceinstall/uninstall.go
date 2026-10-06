package applianceinstall

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// survey is what uninstall finds of an appliance before it asks anything:
// what Proxmox shows, and the manifest pulled from the appliance when it runs,
// as far as it has the shapes the installer writes.
type survey struct {
	exists, running bool
	cfg             ctConfig
	described       int                  // the VMID the description of the container names
	noted           []setup.NoAccessLine // the NoAccess lines its description records
	originalNode    string               // of lxc/described, when the container is a copy of it that is still there
	manifest        *setup.Manifest
	users           []userEntry
	tokens          []tokenEntry
	roles           []roleEntry
	acl             []aclLine
	unpropagated    []aclLine // the lines of acl that count on their own path only
	pools           []poolEntry
	members         []string // of pool pco
	tags            []string
	templates       []string // the template volumes of pco on the node
	hostInstall     bool
	cloudflare      []string // what the install has at Cloudflare, as its purge lists it
	cloudflareErr   error
}

// plan is what uninstall removes, and why it keeps what it keeps.
type plan struct {
	container  bool
	token      bool
	grantLines []aclLine // pco@pve's lines on the networks of the appliance
	noAccess   []aclLine // the NoAccess lines the installer added
	user       bool
	role       bool
	grantRoles []string
	pool       bool
	tags       []string
	template   string // the installer's, which goes unless KeepTemplate
	kept       []string
	notes      []string
}

// Uninstall removes the appliance vmid and what the installer made for it,
// as its marks and its manifest name them. It looks first, lists what goes
// and asks, and only then removes: what is at Cloudflare first, through the
// container, which holds the credentials; then the container, the token
// before its user, the grants, the NoAccess lines the installer added, the
// user and the roles when nothing else uses them, the pool when it is empty,
// the tags the installer added, and the template it downloaded unless
// KeepTemplate.
func (i *Installer) Uninstall(ctx context.Context, vmid int, o UninstallOptions) error {
	switch {
	case o.PurgeCloudflare && o.KeepCloudflare:
		return errors.New("--purge-cloudflare and --keep-cloudflare do not go together")
	case vmid < 100 || vmid > 999999999:
		return fmt.Errorf("--vmid %d: want 100 to 999999999", vmid)
	}
	r := i.newRun(Options{Yes: o.Yes, KeepTemplate: o.KeepTemplate, CloudflareAPI: o.CloudflareAPI}, "uninstall")
	r.j.VMID = vmid
	if err := r.whoami(); err != nil {
		return err
	}
	unlock, err := i.lock()
	if err != nil {
		return err
	}
	defer unlock()
	s, err := r.survey(ctx, vmid, o)
	if err != nil {
		return err
	}
	p := r.plan(vmid, s)
	r.describe(vmid, s, p, o)
	if !o.Yes {
		ok, err := r.ask.Confirm(fmt.Sprintf("Remove the appliance lxc/%d and the objects above?", vmid), false)
		if err != nil {
			return err
		}
		if !ok {
			return setup.ErrAborted
		}
	}
	purge, err := r.decidePurge(vmid, s, o)
	if err != nil {
		return err
	}
	return r.remove(ctx, vmid, s, p, purge)
}

// whoami checks that the installer runs as root and learns the node's name.
func (r *run) whoami() error {
	if r.h.euid() != 0 {
		return errors.New("pco appliance must run as root on the node")
	}
	name, err := r.h.hostname()
	if err != nil {
		return fmt.Errorf("reading the host name: %w", err)
	}
	r.node, _, _ = strings.Cut(name, ".")
	return nil
}

func (r *run) survey(ctx context.Context, vmid int, o UninstallOptions) (survey, error) {
	var s survey
	var err error
	s.cfg, err = readCTConfig(ctx, r.r, r.node, vmid)
	switch {
	case err == nil:
		s.exists = true
		var ok bool
		if s.described, s.noted, ok = parseDescription(s.cfg["description"]); !ok {
			return s, fmt.Errorf("lxc/%d is not a pco appliance (its description lacks the mark of the installer): nothing was removed", vmid)
		}
		if s.running, err = ctRunning(ctx, r.r, vmid); err != nil {
			return s, err
		}
		if s.described != vmid {
			if s.originalNode, _, err = r.originalOf(ctx, s.described); err != nil {
				return s, err
			}
		}
	case !notThere(err):
		return s, fmt.Errorf("reading the configuration of lxc/%d: %w", vmid, err)
	default:
		// Only a container no node of the cluster has is gone.
		if err := r.whereElse(ctx, vmid, "uninstall", "nothing was removed"); err != nil {
			return s, err
		}
	}
	if s.running {
		s.manifest, _ = r.readManifest(ctx, vmid, false)
	}
	if s.users, err = users(ctx, r.r); err != nil {
		return s, err
	}
	if s.tokens, err = tokens(ctx, r.r, s.users); err != nil {
		return s, err
	}
	if s.roles, err = roles(ctx, r.r); err != nil {
		return s, err
	}
	entries, err := aclEntries(ctx, r.r)
	if err != nil {
		return s, err
	}
	for _, e := range entries {
		s.acl = append(s.acl, e.aclLine)
		if !e.propagates() {
			s.unpropagated = append(s.unpropagated, e.aclLine)
		}
	}
	if s.pools, err = pools(ctx, r.r); err != nil {
		return s, err
	}
	if slices.ContainsFunc(s.pools, func(p poolEntry) bool { return p.ID == poolID }) {
		if s.members, err = poolMembers(ctx, r.r, poolID); err != nil {
			return s, err
		}
	}
	if s.tags, err = setup.RegisteredTags(ctx, r.r); err != nil {
		return s, err
	}
	if s.templates, err = r.findTemplates(ctx); err != nil {
		return s, err
	}
	s.hostInstall = r.hostInstalled()
	if s.running && !o.KeepCloudflare && s.originalNode == "" {
		s.cloudflare, s.cloudflareErr = r.listCloudflare(ctx, vmid)
	}
	return s, nil
}

func (r *run) hostInstalled() bool {
	_, err := r.h.readFile(r.h.pveDir + "/" + hostInstall)
	return err == nil
}

// findTemplates finds the templates of pco on the storages of the node that
// hold templates.
func (r *run) findTemplates(ctx context.Context) ([]string, error) {
	all, err := storages(ctx, r.r, r.node)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, st := range all {
		if !st.holds("vztmpl") || !bool(st.Active) {
			continue
		}
		vols, err := templates(ctx, r.r, r.node, st.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range vols {
			if isTemplateVolume(v) {
				found = append(found, v)
			}
		}
	}
	return found, nil
}

// listCloudflare asks the purge in the container what the install has at
// Cloudflare: one object a line, nothing for nothing.
func (r *run) listCloudflare(ctx context.Context, vmid int) ([]string, error) {
	out, err := r.pco(ctx, vmid, "appliance", "purge", "--list")
	if err != nil {
		return nil, err
	}
	var lines []string
	for line := range strings.Lines(out) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func (r *run) plan(vmid int, s survey) plan {
	var p plan
	id := tokenID(vmid)
	p.container = s.exists
	ti := slices.IndexFunc(s.tokens, func(t tokenEntry) bool { return t.Name == tokenName(vmid) })
	switch {
	case ti < 0:
	case s.tokens[ti].Comment == marker(vmid):
		p.token = true
	default:
		p.kept = append(p.kept, fmt.Sprintf("token %s, whose comment is not %q", id, marker(vmid)))
	}
	var (
		others     []string
		appliances []int
	)
	for _, t := range s.tokens {
		if t.Name != tokenName(vmid) {
			others = append(others, setup.UserID+"!"+t.Name)
			if other, ok := applianceOf(t.Name); ok {
				appliances = append(appliances, other)
			}
		}
	}
	p.grantLines = r.grantLines(vmid, s, others)
	p.planNoAccess(vmid, s, appliances)

	// A line goes when it is the token's, which goes with it, one on the
	// container, which goes with it, or a grant line that is removed.
	goes := func(l aclLine) bool {
		switch {
		case l.Type == "token" && l.UGID == id:
			return p.token
		case l.Path == "/vms/"+strconv.Itoa(vmid):
			return p.container
		}
		return slices.Contains(p.grantLines, l)
	}
	ui := slices.IndexFunc(s.users, func(u userEntry) bool { return u.ID == setup.UserID })
	userLines := slices.ContainsFunc(s.acl, func(l aclLine) bool {
		return l.Type == "user" && l.UGID == setup.UserID && !goes(l) && (l.Path != "/" || l.Role != setup.RoleID)
	})
	switch {
	case ui < 0:
	case s.users[ui].Comment != setup.UserComment:
		p.kept = append(p.kept, fmt.Sprintf("user %s, whose comment is not %q", setup.UserID, setup.UserComment))
	case len(others) > 0:
		p.kept = append(p.kept, fmt.Sprintf("user %s and role %s, which %s use", setup.UserID, setup.RoleID, strings.Join(others, ", ")))
	case userLines:
		p.kept = append(p.kept, fmt.Sprintf("user %s, which other lines of the access control list name", setup.UserID))
	case !p.token && slices.ContainsFunc(s.tokens, func(t tokenEntry) bool { return t.Name == tokenName(vmid) }):
		p.kept = append(p.kept, fmt.Sprintf("user %s, whose token %s stays", setup.UserID, id))
	default:
		p.user = true
	}
	userGoes := func(l aclLine) bool { return l.Type == "user" && l.UGID == setup.UserID && p.user }
	unused := func(role string) bool {
		return !slices.ContainsFunc(s.acl, func(l aclLine) bool { return l.Role == role && !goes(l) && !userGoes(l) })
	}
	if role, ok := findRole(s.roles, setup.RoleID); ok {
		switch {
		case !setup.IsPCORole(role.Privs):
			p.kept = append(p.kept, fmt.Sprintf("role %s, which grants %s, not what pco gives it", setup.RoleID, strings.Join(role.Privs, ", ")))
		case !unused(setup.RoleID):
			if p.user || len(others) == 0 {
				p.kept = append(p.kept, fmt.Sprintf("role %s, which other lines of the access control list name", setup.RoleID))
			}
		default:
			p.role = true
		}
	}
	for _, id := range []string{setup.RoleManaged, setup.RoleSDN} {
		if role, ok := findRole(s.roles, id); ok && isGrantRole(role.ID, role.Privs) && unused(id) {
			p.grantRoles = append(p.grantRoles, id)
		}
	}
	if i := slices.IndexFunc(s.pools, func(pl poolEntry) bool { return pl.ID == poolID }); i >= 0 {
		rest := slices.DeleteFunc(slices.Clone(s.members), func(m string) bool { return m == "lxc/"+strconv.Itoa(vmid) && p.container })
		switch {
		case s.pools[i].Comment != poolComment:
			p.kept = append(p.kept, fmt.Sprintf("pool %s, whose comment is not %q", poolID, poolComment))
		case len(rest) > 0:
			p.kept = append(p.kept, fmt.Sprintf("pool %s, which holds %s", poolID, strings.Join(rest, ", ")))
		default:
			p.pool = true
		}
	}
	if s.manifest != nil && len(s.manifest.RegisteredTags) > 0 {
		added := slices.DeleteFunc(slices.Clone(s.manifest.RegisteredTags), func(t string) bool { return !slices.Contains(s.tags, t) })
		switch {
		case len(added) == 0:
		case s.hostInstall:
			p.kept = append(p.kept, fmt.Sprintf("the registered tags %s, which the host install of pco on this node uses", strings.Join(added, ", ")))
		case len(appliances) > 0:
			p.kept = append(p.kept, fmt.Sprintf("the registered tags %s, which another appliance uses", strings.Join(added, ", ")))
		default:
			p.tags = added
		}
	}
	// The template is the installer's when the manifest names it: it names
	// one only when the installer downloaded it.
	if s.manifest != nil && s.manifest.Appliance != nil && slices.Contains(s.templates, s.manifest.Appliance.Template) {
		p.template = s.manifest.Appliance.Template
	}
	if !r.o.KeepTemplate {
		for _, t := range s.templates {
			if t != p.template {
				p.kept = append(p.kept, fmt.Sprintf("the template %s, which the manifest of lxc/%d does not name as the installer's", t, vmid))
			}
		}
	}
	return p
}

// planNoAccess decides which of the NoAccess lines the installer added go:
// each the description of the container records, which is the node's own note
// of them, and the manifest names too when it could be read, that is there as
// the installer made it, but for one on the container, which goes with it, and
// one above the container while another appliance is there, which it keeps the
// principal out of as well. The manifest alone proves nothing, as a
// compromised appliance could name any line. A NoAccess line above the
// container that nothing names is kept and listed, with the command that takes
// it back: another appliance's install may have added it.
func (p *plan) planNoAccess(vmid int, s survey, appliances []int) {
	paths := noAccessPaths(vmid)
	vm := paths[len(paths)-1]
	manifest := s.manifest != nil && s.manifest.Appliance != nil
	var accounted []aclLine
	for _, n := range s.noted {
		l := noAccessLine(n)
		accounted = append(accounted, l)
		line := fmt.Sprintf("NoAccess for %s on %s", n.Principal, n.Path)
		switch {
		case manifest && !slices.Contains(s.manifest.Appliance.NoAccess, n):
			p.kept = append(p.kept, fmt.Sprintf("%s, which the description of lxc/%d names and its manifest does not", line, vmid))
		case !slices.Contains(s.acl, l):
			p.notes = append(p.notes, line+", which the installer added, is not there any more")
		case l.Path == vm && p.container:
		case slices.Contains(s.unpropagated, l):
			p.kept = append(p.kept, fmt.Sprintf("%s, which no longer reaches below %s as the installer made it", line, n.Path))
		case l.Path != vm && len(appliances) > 0:
			p.kept = append(p.kept, fmt.Sprintf("%s, which keeps %s out of the appliance lxc/%d as well", line, n.Principal, appliances[0]))
		default:
			p.noAccess = append(p.noAccess, l)
		}
	}
	for _, l := range s.acl {
		if l.Role == roleNoAccess && slices.Contains(paths[:len(paths)-1], l.Path) && !slices.Contains(accounted, l) {
			p.kept = append(p.kept, fmt.Sprintf("NoAccess for %s on %s, which nothing of lxc/%d names as added by the installer: "+
				"if an install added it, pveum acl delete %s --%ss %s --roles %s takes it back", l.UGID, l.Path, vmid, l.Path, l.Type, l.UGID, roleNoAccess))
		}
	}
}

// grantLines are pco@pve's lines on the networks granted to the appliance,
// as its manifest or the lines of its token name them; one another token of
// pco@pve has the same grant on stays, as that token needs it.
func (r *run) grantLines(vmid int, s survey, others []string) []aclLine {
	var paths []string
	if s.manifest != nil && s.manifest.Appliance != nil {
		for _, g := range s.manifest.Appliance.Grants {
			paths = append(paths, g.Path())
		}
	}
	for _, l := range s.acl {
		if l.Type == "token" && l.UGID == tokenID(vmid) && l.Role == setup.RoleSDN && !slices.Contains(paths, l.Path) {
			paths = append(paths, l.Path)
		}
	}
	var out []aclLine
	for _, path := range paths {
		user := aclLine{Path: path, Type: "user", UGID: setup.UserID, Role: setup.RoleSDN}
		shared := slices.ContainsFunc(s.acl, func(l aclLine) bool {
			return l.Path == path && l.Role == setup.RoleSDN && l.Type == "token" && slices.Contains(others, l.UGID)
		})
		if slices.Contains(s.acl, user) && !shared {
			out = append(out, user)
		}
	}
	return out
}

func (r *run) describe(vmid int, s survey, p plan, o UninstallOptions) {
	r.ask.Info("pco appliance uninstall removes from this node:")
	if p.container {
		state := "stopped"
		if s.running {
			state = "running"
		}
		r.ask.Info("  container lxc/%d (%s), with its state volume and the secrets on it", vmid, state)
		switch {
		case s.originalNode != "":
			r.ask.Info("    (it is a copy of lxc/%d, which is still there, on node %s: what is at Cloudflare is that one's, and stays)",
				s.described, s.originalNode)
		case s.described != vmid:
			r.ask.Info("    (it was made from lxc/%d, which its description names)", s.described)
		}
	} else {
		r.ask.Info("  (no container lxc/%d on this node)", vmid)
	}
	if p.token {
		r.ask.Info("  Proxmox token %s", tokenID(vmid))
	}
	for _, l := range p.grantLines {
		r.ask.Info("  the grant of role %s on %s to %s", l.Role, l.Path, l.UGID)
	}
	for _, l := range p.noAccess {
		r.ask.Info("  the line NoAccess for %s on %s, which the installer added", l.UGID, l.Path)
	}
	if p.user {
		r.ask.Info("  Proxmox user %s", setup.UserID)
	}
	if p.role {
		r.ask.Info("  Proxmox role %s", setup.RoleID)
	}
	for _, id := range p.grantRoles {
		r.ask.Info("  Proxmox role %s", id)
	}
	if p.pool {
		r.ask.Info("  Proxmox pool %s", poolID)
	}
	if len(p.tags) > 0 {
		r.ask.Info("  the registered tags %s", strings.Join(p.tags, ", "))
	}
	switch {
	case p.template == "":
	case o.KeepTemplate:
		r.ask.Info("  (the template %s stays; --keep-template=false removes it)", p.template)
	default:
		r.ask.Info("  the template %s", p.template)
	}
	for _, k := range p.kept {
		r.ask.Info("  (kept: %s)", k)
	}
	for _, n := range p.notes {
		r.ask.Info("  (%s)", n)
	}
	switch {
	case s.originalNode != "":
	case o.KeepCloudflare:
		r.ask.Info("  (what the install has at Cloudflare stays, and nothing on this node can remove it later)")
	case !s.running && s.exists:
		r.ask.Info("  (the container is stopped: what its install has at Cloudflare cannot be looked at, and stays)")
	case s.cloudflareErr != nil:
		r.ask.Info("  (what the install has at Cloudflare cannot be listed: %v)", s.cloudflareErr)
	case len(s.cloudflare) > 0 && o.PurgeCloudflare:
		r.ask.Info("  at Cloudflare, what the install has:")
		for _, c := range s.cloudflare {
			r.ask.Info("    %s", c)
		}
	case len(s.cloudflare) > 0:
		r.ask.Info("  (what the install has at Cloudflare is asked about next)")
	}
}

// decidePurge decides what becomes of what the install has at Cloudflare, as
// pco uninstall does: --yes alone never decides it while there is something
// there, as the credentials that reach it go with the container. What a copy
// beside its original reaches is the original's, and stays.
func (r *run) decidePurge(vmid int, s survey, o UninstallOptions) (bool, error) {
	switch {
	case s.originalNode != "" && o.PurgeCloudflare:
		return false, fmt.Errorf("lxc/%d is a copy of lxc/%d, which is still there: what is at Cloudflare is the install of "+
			"lxc/%d, which --purge-cloudflare would delete under it; remove the copy without it; nothing was removed",
			vmid, s.described, s.described)
	case s.originalNode != "" || o.KeepCloudflare || !s.exists:
		return false, nil
	case o.PurgeCloudflare && !s.running:
		return false, fmt.Errorf("--purge-cloudflare needs lxc/%d running, as its credentials are in it: start it with "+
			"pct start %d and run the uninstall again; nothing was removed", vmid, vmid)
	case !s.running && o.Yes:
		return false, fmt.Errorf("lxc/%d is stopped, so what its install has at Cloudflare cannot be looked at, and the "+
			"uninstall removes the credentials that reach it: start it and pass --purge-cloudflare, or pass "+
			"--keep-cloudflare to leave it; nothing was removed", vmid)
	case !s.running:
		return false, nil
	case s.cloudflareErr != nil:
		return false, fmt.Errorf("what the install of lxc/%d has at Cloudflare cannot be listed (%w): run the uninstall "+
			"again once Cloudflare answers, or with --keep-cloudflare to leave it; nothing was removed", vmid, s.cloudflareErr)
	case o.PurgeCloudflare:
		return true, nil
	case len(s.cloudflare) == 0:
		return false, nil
	case o.Yes:
		return false, fmt.Errorf("the install of lxc/%d has DNS records or tunnels at Cloudflare, and the uninstall "+
			"removes the credentials that reach them: say what becomes of them with --purge-cloudflare, which deletes "+
			"them, or --keep-cloudflare, which leaves them; nothing was removed", vmid)
	}
	r.ask.Info("at Cloudflare, what the install of lxc/%d has:", vmid)
	for _, c := range s.cloudflare {
		r.ask.Info("  %s", c)
	}
	return r.ask.Confirm("Delete these at Cloudflare as well?", false)
}

func (r *run) remove(ctx context.Context, vmid int, s survey, p plan, purge bool) error {
	if purge {
		out, err := r.pco(ctx, vmid, "appliance", "purge")
		for line := range strings.Lines(out) {
			if line = strings.TrimSpace(line); line != "" {
				r.ask.Info("  lxc/%d: %s", vmid, line)
			}
		}
		if err != nil {
			return fmt.Errorf("deleting at Cloudflare through lxc/%d: %w; nothing else was removed, run the uninstall again", vmid, err)
		}
	}
	var failed []string
	fail := func(err error) {
		if err != nil {
			failed = append(failed, err.Error())
			r.ask.Warn("%v", err)
		}
	}
	gone := true
	if p.container {
		err := r.destroyContainer(ctx, vmid, s.running)
		fail(err)
		gone = err == nil
	}
	if p.token {
		if err := r.removeToken(ctx, vmid); err != nil {
			// A user that goes leaves the secrets of its tokens behind, and its
			// grant goes with the role.
			fail(fmt.Errorf("%w; user %s and role %s are kept until it is gone", err, setup.UserID, setup.RoleID))
			p.user, p.role = false, false
		}
	}
	for _, l := range p.grantLines {
		fail(r.deleteLine(ctx, l))
	}
	for _, l := range p.noAccess {
		if !gone {
			// They keep principals away from the secrets on its volume.
			r.ask.Warn("NoAccess for %s on %s, which keeps %s away from the secrets of lxc/%d: kept while it is there",
				l.UGID, l.Path, l.UGID, vmid)
			continue
		}
		fail(r.deleteLine(ctx, l))
	}
	if p.user {
		fail(r.removeObject(ctx, "user "+setup.UserID, "pveum", "user", "delete", setup.UserID))
	}
	if p.role {
		fail(r.removeObject(ctx, "role "+setup.RoleID, "pveum", "role", "delete", setup.RoleID))
	}
	for _, id := range p.grantRoles {
		fail(r.removeObject(ctx, "role "+id, "pveum", "role", "delete", id))
	}
	if p.pool {
		fail(r.removeObject(ctx, "pool "+poolID, "pveum", "pool", "delete", poolID))
	}
	if len(p.tags) > 0 {
		fail(r.removeTags(ctx, p.tags))
	}
	if p.template != "" && !r.o.KeepTemplate {
		fail(r.removeObject(ctx, "template "+p.template, "pvesm", "free", p.template))
	}
	if len(failed) > 0 {
		return fmt.Errorf("the uninstall did not finish: %s; run it again to finish the rest", strings.Join(failed, "; "))
	}
	r.ask.Info("the appliance lxc/%d is removed from this node", vmid)
	return nil
}

func (r *run) removeObject(ctx context.Context, what, name string, args ...string) error {
	if _, err := r.r.Run(ctx, name, args...); err != nil {
		return fmt.Errorf("removing %s: %w", what, err)
	}
	r.ask.Info("%s: removed", what)
	return nil
}

// destroyContainer takes the protection off the container, stops it and
// destroys it with what refers to it: its ACL lines and its pool membership go
// with it.
func (r *run) destroyContainer(ctx context.Context, vmid int, running bool) error {
	id := strconv.Itoa(vmid)
	if _, err := r.r.Run(ctx, "pct", "set", id, "--protection", "0"); err != nil {
		return fmt.Errorf("taking the protection off lxc/%d: %w", vmid, err)
	}
	if running {
		if _, err := r.r.Run(ctx, "pct", "stop", id); err != nil {
			return fmt.Errorf("stopping lxc/%d: %w", vmid, err)
		}
	}
	if _, err := r.r.Run(ctx, "pct", "destroy", id, "--purge", "1"); err != nil {
		return fmt.Errorf("destroying lxc/%d: %w", vmid, err)
	}
	r.ask.Info("container lxc/%d: destroyed", vmid)
	return nil
}

// removeToken removes the token of the appliance, before its user: deleting
// a user leaves the secrets of its tokens behind. A line of the token
// that outlives it is deleted too.
func (r *run) removeToken(ctx context.Context, vmid int) error {
	id := tokenID(vmid)
	if _, err := r.r.Run(ctx, "pveum", "user", "token", "remove", setup.UserID, tokenName(vmid)); err != nil {
		return fmt.Errorf("removing token %s: %w", id, err)
	}
	r.ask.Info("token %s: removed", id)
	acl, err := aclLines(ctx, r.r)
	if err != nil {
		return err
	}
	for _, l := range acl {
		if l.Type == "token" && l.UGID == id {
			if err := r.deleteLine(ctx, l); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *run) deleteLine(ctx context.Context, l aclLine) error {
	if _, err := r.r.Run(ctx, "pveum", "acl", "delete", l.Path, "--"+l.Type+"s", l.UGID, "--roles", l.Role); err != nil {
		return fmt.Errorf("revoking role %s on %s from %s: %w", l.Role, l.Path, l.UGID, err)
	}
	r.ask.Info("acl %s: revoked role %s from %s", l.Path, l.Role, l.UGID)
	return nil
}

// removeTags takes tags out of the registered tags, and keeps the others in
// their order.
func (r *run) removeTags(ctx context.Context, tags []string) error {
	now, err := setup.RegisteredTags(ctx, r.r)
	if err != nil {
		return err
	}
	keep := slices.DeleteFunc(slices.Clone(now), func(t string) bool { return slices.Contains(tags, t) })
	if len(keep) == len(now) {
		return nil
	}
	if err := setup.SetRegisteredTags(ctx, r.r, keep); err != nil {
		return err
	}
	r.ask.Info("registered tags: removed %s", strings.Join(tags, ", "))
	return nil
}
