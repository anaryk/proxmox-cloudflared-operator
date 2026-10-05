package setup

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	testNode    = "pve1"
	testInstall = "0123456789ab"
	testAccount = "acc1"
	testZone    = "zone1"
	tunnelID    = "00000000-0000-4000-8000-000000000001"

	// The two secrets of a setup. Neither may show in anything setup prints,
	// returns or runs.
	cfToken   = "cf-token-must-not-leak-4f1d2c"
	pveSecret = "pve-secret-must-not-leak-9a8b7c"

	privs9 = "VM.Audit,Sys.Audit,VM.GuestAgent.Audit,SDN.Audit,Pool.Audit"
	privs8 = "VM.Audit,Sys.Audit,VM.Monitor,SDN.Audit,Pool.Audit"

	// What setup gave the role before it read pools.
	earlierPrivs9 = "VM.Audit,Sys.Audit,VM.GuestAgent.Audit,SDN.Audit"
	earlierPrivs8 = "VM.Audit,Sys.Audit,VM.Monitor,SDN.Audit"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// call is one command a scripted host expects, and what it answers. In line,
// the name and the arguments are joined by spaces; "*" stands for any one
// argument.
type call struct {
	line string
	out  string
	err  error
	do   func(t *testing.T, args []string) // what the command does besides answering
}

// fakeRunner is a host that runs nothing: it expects the commands of its
// script in order and fails the test on any other.
type fakeRunner struct {
	t      *testing.T
	script []call
	ran    []string
	events *[]string
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.t.Helper()
	line := strings.Join(append([]string{name}, args...), " ")
	r.ran = append(r.ran, line)
	*r.events = append(*r.events, "run "+line)
	if len(r.script) == 0 {
		r.t.Errorf("unexpected command %q", line)
		return "", errors.New("unexpected command")
	}
	next := r.script[0]
	if !sameCommand(next.line, line) {
		r.t.Errorf("ran %q, want %q", line, next.line)
		return "", errors.New("unexpected command")
	}
	r.script = r.script[1:]
	if next.do != nil {
		next.do(r.t, args)
	}
	return next.out, next.err
}

func sameCommand(want, got string) bool {
	w, g := strings.Fields(want), strings.Fields(got)
	if len(w) != len(g) {
		return false
	}
	for i := range w {
		if w[i] != "*" && w[i] != g[i] {
			return false
		}
	}
	return true
}

// answer is the reply of the operator to a question that contains about.
type answer struct {
	about string
	yes   bool
}

// fakePrompter is an operator who answers from a script and remembers every
// line shown.
type fakePrompter struct {
	t       *testing.T
	answers []answer
	secrets []string
	lines   []string
}

func (p *fakePrompter) Confirm(question string, def bool) (bool, error) {
	p.t.Helper()
	p.lines = append(p.lines, "ask: "+question)
	if len(p.answers) == 0 {
		p.t.Errorf("unexpected question %q", question)
		return def, nil
	}
	a := p.answers[0]
	p.answers = p.answers[1:]
	if !strings.Contains(question, a.about) {
		p.t.Errorf("asked %q, want a question about %q", question, a.about)
	}
	return a.yes, nil
}

func (p *fakePrompter) Secret(question string) (string, error) {
	p.t.Helper()
	p.lines = append(p.lines, "secret: "+question)
	if len(p.secrets) == 0 {
		p.t.Errorf("unexpected question for a secret %q", question)
		return "", nil
	}
	s := p.secrets[0]
	p.secrets = p.secrets[1:]
	return s, nil
}

func (p *fakePrompter) Info(format string, args ...any) {
	p.lines = append(p.lines, "info: "+fmt.Sprintf(format, args...))
}

func (p *fakePrompter) Warn(format string, args ...any) {
	p.lines = append(p.lines, "warn: "+fmt.Sprintf(format, args...))
}

func (p *fakePrompter) text() string { return strings.Join(p.lines, "\n") }

// recordingAPI logs the deletes at Cloudflare among the commands, so that a
// test sees their order.
type recordingAPI struct {
	cfapi.API
	events *[]string
}

func (r recordingAPI) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	*r.events = append(*r.events, "cf DeleteRecord "+zoneID+" "+recordID)
	return r.API.DeleteRecord(ctx, zoneID, recordID)
}

func (r recordingAPI) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	*r.events = append(*r.events, "cf DeleteTunnel "+accountID+" "+tunnelID)
	return r.API.DeleteTunnel(ctx, accountID, tunnelID)
}

// unreadableConfig is a Cloudflare that cannot show the configuration of a
// tunnel.
type unreadableConfig struct{ cfapi.API }

func (unreadableConfig) TunnelConfig(context.Context, string, string) (cfapi.TunnelConfig, error) {
	return cfapi.TunnelConfig{}, errors.New("internal server error")
}

// testEnv is a node with an empty store in temporary directories, a scripted
// host, a scripted operator and a fake Cloudflare that knows cfToken.
type testEnv struct {
	t      *testing.T
	base   string
	paths  store.Paths
	st     *store.Store
	run    *fakeRunner
	ask    *fakePrompter
	cf     *cffake.Fake
	api    func(cfapi.API) cfapi.API // wraps the fake, when set
	events []string
	errs   []string // every error a run returned
	s      *Setup
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	base := t.TempDir()
	e := &testEnv{
		t:    t,
		base: base,
		paths: store.Paths{
			Cluster: filepath.Join(base, "cluster"),
			Private: filepath.Join(base, "private"),
			Local:   filepath.Join(base, "local"),
		},
		cf: cffake.New(),
	}
	e.run = &fakeRunner{t: t, events: &e.events}
	e.ask = &fakePrompter{t: t}
	st, err := store.Open(e.paths)
	require.NoError(t, err)
	e.st = st
	e.cf.SetNow(func() time.Time { return t0 })
	e.cf.AddAccount(testAccount, "Main")
	e.cf.AddZone(testZone, "example.com", testAccount)
	for _, dir := range []string{"keyrings", "sources", "units", "default"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, dir), 0o755))
	}

	newClient := func(token string) (cfapi.API, error) {
		if token != cfToken {
			return nil, errors.New("unknown token")
		}
		var api cfapi.API = recordingAPI{API: e.cf, events: &e.events}
		if e.api != nil {
			api = e.api(api)
		}
		return api, nil
	}
	e.s = New(e.run, e.ask, st, newClient, func() time.Time { return t0 }, mathrand.NewChaCha8([32]byte{1}))
	t.Cleanup(e.verify)
	e.s.host = host{
		pveDir:     "/etc/pve",
		keyring:    filepath.Join(base, "keyrings", "cloudflare-main.gpg"),
		sources:    filepath.Join(base, "sources", "cloudflared.sources"),
		unitDirs:   []string{filepath.Join(base, "units")},
		euid:       func() int { return 0 },
		hostname:   func() (string, error) { return testNode + ".example.com", nil },
		checkToken: func(context.Context, store.PVEToken) error { return nil },

		webDir:       filepath.Join(base, "pco", "web"),
		webEnv:       filepath.Join(base, "default", "pco-web"),
		clusterCA:    filepath.Join(base, "pve", "pve-root-ca.pem"),
		clusterCAKey: filepath.Join(base, "pve", "priv", "pve-root-ca.key"),
		nodeCertDir:  filepath.Join(base, "pve", "local"),
		fqdn:         func(hostname string) string { return hostname },
	}
	return e
}

// script sets what the host expects next, once it has had what it expected
// before.
func (e *testEnv) script(parts ...[]call) {
	e.t.Helper()
	for _, c := range e.run.script {
		e.t.Errorf("the host still expected %q", c.line)
	}
	e.run.script = slices.Concat(parts...)
	e.run.ran = nil
}

// done fails the test when the host still expects a command, or the operator
// still has an answer or a secret to give.
func (e *testEnv) done() {
	e.t.Helper()
	e.verify()
	if e.t.Failed() {
		e.t.FailNow()
	}
}

// verify says what the scripts still held: every test ends with it.
func (e *testEnv) verify() {
	e.t.Helper()
	for _, c := range e.run.script {
		e.t.Errorf("the host still expected %q", c.line)
	}
	for _, a := range e.ask.answers {
		e.t.Errorf("the operator still had an answer about %q", a.about)
	}
	if len(e.ask.secrets) > 0 {
		e.t.Errorf("the operator still had %d secrets to give", len(e.ask.secrets))
	}
	e.run.script, e.ask.answers, e.ask.secrets = nil, nil, nil
}

// reopen opens the store again at e.paths, for a test that changes them.
func (e *testEnv) reopen() {
	e.t.Helper()
	st, err := store.Open(e.paths)
	require.NoError(e.t, err)
	e.st, e.s.st = st, st
}

func (e *testEnv) setup(o Options) error {
	err := e.s.Run(context.Background(), o)
	if err != nil {
		e.errs = append(e.errs, err.Error())
	}
	return err
}

func (e *testEnv) uninstall(o UninstallOptions) error {
	err := e.s.Uninstall(context.Background(), o)
	if err != nil {
		e.errs = append(e.errs, err.Error())
	}
	return err
}

// installUnit puts a unit file where systemd would have it.
func (e *testEnv) installUnit(name string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(filepath.Join(e.base, "units", name), []byte("[Unit]\n"), 0o644))
}

func (e *testEnv) manifest() Manifest {
	e.t.Helper()
	m, found, err := readManifest(filepath.Join(e.paths.Local, manifestName))
	require.NoError(e.t, err)
	require.True(e.t, found, "the manifest is written")
	return m
}

func (e *testEnv) install() store.Install {
	e.t.Helper()
	inst, found, err := e.st.Install()
	require.NoError(e.t, err)
	require.True(e.t, found, "the install is stored")
	return inst
}

func (e *testEnv) writer() planner.Writer {
	e.t.Helper()
	w, found, err := e.st.Writer()
	require.NoError(e.t, err)
	require.True(e.t, found, "the writer is stored")
	return w
}

func (e *testEnv) pveToken() store.PVEToken {
	e.t.Helper()
	tok, found, err := e.st.PVEToken()
	require.NoError(e.t, err)
	require.True(e.t, found, "the Proxmox token is stored")
	return tok
}

func (e *testEnv) credentials() []store.Credential {
	e.t.Helper()
	creds, err := e.st.Credentials()
	require.NoError(e.t, err)
	return creds
}

// requireNoSecret fails when either token shows in anything the fakes saw:
// what was printed and asked, the commands and their arguments, the errors,
// the calls to Cloudflare, the manifest and the events.
func (e *testEnv) requireNoSecret() {
	e.t.Helper()
	seen := slices.Concat(e.ask.lines, e.run.ran, e.errs, e.cf.Calls(), e.events)
	if b, err := os.ReadFile(filepath.Join(e.paths.Local, manifestName)); err == nil {
		seen = append(seen, string(b))
	}
	for _, secret := range []string{cfToken, pveSecret} {
		for _, s := range seen {
			require.NotContains(e.t, s, secret)
		}
	}
}

func (e *testEnv) requireShown(want string) {
	e.t.Helper()
	require.Contains(e.t, e.ask.text(), want)
}

// exitErr is how a command that failed answers.
func exitErr(code int, stderr string) error {
	return fmt.Errorf("exit status %d: %s", code, stderr)
}

func notFound(name string) error { return fmt.Errorf("%s: %w", name, ErrCommandNotFound) }

// The scripts of the steps of a setup.

func preflight(version string) []call {
	return []call{
		{line: "pveversion", out: "pve-manager/" + version + "/0123456789abcdef (running kernel: 6.14.8-2-pve)\n"},
		{line: "mountpoint -q /etc/pve"},
		{line: "dpkg --print-architecture", out: "amd64\n"},
	}
}

// preflightNew is the preflight of a setup that creates the install, which
// looks for the connectors of another install before it creates anything.
func preflightNew(version string) []call {
	return slices.Concat(preflight(version), noConnectorsSeen())
}

func roleCreated(privs string) []call {
	return []call{
		{line: "pveum role list --output-format json", out: `[{"roleid":"Administrator","privs":"Sys.Audit,VM.Audit","special":1}]`},
		{line: "pveum role add PCO --privs " + privs},
	}
}

func roleWith(privs string) call {
	return call{
		line: "pveum role list --output-format json",
		out:  `[{"roleid":"Administrator","privs":"Sys.Audit","special":1},{"roleid":"PCO","privs":"` + privs + `","special":0}]`,
	}
}

func roleKept() []call { return []call{roleWith(privs9)} }

const (
	usersWithout = `[{"userid":"root@pam","enable":1}]`
	usersWith    = `[{"userid":"root@pam","enable":1},{"userid":"pco@pve","enable":1,"comment":"pco operator"}]`
	aclWith      = `[{"path":"/","type":"user","ugid":"pco@pve","roleid":"PCO","propagate":1}]`
	tokensWith   = `[{"tokenid":"pco","privsep":0,"expire":0}]`
)

func userCreated() []call {
	return []call{
		{line: "pveum user list --output-format json", out: usersWithout},
		{line: "pveum user add pco@pve --comment pco operator"},
		{line: "pveum acl list --output-format json", out: `[]`},
		{line: "pveum acl modify / --users pco@pve --roles PCO"},
	}
}

func userKept() []call {
	return []call{
		{line: "pveum user list --output-format json", out: usersWith},
		{line: "pveum acl list --output-format json", out: aclWith},
	}
}

func tokenAdd(secret string) call {
	return call{
		line: "pveum user token add pco@pve pco --privsep 0 --output-format json",
		out:  `{"full-tokenid":"pco@pve!pco","info":{"privsep":"0"},"value":"` + secret + `"}`,
	}
}

func tokenCreated() []call {
	return []call{
		{line: "pveum user token list pco@pve --output-format json", out: `[]`},
		tokenAdd(pveSecret),
	}
}

func tokenKept() []call {
	return []call{{line: "pveum user token list pco@pve --output-format json", out: tokensWith}}
}

func tagsAdded(existing, set string) []call {
	return []call{
		{line: "pvesh get /cluster/options --output-format json", out: `{"keyboard":"en-us","registered-tags":"` + existing + `"}`},
		{line: "pvesh set /cluster/options --registered-tags " + set},
	}
}

// tagsRead is a look at the registered tags that changes none.
func tagsRead(existing string) []call {
	return []call{{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"` + existing + `"}`}}
}

func tagsKept() []call {
	return []call{{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"cf-tunnel;cf-tunnel-managed"}`}}
}

const gpgKey = "not a real key\n"

func cloudflaredInstalled() []call {
	return []call{
		{line: "/usr/bin/cloudflared --version", err: notFound("/usr/bin/cloudflared")},
		{
			line: "curl --disable --fail --silent --show-error --location --proto =https --tlsv1.2 --max-time 60 " +
				"--max-filesize 1048576 --output * https://pkg.cloudflare.com/cloudflare-main.gpg",
			do: func(t *testing.T, args []string) {
				out := args[slices.Index(args, "--output")+1]
				require.NoError(t, os.WriteFile(out, []byte(gpgKey), 0o600))
			},
		},
		{line: "apt-get update"},
		{line: "apt-get install -y cloudflared"},
	}
}

func cloudflaredKept() []call {
	return []call{{line: "/usr/bin/cloudflared --version", out: "cloudflared version 2025.9.1 (built 2025-09-22-1234 UTC)\n"}}
}

func daemonIs(state string) []call {
	c := call{line: "systemctl is-active pco.service", out: state + "\n"}
	if state != "active" {
		c.err = exitErr(3, "")
	}
	return []call{c}
}

func serviceStarted() []call {
	return []call{{line: "systemctl enable --now pco.service"}}
}

// serviceRestarted starts the daemon after a new Proxmox token was made while
// it may have run with the old one.
func serviceRestarted() []call {
	return []call{{line: "systemctl try-restart pco.service"}, {line: "systemctl enable --now pco.service"}}
}
