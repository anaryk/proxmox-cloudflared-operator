package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// PVEVersion is the version of pve-manager.
type PVEVersion struct {
	major, minor int
	text         string
}

var versionPattern = regexp.MustCompile(`^pve-manager/((\d+)\.(\d+)[^/\s]*)/`)

// ParsePVEVersion reads what pveversion prints: "pve-manager/9.0.10/<hash>
// (running kernel: ...)".
func ParsePVEVersion(out string) (PVEVersion, error) {
	m := versionPattern.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return PVEVersion{}, fmt.Errorf("pveversion printed %q, which is not a version of pve-manager", printableText(out, 80))
	}
	major, err1 := strconv.Atoi(m[2])
	minor, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil {
		return PVEVersion{}, fmt.Errorf("the version %q of pve-manager is out of range", m[1])
	}
	return PVEVersion{major: major, minor: minor, text: m[1]}, nil
}

// Supported reports whether pco runs on this version: 8.4 or later, or 9.
func (v PVEVersion) Supported() bool { return v.major == 9 || v.major == 8 && v.minor >= 4 }

// String is the version as pveversion printed it, such as 9.0.10.
func (v PVEVersion) String() string { return v.text }

// privileges is what role PCO grants: reading the guests, the node, the
// addresses the guest agent reports, the SDN and the pools, without which the
// cluster resources leave out the pool of every guest. Proxmox VE 8 has the
// guest agent under VM.Monitor.
func (v PVEVersion) privileges() []string {
	return append(v.earlierPrivileges(), "Pool.Audit")
}

// RolePrivileges is what role PCO grants on a Proxmox VE release of major
// version major.
func RolePrivileges(major int) []string { return PVEVersion{major: major}.privileges() }

// earlierPrivileges is what setup gave the role before it read pools.
func (v PVEVersion) earlierPrivileges() []string {
	if v.major == 8 {
		return []string{"VM.Audit", "Sys.Audit", "VM.Monitor", "SDN.Audit"}
	}
	return []string{"VM.Audit", "Sys.Audit", "VM.GuestAgent.Audit", "SDN.Audit"}
}

// IsPCORole reports whether privs are what setup gives role PCO, or gave it
// before, on any version, and nothing else: the mark of the role, which has no
// comment of its own.
func IsPCORole(privs []string) bool {
	return slices.ContainsFunc(setupPrivileges(), func(set []string) bool { return sameSet(privs, set) })
}

// setupPrivileges are the privilege sets setup gives the role, or gave it
// before, on any version.
func setupPrivileges() [][]string {
	var out [][]string
	for _, v := range []PVEVersion{{major: 9}, {major: 8}} {
		out = append(out, v.privileges(), v.earlierPrivileges())
	}
	return out
}

// pveList is a list Proxmox gives as one string, separated by commas,
// semicolons or spaces; a JSON list is read as well.
type pveList []string

func (l *pveList) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*l = strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) })
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("want a string or a list of strings")
	}
	*l = list
	return nil
}

// pveBool is a flag Proxmox gives as 0 or 1, as a number or a string.
type pveBool bool

func (f *pveBool) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "1", "true":
		*f = true
	case "0", "false", "":
		*f = false
	default:
		return errors.New("want 0 or 1")
	}
	return nil
}

type pveRole struct {
	ID    string  `json:"roleid"`
	Privs pveList `json:"privs"`
}

type pveUser struct {
	ID     string   `json:"userid"`
	Enable *pveBool `json:"enable"` // absent: enabled
	Expire pveInt   `json:"expire"` // seconds since the epoch; 0: never
}

type pveACL struct {
	Path      string   `json:"path"`
	Type      string   `json:"type"`
	UGID      string   `json:"ugid"`
	Role      string   `json:"roleid"`
	Propagate *pveBool `json:"propagate"` // absent: propagated, the default
}

type pveToken struct {
	ID      string   `json:"tokenid"`
	Privsep *pveBool `json:"privsep"`
	Expire  pveInt   `json:"expire"` // seconds since the epoch; 0: never
}

// pveInt is a number Proxmox gives as a number or a string.
type pveInt int64

func (n *pveInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("want a whole number")
	}
	*n = pveInt(v)
	return nil
}

// query runs a command that prints JSON and reads it into v.
func query(ctx context.Context, r Runner, v any, name string, args ...string) error {
	out, err := r.Run(ctx, name, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("%s %s printed what is not the JSON expected: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (s *Setup) query(ctx context.Context, v any, name string, args ...string) error {
	return query(ctx, s.run, v, name, args...)
}

func findRole(ctx context.Context, r Runner) (pveRole, bool, error) {
	var roles []pveRole
	if err := query(ctx, r, &roles, "pveum", "role", "list", "--output-format", "json"); err != nil {
		return pveRole{}, false, fmt.Errorf("listing the roles: %w", err)
	}
	i := slices.IndexFunc(roles, func(r pveRole) bool { return r.ID == RoleID })
	if i < 0 {
		return pveRole{}, false, nil
	}
	return roles[i], true, nil
}

func (s *Setup) role(ctx context.Context) (pveRole, bool, error) { return findRole(ctx, s.run) }

func userExists(ctx context.Context, r Runner) (bool, error) {
	var users []pveUser
	if err := query(ctx, r, &users, "pveum", "user", "list", "--output-format", "json"); err != nil {
		return false, fmt.Errorf("listing the users: %w", err)
	}
	return slices.ContainsFunc(users, func(u pveUser) bool { return u.ID == UserID }), nil
}

func (s *Setup) userExists(ctx context.Context) (bool, error) { return userExists(ctx, s.run) }

// listACL returns the access control list.
func listACL(ctx context.Context, r Runner) ([]pveACL, error) {
	var acl []pveACL
	if err := query(ctx, r, &acl, "pveum", "acl", "list", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the access control list: %w", err)
	}
	return acl, nil
}

func (s *Setup) acl(ctx context.Context) ([]pveACL, error) { return listACL(ctx, s.run) }

// isGrant reports whether an entry is the grant setup makes: role PCO on /
// to pco@pve.
func isGrant(a pveACL) bool {
	return a.Path == "/" && a.Type == "user" && a.UGID == UserID && a.Role == RoleID
}

func (s *Setup) findToken(ctx context.Context) (pveToken, bool, error) {
	var tokens []pveToken
	if err := s.query(ctx, &tokens, "pveum", "user", "token", "list", UserID, "--output-format", "json"); err != nil {
		return pveToken{}, false, fmt.Errorf("listing the tokens of %s: %w", UserID, err)
	}
	i := slices.IndexFunc(tokens, func(t pveToken) bool { return t.ID == tokenName })
	if i < 0 {
		return pveToken{}, false, nil
	}
	return tokens[i], true, nil
}

func (s *Setup) removeToken(ctx context.Context) error {
	if _, err := s.run.Run(ctx, "pveum", "user", "token", "remove", UserID, tokenName); err != nil {
		return fmt.Errorf("removing token %s: %w", tokenID, err)
	}
	return nil
}

// RegisteredTags returns the tags only an admin may set, in their order.
func RegisteredTags(ctx context.Context, r Runner) ([]string, error) {
	var options struct {
		Tags pveList `json:"registered-tags"`
	}
	if err := query(ctx, r, &options, "pvesh", "get", "/cluster/options", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("reading the cluster options: %w", err)
	}
	return options.Tags, nil
}

func (s *Setup) registeredTags(ctx context.Context) ([]string, error) {
	return RegisteredTags(ctx, s.run)
}

// SetRegisteredTags makes tags the registered tags, in their order.
func SetRegisteredTags(ctx context.Context, r Runner, tags []string) error {
	args := []string{"set", "/cluster/options", "--registered-tags", strings.Join(tags, ";")}
	if len(tags) == 0 {
		args = []string{"set", "/cluster/options", "--delete", "registered-tags"}
	}
	if _, err := r.Run(ctx, "pvesh", args...); err != nil {
		return fmt.Errorf("setting the registered tags: %w", err)
	}
	return nil
}

func (s *Setup) setRegisteredTags(ctx context.Context, tags []string) error {
	return SetRegisteredTags(ctx, s.run, tags)
}

// without returns the elements of a that are not in b, in the order of a.
func without(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func sameSet(a, b []string) bool { return len(without(a, b)) == 0 && len(without(b, a)) == 0 }

// Objects makes the objects in Proxmox that pco setup and the installer of
// the appliance share: role PCO, user pco@pve with the grant of the role on /,
// and the registered gate tags. Each looks at what is there first.
type Objects struct {
	Run     Runner
	Ask     Prompter
	Version PVEVersion
	// Record notes in a manifest what is about to be created, and writes it,
	// before the command that creates it runs: if the object is there later,
	// it is pco's, also when the run is killed right after making it.
	Record func(change func(*Manifest)) error
}

func (r *run) objects() Objects {
	return Objects{Run: r.run, Ask: r.ask, Version: r.version, Record: r.record}
}

func (r *run) ensureRole(ctx context.Context) error { return r.objects().EnsureRole(ctx) }

func (r *run) ensureUser(ctx context.Context) error { return r.objects().EnsureUser(ctx) }

// takeBack takes back the note of a create that failed, when it left
// nothing: what an admin makes in its place later is the admin's. When that
// cannot be told, the note stays.
func (o Objects) takeBack(there func() (bool, error), unnote func(*Manifest)) {
	if present, err := there(); err == nil && !present {
		if err := o.Record(unnote); err != nil {
			o.Ask.Warn("%v", err)
		}
	}
}

// EnsureRole makes role PCO, or adds to it what it lacks of the privileges of
// this version; what an admin granted besides stays.
func (o Objects) EnsureRole(ctx context.Context) error {
	want := o.Version.privileges()
	role, found, err := findRole(ctx, o.Run)
	if err != nil {
		return err
	}
	if !found {
		if err := o.Record(func(m *Manifest) { m.CreatedRole = true }); err != nil {
			return err
		}
		if _, err := o.Run.Run(ctx, "pveum", "role", "add", RoleID, "--privs", strings.Join(want, ",")); err != nil {
			o.takeBack(func() (bool, error) {
				_, found, err := findRole(ctx, o.Run)
				return found, err
			}, func(m *Manifest) { m.CreatedRole = false })
			return fmt.Errorf("creating role %s: %w", RoleID, err)
		}
		o.Ask.Info("role %s: created with %s", RoleID, strings.Join(want, ", "))
		return nil
	}
	missing := without(want, role.Privs)
	if len(missing) == 0 {
		o.Ask.Info("role %s: nothing needed", RoleID)
	} else {
		// Appended, so that what an admin granted besides stays.
		if _, err := o.Run.Run(ctx, "pveum", "role", "modify", RoleID, "--append", "1", "--privs", strings.Join(missing, ",")); err != nil {
			return fmt.Errorf("adding %s to role %s: %w", strings.Join(missing, ", "), RoleID, err)
		}
		o.Ask.Info("role %s: added %s", RoleID, strings.Join(missing, ", "))
	}
	if extra := without(role.Privs, want); len(extra) > 0 {
		o.Ask.Warn("role %s also grants %s, which pco does not need; kept", RoleID, strings.Join(extra, ", "))
	}
	return nil
}

// EnsureUser makes user pco@pve and grants it role PCO on /.
func (o Objects) EnsureUser(ctx context.Context) error {
	exists, err := userExists(ctx, o.Run)
	if err != nil {
		return err
	}
	var did []string
	if !exists {
		if err := o.Record(func(m *Manifest) { m.CreatedUser = true }); err != nil {
			return err
		}
		if _, err := o.Run.Run(ctx, "pveum", "user", "add", UserID, "--comment", UserComment); err != nil {
			o.takeBack(func() (bool, error) { return userExists(ctx, o.Run) }, func(m *Manifest) { m.CreatedUser = false })
			return fmt.Errorf("creating user %s: %w", UserID, err)
		}
		did = append(did, "created")
	}
	acl, err := listACL(ctx, o.Run)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(acl, func(a pveACL) bool { return isGrant(a) && (a.Propagate == nil || bool(*a.Propagate)) }) {
		if err := o.Record(func(m *Manifest) { m.GrantedACL = true }); err != nil {
			return err
		}
		if _, err := o.Run.Run(ctx, "pveum", "acl", "modify", "/", "--users", UserID, "--roles", RoleID); err != nil {
			o.takeBack(func() (bool, error) {
				acl, err := listACL(ctx, o.Run)
				return slices.ContainsFunc(acl, isGrant), err
			}, func(m *Manifest) { m.GrantedACL = false })
			return fmt.Errorf("granting role %s on / to %s: %w", RoleID, UserID, err)
		}
		did = append(did, "granted role "+RoleID+" on /")
	}
	if len(did) == 0 {
		o.Ask.Info("user %s: nothing needed", UserID)
	} else {
		o.Ask.Info("user %s: %s", UserID, strings.Join(did, ", "))
	}
	return nil
}

// ensureToken makes sure that Proxmox has the token of the daemon and that its
// secret is stored. Proxmox shows a secret once, when it makes the token, so a
// token whose secret is not stored is made anew.
func (r *run) ensureToken(ctx context.Context) error {
	stored, found, err := r.st.PVEToken()
	if err != nil {
		return fmt.Errorf("reading the stored Proxmox token: %w", err)
	}
	tok, exists, err := r.findToken(ctx)
	if err != nil {
		return err
	}
	separated := exists && tok.Privsep != nil && bool(*tok.Privsep)
	switch {
	case exists && found && stored.TokenID == tokenID && !separated:
		err := r.host.checkToken(ctx, stored)
		if err == nil {
			r.ask.Info("token %s: nothing needed", tokenID)
			return nil
		}
		if !refusedByProxmox(err) {
			return fmt.Errorf("checking the stored secret of token %s with Proxmox: %w", tokenID, err)
		}
		// A user that is disabled or expired, and a token that expired, are
		// refused as well, and a new token would be refused just the same.
		why, err := r.whyRefused(ctx, tok)
		if err != nil {
			return err
		}
		if why != "" {
			return fmt.Errorf("the stored secret of token %s is refused: %s, then run pco setup again", tokenID, why)
		}
		r.ask.Warn("Proxmox refuses the stored secret of token %s: making it anew", tokenID)
	case separated:
		r.ask.Warn("token %s is privilege separated and lacks the privileges of role %s: making it anew", tokenID, RoleID)
	case exists:
		r.ask.Warn("token %s is there, but its secret is not stored on this node and cannot be read back from Proxmox: making it anew", tokenID)
	}
	if exists {
		if err := r.removeToken(ctx); err != nil {
			return err
		}
	}
	return r.createToken(ctx)
}

func (r *run) createToken(ctx context.Context) error {
	if err := r.record(func(m *Manifest) { m.CreatedToken = true }); err != nil {
		return err
	}
	out, err := r.run.Run(ctx, "pveum", "user", "token", "add", UserID, tokenName, "--privsep", "0", "--output-format", "json")
	if err != nil {
		r.takeBack(func() (bool, error) {
			_, found, err := r.findToken(ctx)
			return found, err
		}, func(m *Manifest) { m.CreatedToken = false })
		return fmt.Errorf("creating token %s: %w", tokenID, err)
	}
	secret, err := tokenSecret(out)
	if err != nil {
		return err
	}
	if err := r.st.SavePVEToken(store.PVEToken{TokenID: tokenID, Secret: store.NewSecret(secret)}); err != nil {
		return fmt.Errorf("storing the Proxmox token: %w", err)
	}
	r.newPVEToken = true
	r.ask.Info("token %s: created", tokenID)
	return nil
}

// whyRefused says what explains that Proxmox refuses the token, if the user
// or the token does: a user that is disabled or expired, a token that
// expired. Empty when neither does.
func (r *run) whyRefused(ctx context.Context, tok pveToken) (string, error) {
	var users []pveUser
	if err := r.query(ctx, &users, "pveum", "user", "list", "--output-format", "json"); err != nil {
		return "", fmt.Errorf("listing the users: %w", err)
	}
	now := r.now().Unix()
	expired := func(at pveInt) bool { return at > 0 && int64(at) <= now }
	date := func(at pveInt) string { return time.Unix(int64(at), 0).UTC().Format(time.DateOnly) }
	if i := slices.IndexFunc(users, func(u pveUser) bool { return u.ID == UserID }); i >= 0 {
		switch u := users[i]; {
		case u.Enable != nil && !bool(*u.Enable):
			return fmt.Sprintf("user %s is disabled; enable it with pveum user modify %s --enable 1", UserID, UserID), nil
		case expired(u.Expire):
			return fmt.Sprintf("user %s expired on %s; lift that with pveum user modify %s --expire 0",
				UserID, date(u.Expire), UserID), nil
		}
	}
	if expired(tok.Expire) {
		return fmt.Sprintf("token %s expired on %s; remove it with pveum user token remove %s %s, and setup makes it anew",
			tokenID, date(tok.Expire), UserID, tokenName), nil
	}
	return "", nil
}

// refusedByProxmox reports whether Proxmox refused a token: an answer 401.
func refusedByProxmox(err error) bool {
	var apiErr *pve.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

func tokenSecret(out string) (string, error) { return TokenSecret(out, tokenID) }

// TokenSecret reads the secret of token fullID (user@realm!name) from what
// pveum user token add --output-format json printed. That holds the secret,
// so no error quotes it, not even in part.
func TokenSecret(out, fullID string) (string, error) {
	var answer struct {
		FullID string `json:"full-tokenid"`
		Value  string `json:"value"`
	}
	if json.Unmarshal([]byte(out), &answer) != nil {
		return "", errors.New("pveum user token add printed what is not the JSON expected")
	}
	if answer.FullID != "" && answer.FullID != fullID {
		return "", fmt.Errorf("pveum user token add made another token than %s", fullID)
	}
	if strings.TrimSpace(answer.Value) == "" {
		return "", errors.New("pveum user token add printed no secret")
	}
	return answer.Value, nil
}

func (r *run) ensureTags(ctx context.Context) error {
	settings, err := r.st.Settings()
	if err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	r.gate = settings.GateTag
	register, err := r.choose(r.o.RegisterTags, true, TagsQuestion(r.gate))
	if err != nil {
		return err
	}
	return r.objects().EnsureTags(ctx, r.gate, register)
}

// TagsQuestion is the question whether to register the gate tags, whose
// default answer is yes.
func TagsQuestion(gate string) string {
	return fmt.Sprintf("Register the gate tags %s, so that only admins can set them on a guest?", strings.Join(wantedTags(gate), ", "))
}

// EnsureTags registers the tags of the gate tag gate, or with register false
// says what leaving them open allows.
func (o Objects) EnsureTags(ctx context.Context, gate string, register bool) error {
	wanted := wantedTags(gate)
	tags, err := RegisteredTags(ctx, o.Run)
	if err != nil {
		return err
	}
	if !register {
		o.warnOpenGate(gate, tags)
		return nil
	}
	if missing := without(wanted, tags); len(missing) == 0 {
		o.Ask.Info("registered tags: nothing needed")
	} else {
		if err := o.Record(func(m *Manifest) { m.addTags(missing) }); err != nil {
			return err
		}
		if err := SetRegisteredTags(ctx, o.Run, append(tags, missing...)); err != nil {
			if now, rerr := RegisteredTags(ctx, o.Run); rerr == nil {
				if absent := without(missing, now); len(absent) > 0 {
					o.takeBack(func() (bool, error) { return false, nil },
						func(m *Manifest) { m.RegisteredTags = without(m.RegisteredTags, absent) })
				}
			}
			return err
		}
		o.Ask.Info("registered tags: added %s", strings.Join(missing, ", "))
	}
	o.Ask.Info("note: clones and restores of a guest keep its tags, so the clone of a published guest asks to be published too")
	return nil
}

// warnOpenGate says what a declined registration leaves open: with the gate
// tag not registered, whoever may edit the options of a guest can set it.
func (o Objects) warnOpenGate(gate string, registered []string) {
	if slices.Contains(registered, gate) {
		o.Ask.Info("registered tags: skipped; the gate tag %s is registered already", gate)
		return
	}
	o.Ask.Warn("registered tags: skipped, and the gate tag %s is not registered: any user who may edit the options of a guest "+
		"(VM.Config.Options) can set it and so publish the guest; pco setup --repair registers it", gate)
}
