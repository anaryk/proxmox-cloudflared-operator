package setup

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// errKilled is what a fakeHost panics with to stop setup where a kill would:
// right before or right after a command, with nothing of setup run after it.
var errKilled = errors.New("killed")

// fakeHost is a node that keeps what the commands of setup change, as Proxmox
// does: a user that goes takes its tokens and its grants with it, and a role
// that goes takes its grants. It may be killed before or after a command.
type fakeHost struct {
	t           *testing.T
	version     string // of pve-manager; empty: 9.0.10
	roles       map[string][]string
	users       []string
	acl         []pveACL
	tokens      []string // of pco@pve
	tags        []string
	cloudflared bool
	active      map[string]bool
	enabled     map[string]bool
	tables      []string
	secrets     int
	secret      string // of the token pco@pve!pco, while there is one

	ran      []string
	killedAt int              // the command, counted from 1, that the host dies at; 0: none
	before   bool             // the host dies before the command, not after it
	refuse   map[string]error // by command line, or by command name

	userAttrs  map[string]any // more fields of pco@pve in the user list
	tokenAttrs map[string]any // more fields of its token in the token list
}

// newFakeHost is a node an admin has used: a role, a user, its grant and a
// registered tag that are not setup's.
func newFakeHost(t *testing.T) *fakeHost {
	return &fakeHost{
		t:       t,
		roles:   map[string][]string{"Administrator": {"Sys.Audit", "VM.Audit"}, "Auditors": {"VM.Audit"}},
		users:   []string{"root@pam", "audit@pve"},
		acl:     []pveACL{{Path: "/", Type: "user", UGID: "audit@pve", Role: "Auditors"}},
		tags:    []string{"a"},
		active:  map[string]bool{},
		enabled: map[string]bool{},
		refuse:  map[string]error{},
	}
}

// requireUntouched fails unless the host holds what the admin had and
// nothing of setup.
func (h *fakeHost) requireUntouched() {
	t := h.t
	t.Helper()
	require.Equal(t, map[string][]string{"Administrator": {"Sys.Audit", "VM.Audit"}, "Auditors": {"VM.Audit"}}, h.roles)
	require.Equal(t, []string{"root@pam", "audit@pve"}, h.users)
	require.Equal(t, []pveACL{{Path: "/", Type: "user", UGID: "audit@pve", Role: "Auditors"}}, h.acl)
	require.Empty(t, h.tokens)
	require.Equal(t, []string{"a"}, h.tags)
	require.False(t, h.cloudflared, "cloudflared is removed")
	require.False(t, h.active[serviceUnit])
	require.False(t, h.enabled[serviceUnit])
	require.False(t, h.active[egressUnit])
	require.False(t, h.enabled[egressUnit])
}

func (h *fakeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	h.t.Helper()
	line := strings.Join(append([]string{name}, args...), " ")
	h.ran = append(h.ran, line)
	n := len(h.ran)
	if n == h.killedAt && h.before {
		panic(errKilled)
	}
	out, err := h.do(name, args)
	if n == h.killedAt {
		panic(errKilled)
	}
	return out, err
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func flag(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (h *fakeHost) do(name string, args []string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	if err := h.refuse[cmd]; err != nil {
		return "", err
	}
	if err := h.refuse[name]; err != nil {
		return "", err
	}
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	switch {
	case cmd == "pveversion":
		return "pve-manager/" + cmp.Or(h.version, "9.0.10") + "/0123456789abcdef (running kernel: 6.14.8-2-pve)\n", nil
	case cmd == "mountpoint -q /etc/pve":
		return "", nil
	case cmd == "dpkg --print-architecture":
		return "amd64\n", nil

	case name == "pveum" && cmd == "pveum role list --output-format json":
		var roles []map[string]string
		for _, id := range slices.Sorted(maps.Keys(h.roles)) {
			roles = append(roles, map[string]string{"roleid": id, "privs": strings.Join(h.roles[id], ",")})
		}
		return asJSON(roles), nil
	case name == "pveum" && verb == "role" && args[1] == "add":
		if _, ok := h.roles[args[2]]; ok {
			return "", exitErr(255, "role '"+args[2]+"' already exists")
		}
		h.roles[args[2]] = strings.Split(flag(args, "--privs"), ",")
		return "", nil
	case name == "pveum" && verb == "role" && args[1] == "modify":
		h.roles[args[2]] = append(h.roles[args[2]], strings.Split(flag(args, "--privs"), ",")...)
		return "", nil
	case name == "pveum" && verb == "role" && args[1] == "delete":
		if _, ok := h.roles[args[2]]; !ok {
			return "", exitErr(255, "role '"+args[2]+"' does not exist")
		}
		delete(h.roles, args[2])
		h.acl = slices.DeleteFunc(h.acl, func(a pveACL) bool { return a.Role == args[2] })
		return "", nil

	case cmd == "pveum user list --output-format json":
		var users []map[string]any
		for _, u := range h.users {
			user := map[string]any{"userid": u}
			if u == userID {
				maps.Copy(user, h.userAttrs)
			}
			users = append(users, user)
		}
		return asJSON(users), nil
	case name == "pveum" && verb == "user" && args[1] == "add":
		if slices.Contains(h.users, args[2]) {
			return "", exitErr(255, "create user failed: user '"+args[2]+"' already exists")
		}
		h.users = append(h.users, args[2])
		return "", nil
	case name == "pveum" && verb == "user" && args[1] == "delete":
		if !slices.Contains(h.users, args[2]) {
			return "", exitErr(255, "delete user failed: user '"+args[2]+"' does not exist")
		}
		h.users = slices.DeleteFunc(h.users, func(u string) bool { return u == args[2] })
		h.acl = slices.DeleteFunc(h.acl, func(a pveACL) bool { return a.UGID == args[2] })
		if args[2] == userID {
			h.tokens, h.secret = nil, ""
		}
		return "", nil

	case cmd == "pveum acl list --output-format json":
		return asJSON(h.acl), nil
	case name == "pveum" && verb == "acl" && (args[1] == "modify" || args[1] == "delete"):
		grant := pveACL{Path: args[2], Type: "user", UGID: flag(args, "--users"), Role: flag(args, "--roles")}
		if !slices.Contains(h.users, grant.UGID) {
			return "", exitErr(255, "user '"+grant.UGID+"' does not exist")
		}
		h.acl = slices.DeleteFunc(h.acl, func(a pveACL) bool { return sameGrant(a, grant) })
		if args[1] == "modify" {
			h.acl = append(h.acl, grant)
		}
		return "", nil

	case cmd == "pveum user token list pco@pve --output-format json":
		if !slices.Contains(h.users, userID) {
			return "", exitErr(255, "no such user ('pco@pve')")
		}
		var tokens []map[string]any
		for _, tok := range h.tokens {
			token := map[string]any{"tokenid": tok, "privsep": 0}
			maps.Copy(token, h.tokenAttrs)
			tokens = append(tokens, token)
		}
		return asJSON(tokens), nil
	case cmd == "pveum user token add pco@pve pco --privsep 0 --output-format json":
		if slices.Contains(h.tokens, tokenName) {
			return "", exitErr(255, "Token already exists.")
		}
		h.tokens = append(h.tokens, tokenName)
		h.secrets++
		h.secret = "pve-secret-" + strconv.Itoa(h.secrets)
		return asJSON(map[string]any{"full-tokenid": tokenID, "value": h.secret}), nil
	case cmd == "pveum user token remove pco@pve pco":
		if !slices.Contains(h.tokens, tokenName) {
			return "", exitErr(255, "no such token 'pco' for user 'pco@pve'")
		}
		h.tokens, h.secret = nil, ""
		return "", nil

	case cmd == "pvesh get /cluster/options --output-format json":
		return asJSON(map[string]string{"registered-tags": strings.Join(h.tags, ";")}), nil
	case cmd == "pvesh set /cluster/options --delete registered-tags":
		h.tags = nil
		return "", nil
	case name == "pvesh" && flag(args, "--registered-tags") != "":
		h.tags = strings.Split(flag(args, "--registered-tags"), ";")
		return "", nil

	case cmd == cloudflaredBin+" --version":
		if !h.cloudflared {
			return "", notFound(cloudflaredBin)
		}
		return "cloudflared version 2025.9.1 (built 2025-09-22-1234 UTC)\n", nil
	case name == "curl":
		require.NoError(h.t, os.WriteFile(flag(args, "--output"), []byte(gpgKey), 0o600))
		return "", nil
	case cmd == "apt-get update":
		return "", nil
	case cmd == "apt-get install -y cloudflared":
		h.cloudflared = true
		return "", nil
	case cmd == "apt-get remove -y cloudflared":
		h.cloudflared = false
		return "", nil

	case name == "systemctl":
		return h.systemctl(args)

	case cmd == "nft list tables":
		var out strings.Builder
		for _, table := range h.tables {
			fmt.Fprintf(&out, "table %s\n", table)
		}
		return out.String(), nil
	case cmd == "nft delete table inet "+egressTable:
		h.tables = slices.DeleteFunc(h.tables, func(table string) bool { return table == "inet "+egressTable })
		return "", nil
	}
	h.t.Errorf("the fake host does not know %q", cmd)
	return "", errors.New("unknown command")
}

func (h *fakeHost) systemctl(args []string) (string, error) {
	unit := args[len(args)-1]
	switch strings.Join(args[:len(args)-1], " ") {
	case "is-active", "is-active --":
		if h.active[unit] {
			return "active\n", nil
		}
		return "inactive\n", exitErr(3, "")
	case "stop":
		h.active[unit] = false
	case "start":
		h.active[unit] = true
	case "try-restart":
	case "enable --now", "enable --now --":
		h.active[unit], h.enabled[unit] = true, true
	case "enable":
		h.enabled[unit] = true
	case "disable --now", "disable --now --":
		h.active[unit], h.enabled[unit] = false, false
	case "list-units --all --plain --no-legend --":
		var out strings.Builder
		for _, u := range slices.Sorted(maps.Keys(h.active)) {
			if h.active[u] && strings.HasPrefix(u, "pco-cloudflared@") {
				fmt.Fprintf(&out, "%s loaded active running pco cloudflared connector\n", u)
			}
		}
		return out.String(), nil
	default:
		h.t.Errorf("the fake host does not know systemctl %v", args)
		return "", errors.New("unknown command")
	}
	return "", nil
}

func sameGrant(a, b pveACL) bool {
	return a.Path == b.Path && a.Type == b.Type && a.UGID == b.UGID && a.Role == b.Role
}

// onHost makes the env run its commands on h instead of a script, and check
// the Proxmox token against it.
func (e *testEnv) onHost(h *fakeHost) {
	e.s.run = h
	e.s.host.checkToken = h.checkToken
}

// checkToken answers as the API of the host: the secret of the token it has,
// or a 401.
func (h *fakeHost) checkToken(_ context.Context, tok store.PVEToken) error {
	if tok.TokenID != tokenID || h.secret == "" || !tok.Secret.Equal(store.NewSecret(h.secret)) {
		return &pve.APIError{Status: http.StatusUnauthorized, Message: "authentication failure"}
	}
	return nil
}

// killed runs f and reports whether the host killed it.
func killed(t *testing.T, f func() error) bool {
	t.Helper()
	dead := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); !ok || !errors.Is(err, errKilled) {
					panic(r)
				}
				dead = true
			}
		}()
		require.NoError(t, f())
	}()
	return dead
}

// requireNothingLeft fails unless the node holds nothing setup made: the host
// as the admin left it, no apt source, key or temporary file, and no store.
func (e *testEnv) requireNothingLeft(h *fakeHost) {
	e.t.Helper()
	h.requireUntouched()
	for _, dir := range []string{"keyrings", "sources"} {
		entries, err := os.ReadDir(filepath.Join(e.base, dir))
		require.NoError(e.t, err)
		require.Empty(e.t, entries, dir)
	}
	e.requireStoreGone()
}

func TestSetupKilledAtAnyCommandLeavesNothingBehind(t *testing.T) {
	killSweep(t, "")
}

func TestSetupKilledWithAnotherGateTagLeavesNothingBehind(t *testing.T) {
	killSweep(t, "edge")
}

// killSweep kills a setup at every command of it, before and after, runs it
// again and uninstalls, with the gate tag of the settings, if one is given.
func killSweep(t *testing.T, gateTag string) {
	full := Options{Yes: true, CloudflareToken: cfToken, Node: testNode}
	clean := newTestEnv(t)
	clean.installUnit(serviceUnit)
	clean.installUnit(egressUnit)
	if gateTag != "" {
		clean.saveGateTag(gateTag)
	}
	h := newFakeHost(t)
	clean.onHost(h)
	require.NoError(t, clean.setup(full))
	commands := len(h.ran)
	require.Greater(t, commands, 15)

	for k := 1; k <= commands; k++ {
		for _, before := range []bool{true, false} {
			name := sweepName(k, before, h.ran[k-1])
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				e := newTestEnv(t)
				e.installUnit(serviceUnit)
				e.installUnit(egressUnit)
				if gateTag != "" {
					e.saveGateTag(gateTag)
				}
				host := newFakeHost(t)
				host.killedAt, host.before = k, before
				e.onHost(host)

				require.True(t, killed(t, func() error { return e.s.Run(context.Background(), full) }), "the run is killed")
				host.killedAt = 0
				require.NoError(t, e.setup(full), "a run after the kill finishes the setup")
				require.Len(t, e.credentials(), 1)
				require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}))

				e.requireNothingLeft(host)
			})
		}
	}
}

// sweepName names the kill at command k: its number and its first words.
func sweepName(k int, before bool, command string) string {
	when := "after"
	if before {
		when = "before"
	}
	words := strings.Fields(command)
	return fmt.Sprintf("%02d %s %s", k, when, strings.Join(words[:min(len(words), 4)], " "))
}
