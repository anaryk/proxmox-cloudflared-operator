package applianceinstall

import (
	"crypto/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
)

const joinInstall = "0123456789ab"

// joinToken is a token of install joinInstall for node, made now.
func joinToken(t *testing.T, node string) pki.JoinToken {
	t.Helper()
	ca, err := pki.NewCA(joinInstall, t0, rand.Reader)
	require.NoError(t, err)
	tok, err := pki.NewJoinToken(joinInstall, node, []string{"192.0.2.200:8644"}, ca, t0, rand.Reader)
	require.NoError(t, err)
	return tok
}

// withController puts the controller lxc/200 on pve2, in pool pco.
func (e *testEnv) withController() {
	e.node.elsewhere[200] = "pve2"
	e.node.otherCTs[200] = &fakeCT{cfg: map[string]string{"tags": ctTags(KindController)}}
	e.node.pools["pco"] = &fakePool{Comment: poolComment, Members: []int{200}}
	e.node.guestFirewalls[200] = &fakeGuestFirewall{ipsets: []string{agentsIPSet}}
	e.node.addToken("pco@pve", "vm200", marker(200), true)
}

func (e *testEnv) agentOptions(tok pki.JoinToken) Options {
	o := e.options()
	o.Kind, o.JoinToken, o.Storage = KindAgent, tok.Encode(), "local-zfs"
	return o
}

func TestAnAgentIsInstalledWithItsTokenOnItsVolumeAndNowhereElse(t *testing.T) {
	e := newEnv(t)
	e.withController()
	tok := joinToken(t, testNode)

	e.install(e.agentOptions(tok))

	ct := e.node.cts[100]
	require.Equal(t, "pco-appliance;pco-agent", ct.cfg["tags"])
	require.Contains(t, e.node.pushes, "100 /var/lib/pco/join.token 0600")
	require.Equal(t, tok.Encode()+"\n", string(ct.files["/var/lib/pco/join.token"].data))
	for _, line := range e.node.ran {
		require.NotContains(t, line, tok.Encode(), "the token is in no argument")
		require.NotContains(t, line, strings.TrimPrefix(tok.Encode(), pki.JoinPrefix)[:24])
	}
	require.NotContains(t, e.ask.text(), tok.Encode())
	b := e.bootstrap(0)
	require.Equal(t, "agent", b["kind"])
	require.Equal(t, joinInstall, b["installId"])
	require.Empty(t, b["cloudflareToken"])
	require.True(t, e.node.has("/vms/100", "user", "pco@pve", "PCOManaged"))
	require.True(t, e.node.has("/vms/100", "token", "pco@pve!vm200", "PCOManaged"), "the controller's token puts the legs on the agent")
	require.True(t, e.node.has("/", "token", "pco@pve!vm100", "PCO"), "the agent's token audits, nothing more")
	require.False(t, e.node.has("/", "token", "pco@pve!vm100", "PCOManaged"))
	require.Equal(t, []map[string]string{{"cidr": "192.0.2.150", "comment": "pco appliance vm100"}}, e.node.guestFirewalls[200].entries[agentsIPSet])
	require.False(t, ct.web, "an agent has no web interface")
	require.Equal(t, []string{"admin-only"}, e.node.tags, "the gate tags are the controller's")
	require.Contains(t, e.ask.text(), "pco node admit pve1 --fingerprint")
	require.Empty(t, entries(t, e.runDir), "the token is gone from /run")
}

func TestAnAgentThatCannotServeThisNodeIsRefusedBeforeAnythingIsMade(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(e *testEnv, o *Options)
		want  string
	}{
		{"a token of another node", func(e *testEnv, o *Options) { o.JoinToken = joinToken(t, "pve3").Encode() },
			"the join token is for node pve3, and this is node pve1"},
		{"no token", func(e *testEnv, o *Options) { o.JoinToken = "" }, "an agent joins its controller with a join token"},
		{"a combined appliance in the cluster", func(e *testEnv, _ *Options) {
			e.node.otherCTs[200].cfg["tags"] = ctTags(KindCombined)
		}, "the combined appliance lxc/200 serves this cluster: pco appliance convert --vmid 200"},
		{"an agent on this node", func(e *testEnv, _ *Options) {
			e.node.cts[150] = &fakeCT{cfg: map[string]string{"tags": ctTags(KindAgent)}, files: map[string]fakeFile{}}
			e.node.pools["pco"].Members = append(e.node.pools["pco"].Members, 150)
		}, "lxc/150, the agent on this node, serves it already"},
		{"on HA", func(_ *testEnv, o *Options) { o.HA = true }, "--ha is for a controller"},
		{"with a Cloudflare token", func(_ *testEnv, o *Options) { o.CloudflareToken = cfToken }, "an agent holds no Cloudflare token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.withController()
			o := e.agentOptions(joinToken(t, testNode))
			tt.setup(e, &o)

			err := e.in.Install(t.Context(), o)

			require.ErrorContains(t, err, tt.want)
			require.Zero(t, e.node.count("pct create"))
			require.Zero(t, e.node.count("pveum user token add"))
		})
	}
}

func TestAControllerTakesTheAgentsAloneOnItsEndpoint(t *testing.T) {
	e := newEnv(t)
	e.node.sharedStorage = true
	o := e.options()
	o.Kind, o.Storage = KindController, "local-zfs"

	e.install(o)

	ct := e.node.cts[100]
	require.Equal(t, "pco-appliance;pco-controller", ct.cfg["tags"])
	fw := e.node.guestFirewalls[100]
	require.Equal(t, []string{agentsIPSet}, fw.ipsets)
	require.Equal(t, []fakeRule{{comment: agentsComment, source: "+pco-agents", dport: "8644"}}, fw.rules)
	require.True(t, e.node.has("/storage/local-zfs", "token", "pco@pve!vm100", roleDatastoreAudit))
	b := e.bootstrap(0)
	require.Equal(t, "controller", b["kind"])
	require.Equal(t, map[string]any{"id": "local-zfs", "shared": true}, b["storage"])
	require.True(t, ct.web, "the controller has the web interface")
	require.Empty(t, e.node.ha)
}

func TestAControllerOnHAIsKeptOnTheNodesChosen(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.Kind, o.Storage, o.HA, o.HANodes = KindController, "local-zfs", true, []string{"pve1", "pve2"}

	e.install(o)

	require.Equal(t, []string{"add ct:100 --state started",
		"rules add node-affinity pco-100 --resources ct:100 --nodes pve1,pve2 --comment pco appliance vm100"}, e.node.ha)
	require.Contains(t, e.ask.text(), "fences itself when it loses quorum")
	require.Equal(t, map[string]any{"id": "local-zfs", "ha": true}, e.bootstrap(0)["storage"])
}

func TestAControllerBesideACombinedApplianceIsRefused(t *testing.T) {
	e := newEnv(t)
	e.withController()
	e.node.otherCTs[200].cfg["tags"] = ctTags(KindCombined)
	o := e.options()
	o.Kind, o.Storage = KindController, "local-zfs"

	require.ErrorContains(t, e.in.Install(t.Context(), o), "pco appliance convert --vmid 200")
	require.Zero(t, e.node.count("pct create"))
}

func TestUninstallingAnAgentTakesOnlyItsOwnAddressOutOfTheControllersIPSet(t *testing.T) {
	e := newEnv(t)
	e.withController()
	e.install(e.agentOptions(joinToken(t, testNode)))
	fw := e.node.guestFirewalls[200]
	fw.entries[agentsIPSet] = append(fw.entries[agentsIPSet], map[string]string{"cidr": "192.0.2.77", "comment": "pco appliance vm300"})

	require.NoError(t, e.in.Uninstall(t.Context(), 100, UninstallOptions{Yes: true, KeepCloudflare: true, KeepTemplate: true}), e.ask.text())

	require.Equal(t, []map[string]string{{"cidr": "192.0.2.77", "comment": "pco appliance vm300"}}, fw.entries[agentsIPSet])
	require.Contains(t, e.ask.text(), "pco node forget pve1")
}

// The uninstall of an HA controller removes the rule the install made; a
// rule of that name without pco's mark is the admin's and stays.
func TestUninstallingAnHAControllerRemovesItsRule(t *testing.T) {
	for _, tt := range []struct {
		name    string
		comment string
		left    int
	}{
		{"pco's", "pco appliance vm100", 0},
		{"the admin's", "the admin's", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			o := e.options()
			o.Kind, o.Storage, o.HA, o.HANodes = KindController, "local-zfs", true, []string{"pve1", "pve2"}
			e.install(o)
			e.node.haRules[0]["comment"] = tt.comment

			require.NoError(t, e.in.Uninstall(t.Context(), 100, UninstallOptions{Yes: true, KeepCloudflare: true, KeepTemplate: true}), e.ask.text())

			require.Len(t, e.node.haRules, tt.left)
		})
	}
}
