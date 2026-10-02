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

// pveVersion is the version of pve-manager.
type pveVersion struct {
	major, minor int
	text         string
}

var versionPattern = regexp.MustCompile(`^pve-manager/((\d+)\.(\d+)[^/\s]*)/`)

// parsePVEVersion reads what pveversion prints: "pve-manager/9.0.10/<hash>
// (running kernel: ...)".
func parsePVEVersion(out string) (pveVersion, error) {
	m := versionPattern.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return pveVersion{}, fmt.Errorf("pveversion printed %q, which is not a version of pve-manager", printableText(out, 80))
	}
	major, err1 := strconv.Atoi(m[2])
	minor, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil {
		return pveVersion{}, fmt.Errorf("the version %q of pve-manager is out of range", m[1])
	}
	return pveVersion{major: major, minor: minor, text: m[1]}, nil
}

func (v pveVersion) supported() bool { return v.major == 9 || v.major == 8 && v.minor >= 4 }

// privileges is what role PCO grants: reading the guests, the node, the
// addresses the guest agent reports and the SDN. Proxmox VE 8 has the guest
// agent under VM.Monitor.
func (v pveVersion) privileges() []string {
	if v.major == 8 {
		return []string{"VM.Audit", "Sys.Audit", "VM.Monitor", "SDN.Audit"}
	}
	return []string{"VM.Audit", "Sys.Audit", "VM.GuestAgent.Audit", "SDN.Audit"}
}

// setupPrivileges are the privilege sets setup gives the role, on any
// version.
func setupPrivileges() [][]string {
	return [][]string{pveVersion{major: 9}.privileges(), pveVersion{major: 8}.privileges()}
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
func (s *Setup) query(ctx context.Context, v any, name string, args ...string) error {
	out, err := s.run.Run(ctx, name, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("%s %s printed what is not the JSON expected: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (s *Setup) role(ctx context.Context) (pveRole, bool, error) {
	var roles []pveRole
	if err := s.query(ctx, &roles, "pveum", "role", "list", "--output-format", "json"); err != nil {
		return pveRole{}, false, fmt.Errorf("listing the roles: %w", err)
	}
	i := slices.IndexFunc(roles, func(r pveRole) bool { return r.ID == roleID })
	if i < 0 {
		return pveRole{}, false, nil
	}
	return roles[i], true, nil
}

func (s *Setup) userExists(ctx context.Context) (bool, error) {
	var users []pveUser
	if err := s.query(ctx, &users, "pveum", "user", "list", "--output-format", "json"); err != nil {
		return false, fmt.Errorf("listing the users: %w", err)
	}
	return slices.ContainsFunc(users, func(u pveUser) bool { return u.ID == userID }), nil
}

// acl returns the access control list.
func (s *Setup) acl(ctx context.Context) ([]pveACL, error) {
	var acl []pveACL
	if err := s.query(ctx, &acl, "pveum", "acl", "list", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("listing the access control list: %w", err)
	}
	return acl, nil
}

// isGrant reports whether an entry is the grant setup makes: role PCO on /
// to pco@pve.
func isGrant(a pveACL) bool {
	return a.Path == "/" && a.Type == "user" && a.UGID == userID && a.Role == roleID
}

func (s *Setup) findToken(ctx context.Context) (pveToken, bool, error) {
	var tokens []pveToken
	if err := s.query(ctx, &tokens, "pveum", "user", "token", "list", userID, "--output-format", "json"); err != nil {
		return pveToken{}, false, fmt.Errorf("listing the tokens of %s: %w", userID, err)
	}
	i := slices.IndexFunc(tokens, func(t pveToken) bool { return t.ID == tokenName })
	if i < 0 {
		return pveToken{}, false, nil
	}
	return tokens[i], true, nil
}

func (s *Setup) removeToken(ctx context.Context) error {
	if _, err := s.run.Run(ctx, "pveum", "user", "token", "remove", userID, tokenName); err != nil {
		return fmt.Errorf("removing token %s: %w", tokenID, err)
	}
	return nil
}

// registeredTags returns the tags only an admin may set, in their order.
func (s *Setup) registeredTags(ctx context.Context) ([]string, error) {
	var options struct {
		Tags pveList `json:"registered-tags"`
	}
	if err := s.query(ctx, &options, "pvesh", "get", "/cluster/options", "--output-format", "json"); err != nil {
		return nil, fmt.Errorf("reading the cluster options: %w", err)
	}
	return options.Tags, nil
}

func (s *Setup) setRegisteredTags(ctx context.Context, tags []string) error {
	args := []string{"set", "/cluster/options", "--registered-tags", strings.Join(tags, ";")}
	if len(tags) == 0 {
		args = []string{"set", "/cluster/options", "--delete", "registered-tags"}
	}
	if _, err := s.run.Run(ctx, "pvesh", args...); err != nil {
		return fmt.Errorf("setting the registered tags: %w", err)
	}
	return nil
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

func (r *run) ensureRole(ctx context.Context) error {
	want := r.version.privileges()
	role, found, err := r.role(ctx)
	if err != nil {
		return err
	}
	if !found {
		if err := r.record(func(m *Manifest) { m.CreatedRole = true }); err != nil {
			return err
		}
		if _, err := r.run.Run(ctx, "pveum", "role", "add", roleID, "--privs", strings.Join(want, ",")); err != nil {
			r.takeBack(func() (bool, error) {
				_, found, err := r.role(ctx)
				return found, err
			}, func(m *Manifest) { m.CreatedRole = false })
			return fmt.Errorf("creating role %s: %w", roleID, err)
		}
		r.ask.Info("role %s: created with %s", roleID, strings.Join(want, ", "))
		return nil
	}
	missing := without(want, role.Privs)
	if len(missing) == 0 {
		r.ask.Info("role %s: nothing needed", roleID)
	} else {
		// Appended, so that what an admin granted besides stays.
		if _, err := r.run.Run(ctx, "pveum", "role", "modify", roleID, "--append", "1", "--privs", strings.Join(missing, ",")); err != nil {
			return fmt.Errorf("adding %s to role %s: %w", strings.Join(missing, ", "), roleID, err)
		}
		r.ask.Info("role %s: added %s", roleID, strings.Join(missing, ", "))
	}
	if extra := without(role.Privs, want); len(extra) > 0 {
		r.ask.Warn("role %s also grants %s, which pco does not need; kept", roleID, strings.Join(extra, ", "))
	}
	return nil
}

func (r *run) ensureUser(ctx context.Context) error {
	exists, err := r.userExists(ctx)
	if err != nil {
		return err
	}
	var did []string
	if !exists {
		if err := r.record(func(m *Manifest) { m.CreatedUser = true }); err != nil {
			return err
		}
		if _, err := r.run.Run(ctx, "pveum", "user", "add", userID, "--comment", userComment); err != nil {
			r.takeBack(func() (bool, error) { return r.userExists(ctx) }, func(m *Manifest) { m.CreatedUser = false })
			return fmt.Errorf("creating user %s: %w", userID, err)
		}
		did = append(did, "created")
	}
	acl, err := r.acl(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(acl, func(a pveACL) bool { return isGrant(a) && (a.Propagate == nil || bool(*a.Propagate)) }) {
		if err := r.record(func(m *Manifest) { m.GrantedACL = true }); err != nil {
			return err
		}
		if _, err := r.run.Run(ctx, "pveum", "acl", "modify", "/", "--users", userID, "--roles", roleID); err != nil {
			r.takeBack(func() (bool, error) {
				acl, err := r.acl(ctx)
				return slices.ContainsFunc(acl, isGrant), err
			}, func(m *Manifest) { m.GrantedACL = false })
			return fmt.Errorf("granting role %s on / to %s: %w", roleID, userID, err)
		}
		did = append(did, "granted role "+roleID+" on /")
	}
	if len(did) == 0 {
		r.ask.Info("user %s: nothing needed", userID)
	} else {
		r.ask.Info("user %s: %s", userID, strings.Join(did, ", "))
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
		r.ask.Warn("token %s is privilege separated and lacks the privileges of role %s: making it anew", tokenID, roleID)
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
	out, err := r.run.Run(ctx, "pveum", "user", "token", "add", userID, tokenName, "--privsep", "0", "--output-format", "json")
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
	if i := slices.IndexFunc(users, func(u pveUser) bool { return u.ID == userID }); i >= 0 {
		switch u := users[i]; {
		case u.Enable != nil && !bool(*u.Enable):
			return fmt.Sprintf("user %s is disabled; enable it with pveum user modify %s --enable 1", userID, userID), nil
		case expired(u.Expire):
			return fmt.Sprintf("user %s expired on %s; lift that with pveum user modify %s --expire 0",
				userID, date(u.Expire), userID), nil
		}
	}
	if expired(tok.Expire) {
		return fmt.Sprintf("token %s expired on %s; remove it with pveum user token remove %s %s, and setup makes it anew",
			tokenID, date(tok.Expire), userID, tokenName), nil
	}
	return "", nil
}

// refusedByProxmox reports whether Proxmox refused a token: an answer 401.
func refusedByProxmox(err error) bool {
	var apiErr *pve.APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// tokenSecret reads the secret from what pveum user token add printed. That
// holds the secret, so no error quotes it, not even in part.
func tokenSecret(out string) (string, error) {
	var answer struct {
		FullID string `json:"full-tokenid"`
		Value  string `json:"value"`
	}
	if json.Unmarshal([]byte(out), &answer) != nil {
		return "", errors.New("pveum user token add printed what is not the JSON expected")
	}
	if answer.FullID != "" && answer.FullID != tokenID {
		return "", fmt.Errorf("pveum user token add made another token than %s", tokenID)
	}
	if strings.TrimSpace(answer.Value) == "" {
		return "", errors.New("pveum user token add printed no secret")
	}
	return answer.Value, nil
}

func (r *run) ensureTags(ctx context.Context) error {
	register, err := r.choose(r.o.RegisterTags, true, fmt.Sprintf(
		"Register the gate tags %s, so that only admins can set them on a guest?", strings.Join(gateTags(), " and ")))
	if err != nil {
		return err
	}
	if !register {
		r.ask.Info("registered tags: skipped; whoever may edit a guest can set the gate tags")
		return nil
	}
	tags, err := r.registeredTags(ctx)
	if err != nil {
		return err
	}
	if missing := without(gateTags(), tags); len(missing) == 0 {
		r.ask.Info("registered tags: nothing needed")
	} else {
		if err := r.record(func(m *Manifest) { m.addTags(missing) }); err != nil {
			return err
		}
		if err := r.setRegisteredTags(ctx, append(tags, missing...)); err != nil {
			if now, rerr := r.registeredTags(ctx); rerr == nil {
				if absent := without(missing, now); len(absent) > 0 {
					r.takeBack(func() (bool, error) { return false, nil },
						func(m *Manifest) { m.RegisteredTags = without(m.RegisteredTags, absent) })
				}
			}
			return err
		}
		r.ask.Info("registered tags: added %s", strings.Join(missing, ", "))
	}
	r.ask.Info("note: clones and restores of a guest keep its tags, so the clone of a published guest asks to be published too")
	return nil
}
