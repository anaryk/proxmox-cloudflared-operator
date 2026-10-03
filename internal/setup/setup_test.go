package setup

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

func ptr[T any](v T) *T { return &v }

// freshInstall is the whole of a first setup on Proxmox VE 9 with --yes and a
// Cloudflare token.
func freshInstall() [][]call { return freshInstallAfter(preflightNew("9.0.10")) }

// freshInstallAfter is a first setup that begins with the given preflight.
func freshInstallAfter(first []call) [][]call {
	return [][]call{
		first, roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), cloudflaredInstalled(), daemonIs("inactive"), serviceStarted(),
	}
}

func TestFreshInstallOnPVE9(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(freshInstall()...)

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	inst := e.install()
	require.Regexp(t, `^[0-9a-f]{12}$`, inst.ID)
	require.Equal(t, store.ProfileHost, inst.Profile)
	require.Equal(t, t0, inst.CreatedAt)

	w := e.writer()
	require.Equal(t, inst.ID, w.InstallID)
	require.Equal(t, 1, w.Generation)
	require.Regexp(t, `^[a-z0-9]{8}$`, w.Nonce)

	settings, err := e.st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly, "a fresh install only observes")

	tok := e.pveToken()
	require.Equal(t, "pco@pve!pco", tok.TokenID)
	require.Equal(t, pveSecret, tok.Secret.Reveal())

	creds := e.credentials()
	require.Len(t, creds, 1)
	require.Equal(t, "scoped", creds[0].Kind)
	require.Equal(t, credentialLabel, creds[0].Label)
	require.Equal(t, cfToken, creds[0].Token.Reveal())
	require.Equal(t, t0, creds[0].AddedAt)
	require.Regexp(t, `^[0-9a-f]{8}$`, creds[0].ID)

	nodes, err := e.st.Nodes()
	require.NoError(t, err)
	require.Equal(t, []store.NodeEntry{{Name: testNode, Version: version.Version, Since: t0}}, nodes)

	require.Equal(t, Manifest{
		Node:                 testNode,
		InstalledAt:          t0,
		CreatedRole:          true,
		CreatedUser:          true,
		CreatedToken:         true,
		GrantedACL:           true,
		RegisteredTags:       []string{"cf-tunnel", "cf-tunnel-managed"},
		InstalledCloudflared: true,
		AddedAptSource:       true,
		AddedKeyring:         true,
	}, e.manifest())

	key, err := os.ReadFile(e.s.host.keyring)
	require.NoError(t, err)
	require.Equal(t, gpgKey, string(key))
	sources, err := os.ReadFile(e.s.host.sources)
	require.NoError(t, err)
	require.Equal(t, "Types: deb\n"+
		"URIs: https://pkg.cloudflare.com/cloudflared\n"+
		"Suites: any\n"+
		"Components: main\n"+
		"Signed-By: "+e.s.host.keyring+"\n", string(sources))
	for _, path := range []string{e.s.host.keyring, e.s.host.sources} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), path)
	}
	leftovers, err := filepath.Glob(filepath.Join(e.base, "keyrings", ".*"))
	require.NoError(t, err)
	require.Empty(t, leftovers, "no temporary file is left next to the key")

	e.requireShown("clones and restores")
	e.requireShown("pco plan")
	e.requireNoSecret()
}

func TestSetupDefaultsTheNodeToTheHostName(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(freshInstall()...)

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken}))

	require.Equal(t, testNode, e.manifest().Node)
}

func TestSetupOnPVE84GrantsVMMonitor(t *testing.T) {
	e := newTestEnv(t)
	e.script(preflightNew("8.4.1"), roleCreated(privs8),
		[]call{{line: "pveum user list --output-format json", err: exitErr(255, "ipcc_send_rec failed")}})

	err := e.setup(Options{Yes: true, Node: testNode})

	// A step that fails stops the run and is named.
	require.ErrorContains(t, err, "step user")
	require.ErrorContains(t, err, "ipcc_send_rec failed")
	e.done()
	require.True(t, e.manifest().CreatedRole, "what was created before the failure is in the manifest")
}

func TestSetupRefusesUnsupportedVersions(t *testing.T) {
	for _, tt := range []struct{ name, version string }{
		{"8.2", "8.2.4"},
		{"7.x", "7.4-17"},
		{"a later major", "10.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.script(preflight(tt.version)[:1])

			err := e.setup(Options{Yes: true, Node: testNode})

			require.ErrorContains(t, err, "is not supported")
			require.ErrorContains(t, err, "step preflight")
			e.done()
			_, found, err := e.st.Install()
			require.ErrorIs(t, err, store.ErrNoRoot, "nothing is set up: %v", found)
		})
	}
}

func TestSetupRefusesWithoutRoot(t *testing.T) {
	e := newTestEnv(t)
	e.s.host.euid = func() int { return 1000 }

	require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "root")
	e.done()
}

func TestSetupRefusesAnUnsupportedArchitecture(t *testing.T) {
	e := newTestEnv(t)
	script := preflight("9.0.10")
	script[2].out = "i386\n"
	e.script(script)

	require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "i386")
}

func TestSetupTwiceConverges(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(freshInstall()...)
	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	inst, w, tok, m := e.install(), e.writer(), e.pveToken(), e.manifest()
	creds := e.credentials()

	// The second run finds everything in place and creates nothing: the
	// script has no command that would.
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(),
		tagsKept(), cloudflaredKept(), serviceStarted())
	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	require.Equal(t, inst, e.install())
	require.Equal(t, w, e.writer())
	require.True(t, tok.Secret.Equal(e.pveToken().Secret), "the Proxmox token is kept")
	require.Equal(t, m, e.manifest())
	again := e.credentials()
	require.Len(t, again, 1)
	require.Equal(t, creds[0].ID, again[0].ID)
	e.requireNoSecret()
}

func TestSetupContinuesAHalfFinishedRun(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(preflightNew("9.0.10"), roleCreated(privs9),
		[]call{{line: "pveum user list --output-format json", err: exitErr(255, "connection refused")}})
	require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "step user")
	inst := e.install()

	// The role is there and the user is not: the second run goes on from there.
	e.script(preflight("9.0.10"), roleKept(), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), cloudflaredKept(), serviceRestarted())
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.Equal(t, inst, e.install(), "the install of the first run is kept")
	m := e.manifest()
	require.True(t, m.CreatedRole, "the role the first run created is still setup's")
	require.True(t, m.CreatedUser)
	require.True(t, m.CreatedToken)
	require.False(t, m.InstalledCloudflared)
}

func TestSetupMergesTagsAndRole(t *testing.T) {
	for _, tt := range []struct {
		name          string
		existing, set string
		added         []string
	}{
		{"both added", "a;b", "a;b;cf-tunnel;cf-tunnel-managed", []string{"cf-tunnel", "cf-tunnel-managed"}},
		{"one there", "a;cf-tunnel", "a;cf-tunnel;cf-tunnel-managed", []string{"cf-tunnel-managed"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit("pco.service")
			e.script(preflightNew("9.0.10"),
				[]call{
					roleWith("VM.Audit,Sys.Audit,Datastore.Audit"),
					{line: "pveum role modify PCO --append 1 --privs VM.GuestAgent.Audit,SDN.Audit"},
				},
				userKept(), tokenCreated(), tagsAdded(tt.existing, tt.set), cloudflaredKept(), serviceRestarted())

			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			e.done()

			// The difference is printed; what the admin added is kept.
			e.requireShown("info: role PCO: added VM.GuestAgent.Audit, SDN.Audit")
			e.requireShown("Datastore.Audit")
			m := e.manifest()
			require.False(t, m.CreatedRole, "a role that was there is not setup's")
			require.False(t, m.CreatedUser)
			require.Equal(t, tt.added, m.RegisteredTags, "only the tags setup added are setup's")
		})
	}
}

func TestSetupRecreatesATokenWhoseSecretIsLost(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(preflightNew("9.0.10"), roleKept(), userKept(),
		[]call{
			{line: "pveum user token list pco@pve --output-format json", out: tokensWith},
			{line: "pveum user token remove pco@pve pco"},
			tokenAdd(pveSecret),
		},
		tagsKept(), cloudflaredKept(), serviceRestarted())

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.Equal(t, pveSecret, e.pveToken().Secret.Reveal())
	e.requireShown("cannot be read back")
	require.True(t, e.manifest().CreatedToken)
	e.requireNoSecret()
}

func TestSetupRestartsARunningDaemonForANewToken(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(preflightNew("9.0.10"), roleKept(), userKept(),
		[]call{
			{line: "pveum user token list pco@pve --output-format json", out: tokensWith},
			{line: "pveum user token remove pco@pve pco"},
			tokenAdd(pveSecret),
		},
		tagsKept(), cloudflaredKept(), daemonIs("active"), serviceRestarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()
	require.Empty(t, e.credentials(), "a running daemon owns the credentials")
}

func TestSetupDeclinesTagsAndCloudflared(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options Options
		answers []answer
		secrets []string
	}{
		{
			name:    "asked",
			options: Options{Node: testNode},
			answers: []answer{{"cf-tunnel", false}, {"cloudflared", false}},
			secrets: []string{""},
		},
		{
			name:    "flags",
			options: Options{Yes: true, Node: testNode, RegisterTags: ptr(false), InstallCloudflared: ptr(false)},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit("pco.service")
			e.ask.answers, e.ask.secrets = tt.answers, tt.secrets
			script := [][]call{
				preflightNew("9.0.10"), roleCreated(privs9), userCreated(), tokenCreated(), tagsRead(""),
				{{line: "/usr/bin/cloudflared --version", err: notFound("/usr/bin/cloudflared")}},
			}
			if tt.options.Yes {
				// Nobody asked systemd whether the daemon runs with the old token.
				script = append(script, serviceRestarted())
			} else {
				// The token is asked for only while the daemon cannot take it.
				script = append(script, daemonIs("inactive"), serviceStarted())
			}
			e.script(script...)

			require.NoError(t, e.setup(tt.options))
			e.done()
			require.Empty(t, e.ask.answers, "every question was asked")

			m := e.manifest()
			require.Empty(t, m.RegisteredTags)
			require.False(t, m.InstalledCloudflared)
			require.False(t, m.AddedAptSource)
			require.NoFileExists(t, e.s.host.sources)
			require.Empty(t, e.credentials())
			e.requireShown("pco credential add")
			e.requireShown("the gate tag cf-tunnel is not registered")
		})
	}
}

func TestSetupAsksForTheToken(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.ask.answers = []answer{{"cf-tunnel", true}, {"cloudflared", true}}
	e.ask.secrets = []string{cfToken}
	e.script(freshInstall()...)

	require.NoError(t, e.setup(Options{Node: testNode}))
	e.done()

	require.Len(t, e.credentials(), 1)
	e.requireNoSecret()
}

func TestSetupDoesNotStoreAnUnusableToken(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.cf.Deny("dns.write")
	e.script(freshInstall()...)

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	require.Empty(t, e.credentials())
	e.requireShown("dns.write on example.com")
	e.requireShown("not stored")
	e.requireNoSecret()
}

func TestSetupSkipsTheTokenWhileTheDaemonRuns(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(preflightNew("9.0.10"), roleKept(), userKept(), tokenCreated(),
		tagsKept(), cloudflaredKept(), daemonIs("active"), serviceRestarted())

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	e.requireShown(`pco is running: add credentials with "pco credential add"`)
	require.Empty(t, e.credentials())
	require.Empty(t, e.cf.Calls(), "the token is not even checked")
}

func TestSetupWithoutTheUnitFile(t *testing.T) {
	e := newTestEnv(t)
	e.script(preflightNew("9.0.10"), roleKept(), userKept(), tokenCreated(),
		tagsKept(), cloudflaredKept())

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	e.requireShown("pco.service is not installed")
}

func TestSetupRefusesWhenAnotherNodeIsRegistered(t *testing.T) {
	e := newTestEnv(t)
	require.NoError(t, e.st.Init())
	require.NoError(t, e.st.SaveNode(store.NodeEntry{Name: "pve2", Since: t0}))
	e.script(preflightNew("9.0.10"))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.EqualError(t, err, "setup step store: pco is already set up on node pve2; cluster support arrives in a later release")
	e.done()
}

func TestSetupRefusesOptionsThatDoNotGoTogether(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options Options
		want    string
	}{
		{"repair and recover", Options{Repair: true, Recover: true}, "--repair"},
		{"repair with a token", Options{Repair: true, CloudflareToken: cfToken}, "pco credential add"},
		{"install id without recover", Options{InstallID: testInstall}, "--install-id"},
		{"install id that is none", Options{Recover: true, InstallID: "ABC"}, "install id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			require.ErrorContains(t, e.setup(tt.options), tt.want)
			e.done()
		})
	}
}

// sentinel is the configuration of a tunnel written by the writer of an
// install at a generation.
func sentinel(install string, generation int) []planner.IngressRule {
	return []planner.IngressRule{
		{Hostname: "www.example.com", Service: "http://10.0.0.5:80"},
		planner.SentinelRule(planner.Writer{InstallID: install, Generation: generation, Nonce: "abcd1234"}),
		planner.CatchAllRule(),
	}
}

// stoppedForRecovery stops the daemon that ran before a recovery.
func stoppedForRecovery() []call {
	return append(daemonIs("active"), call{line: "systemctl stop pco.service"})
}

// startedAgain starts the daemon a failed recovery stopped.
func startedAgain() []call { return []call{{line: "systemctl start pco.service"}} }

// recovered is the rest of a setup that recovers an install. The daemon was
// stopped first, so the token is stored without asking systemd again.
func recovered() [][]call {
	return [][]call{
		roleCreated(privs9), userCreated(), tokenCreated(),
		tagsAdded("", "cf-tunnel;cf-tunnel-managed"), cloudflaredKept(), serviceStarted(),
	}
}

func TestRecoverAdoptsTheOneInstallFound(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 7))
	// Neither a probe tunnel nor a tunnel of another name is an install.
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall+"_probe_x1y2", nil)
	e.cf.SeedTunnel(testAccount, "pco-not-an-install", nil)
	e.cf.SeedTunnel(testAccount, "office", nil)
	e.script(append([][]call{preflight("9.0.10"), stoppedForRecovery()}, recovered()...)...)

	require.NoError(t, e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	inst := e.install()
	require.Equal(t, testInstall, inst.ID)
	require.Equal(t, store.ProfileHost, inst.Profile)
	w := e.writer()
	require.Equal(t, testInstall, w.InstallID)
	require.Equal(t, 8, w.Generation, "the generation follows the highest sentinel")
	require.NotEqual(t, "abcd1234", w.Nonce)
	settings, err := e.st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly)
	require.Len(t, e.credentials(), 1, "the token of the recovery is stored as well")
	e.requireNoSecret()
}

func TestRecoverWithTwoInstallsNeedsTheID(t *testing.T) {
	const other = "ba9876543210"
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 3))
	e.cf.SeedTunnel(testAccount, "pco-"+other, sentinel(other, 12))
	e.script(preflight("9.0.10"), stoppedForRecovery(), startedAgain())

	err := e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode})

	require.ErrorContains(t, err, "--install-id")
	require.ErrorContains(t, err, testInstall)
	require.ErrorContains(t, err, other)
	e.done()
	_, found, err := e.st.Install()
	require.NoError(t, err)
	require.False(t, found)

	e.script(append([][]call{preflight("9.0.10"), stoppedForRecovery()}, recovered()...)...)
	require.NoError(t, e.setup(Options{Yes: true, Recover: true, InstallID: other, CloudflareToken: cfToken, Node: testNode}))
	e.done()
	require.Equal(t, other, e.install().ID)
	require.Equal(t, 13, e.writer().Generation)
}

func TestRecoverRefusesAnUnreadableConfiguration(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.api = func(api cfapi.API) cfapi.API { return unreadableConfig{api} }
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 7))
	e.script(preflight("9.0.10"), daemonIs("inactive"))

	err := e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode})

	require.ErrorContains(t, err, "configuration")
	e.done()
	e.requireShown("pco.service was not running before --recover and stays stopped")
	_, found, err := e.st.Writer()
	require.NoError(t, err)
	require.False(t, found, "no generation is guessed")
}

func TestRecoverNeedsAToken(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(preflight("9.0.10"), stoppedForRecovery(), startedAgain())

	require.ErrorContains(t, e.setup(Options{Yes: true, Recover: true, Node: testNode}), "Cloudflare token")
	e.done()
	e.requireShown("pco.service was running before --recover and is started again")
}

func TestRecoverAsksForTheToken(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 2))
	e.ask.answers = []answer{{"cf-tunnel", true}}
	e.ask.secrets = []string{cfToken}
	e.script(append([][]call{preflight("9.0.10"), stoppedForRecovery()}, recovered()...)...)

	require.NoError(t, e.setup(Options{Recover: true, Node: testNode}))
	e.done()

	require.Equal(t, 3, e.writer().Generation)
	require.Len(t, e.credentials(), 1, "the token is asked for once")
}

func TestRepairTouchesOnlyProxmoxAndTheService(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(freshInstall()...)
	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	inst, w, m := e.install(), e.writer(), e.manifest()
	calls := len(e.cf.Calls())
	// A restore of the node took the token file with it.
	require.NoError(t, os.Remove(filepath.Join(e.paths.Private, "meta", "pve-token.json")))

	e.script(preflight("9.0.10"), roleKept(), userKept(),
		[]call{
			{line: "pveum user token list pco@pve --output-format json", out: tokensWith},
			{line: "pveum user token remove pco@pve pco"},
			tokenAdd("pve-secret-after-the-repair"),
		},
		tagsKept(), serviceRestarted())
	require.NoError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}))
	e.done()

	require.Equal(t, inst, e.install())
	require.Equal(t, w, e.writer())
	require.Equal(t, m, e.manifest())
	require.Equal(t, "pve-secret-after-the-repair", e.pveToken().Secret.Reveal())
	require.Len(t, e.cf.Calls(), calls, "a repair does not go to Cloudflare")
}

func TestRepairNeedsASetUpNode(t *testing.T) {
	e := newTestEnv(t)
	e.script(preflight("9.0.10"))

	require.ErrorContains(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}), "pco setup")
	e.done()
}

func TestNoTokenIsShown(t *testing.T) {
	t.Run("a token answer that is not JSON", func(t *testing.T) {
		e := newTestEnv(t)
		e.script(preflightNew("9.0.10"), roleKept(), userKept(),
			[]call{
				{line: "pveum user token list pco@pve --output-format json", out: `[]`},
				{
					line: "pveum user token add pco@pve pco --privsep 0 --output-format json",
					out:  `{"full-tokenid":"pco@pve!pco","value":"` + pveSecret,
				},
			})

		require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "step token")
		e.requireNoSecret()
	})
	t.Run("a token answer without a value", func(t *testing.T) {
		e := newTestEnv(t)
		e.script(preflightNew("9.0.10"), roleKept(), userKept(),
			[]call{
				{line: "pveum user token list pco@pve --output-format json", out: `[]`},
				{
					line: "pveum user token add pco@pve pco --privsep 0 --output-format json",
					out:  `{"full-tokenid":"pco@pve!pco","secret":"` + pveSecret + `"}`,
				},
			})

		require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "step token")
		e.requireNoSecret()
	})
	t.Run("a token Cloudflare refuses", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit("pco.service")
		e.cf.SetTokenStatus("disabled", nil)
		e.script(freshInstall()...)

		require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
		e.requireNoSecret()
	})
	t.Run("a token setup cannot use", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit("pco.service")
		e.script(freshInstall()...)

		require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: "another " + cfToken, Node: testNode}))
		e.requireNoSecret()
	})
}

func TestInstallIDsAndNonces(t *testing.T) {
	e := newTestEnv(t)
	for range 50 {
		id, err := e.s.newInstallID()
		require.NoError(t, err)
		require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{12}$`), id)
		nonce, err := e.s.newNonce()
		require.NoError(t, err)
		require.Regexp(t, regexp.MustCompile(`^[a-z0-9]{8}$`), nonce)
		require.NoError(t, planner.Writer{InstallID: id, Generation: 1, Nonce: nonce}.Validate())
	}
}
