package doctor

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeCluster answers the checks of a cluster from its fields.
type fakeCluster struct {
	agent    agentproto.AgentView
	agentErr error
	members  members.Members
	memErr   error
	corosync members.Corosync
	csErr    error
	ca       time.Time
	caErr    error
	facts    engine.ClusterFacts
	answers  map[string]AgentAnswer
	asked    []string
	waited   time.Duration
}

func (f *fakeCluster) Agent() (agentproto.AgentView, error) { return f.agent, f.agentErr }
func (f *fakeCluster) Members() (members.Members, error)    { return f.members, f.memErr }
func (f *fakeCluster) Corosync() (members.Corosync, error)  { return f.corosync, f.csErr }
func (f *fakeCluster) CAExpires() (time.Time, error)        { return f.ca, f.caErr }
func (f *fakeCluster) Facts() engine.ClusterFacts           { return f.facts }
func (f *fakeCluster) ClusterSource() ClusterSource         { return f }
func (f *fakeCluster) Ask(_ context.Context, nodes []string, wait time.Duration) map[string]AgentAnswer {
	f.asked, f.waited = nodes, wait
	return f.answers
}

// clusterEnv is a node of a cluster: the host as fakeEnv has it, and the
// cluster as fakeCluster does.
type clusterEnv struct {
	*fakeEnv
	*fakeCluster
}

const sharedID = "0c6b4a2e-shared"

// healthyCluster is pve1, the controller, with pve2 and pve3, all well.
func healthyCluster() (engine.State, *clusterEnv) {
	st := healthyState()
	st.Node = "pve1"
	st.Tunnels[0].ID = sharedID
	st.Connectors[0].TunnelID = sharedID
	st.Lease = &engine.LeaseView{Holder: "pve1", Address: netip.MustParseAddr("10.0.0.1"), Generation: 4, RefreshAge: 3 * time.Second, Valid: true}
	node := func(name, addr string, ctrl bool) engine.NodeView {
		return engine.NodeView{
			Name: name, State: store.NodeActive, Kind: "host", AgentAddress: netip.MustParseAddr(addr), Controller: ctrl,
			Build: "0.3.0", Protocol: "1-1", StatusAge: 4 * time.Second, WatchRunning: true, Covered: true, ReplicaReady: true,
			CertExpires: now.Add(80 * 24 * time.Hour), ClockSkew: 200 * time.Millisecond,
		}
	}
	st.Nodes = []engine.NodeView{node("pve1", "10.0.0.1", true), node("pve2", "10.0.0.2", false), node("pve3", "10.0.0.3", false)}
	st.Cluster = &engine.ClusterView{Controller: "pve1", Generation: 4, Members: []string{"pve1", "pve2", "pve3"}, Quorate: true,
		Coverage: engine.CoverageView{Covered: true}}
	c := &fakeCluster{
		agent: agentproto.AgentView{Node: "pve1", Kind: pki.KindHost, Controller: "pve1", Heard: true,
			Epoch: agentproto.Epoch{InstallID: "abc123", Generation: 4, Nonce: "n"}, Seq: 7, SeqAge: 9 * time.Second},
		members: members.Members{Self: "pve1", Quorate: true, Nodes: []members.Member{
			{Name: "pve1", ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}, {Name: "pve3", ID: 3, Online: true},
		}},
		corosync: members.Corosync{Nodes: []string{"pve1", "pve2", "pve3"}, Links: map[string][]netip.Addr{
			"pve1": {netip.MustParseAddr("192.168.10.1")}, "pve2": {netip.MustParseAddr("192.168.10.2")}, "pve3": {netip.MustParseAddr("192.168.10.3")},
		}},
		ca:    now.AddDate(9, 0, 0),
		facts: engine.ClusterFacts{Started: now.Add(-time.Hour), CloudflareAt: now.Add(-10 * time.Second), ReadyReplicas: map[string]int{sharedID: 3}},
	}
	return st, &clusterEnv{fakeEnv: healthyEnv(), fakeCluster: c}
}

func clusterFindings(findings []Finding) map[string]Finding {
	out := map[string]Finding{}
	for _, f := range findings {
		out[f.Check] = f
	}
	return out
}

func TestAHealthyClusterOnTheController(t *testing.T) {
	st, env := healthyCluster()

	got := clusterFindings(RunWith(t.Context(), st, env, Options{}))

	for check, detail := range map[string]string{
		"lease":                               "pve1 holds the lease at generation 4, refreshed 3s ago",
		"quorum device":                       "3 nodes; a quorum device is not needed",
		"quorum override":                     "corosync.conf overrides no quorum",
		"cluster members":                     "every node of the install is a member of the cluster",
		"agent channel":                       "every node online sends its status",
		"agent address":                       "no agent listens on a link of corosync",
		"clock skew":                          "the clocks of the nodes agree within 2s",
		"node versions":                       "every node runs the same build",
		"certificates":                        "no certificate of a node ends within 30 days, nor the CA within a year",
		"ca rotation":                         "no rotation under way",
		"upgrade":                             "no upgrade is unfinished",
		"coverage":                            "every node has an agent whose watch runs",
		"cloudflare reach":                    "the controller reached Cloudflare 10s ago",
		"pinned tunnels":                      "no tunnel pinned to a node is left without routes",
		"replicas pco-abc123 in account acc1": "3 nodes report a ready connector",
		"agent":                               "the agent serves snapshot 7 of generation 4, applied 9s ago",
		"agent certificate":                   "the agent of this node runs in the controller's process",
	} {
		require.Equal(t, Finding{Check: check, Level: LevelOK, Detail: detail}, got[check], check)
	}
	require.NotContains(t, got, "controller", "the controller dials no one")
	require.False(t, Failed(RunWith(t.Context(), st, env, Options{})))
}

// A single node has none of the checks of a cluster: its findings are what
// they were before there were clusters.
func TestASingleNodeHasNoChecksOfACluster(t *testing.T) {
	st, env := healthyCluster()
	st.Nodes = st.Nodes[:0]
	st.Lease.Holder = "pve1"
	env.agent.Remote = false

	require.Equal(t, Run(t.Context(), st, env.fakeEnv), RunWith(t.Context(), st, env, Options{}))
}

func TestTheChecksOfAClusterFindFaults(t *testing.T) {
	for _, tt := range []struct {
		name   string
		break_ func(st *engine.State, c *fakeCluster)
		check  string
		level  Level
		detail string
		fix    string
	}{
		{"a slow refresh of the lease", func(st *engine.State, _ *fakeCluster) { st.Lease.RefreshAge = 20 * time.Second },
			"lease", LevelWarn, "pve1 holds the lease, refreshed 20s ago, more than 15s", "pmxcfs answers slowly: pvecm status and journalctl -u pve-cluster on pve1"},
		{"a controller behind", func(st *engine.State, _ *fakeCluster) { st.Lease.Behind = "pve2 reported generation 5" },
			"lease", LevelFail, "the controller is behind the agents: pve2 reported generation 5; nothing is written",
			"read the differences with pco promote, then confirm them or put the store right"},
		{"two nodes without a quorum device", func(st *engine.State, _ *fakeCluster) { st.Cluster.Members = []string{"pve1", "pve2"} },
			"quorum device", LevelWarn, "two nodes without a quorum device: when one is lost, the other has no quorum and pco on it writes nothing",
			"add a quorum device: pvecm qdevice setup <address>"},
		{"two_node without a device", func(st *engine.State, _ *fakeCluster) { st.Cluster.Overrides = []string{"two_node"} },
			"quorum override", LevelFail,
			"corosync.conf overrides the quorum: two_node; two parts of the cluster may both have quorum, and pco relies on the sentinel of its writer alone",
			"remove two_node from the quorum section of /etc/pve/corosync.conf"},
		{"expected_votes", func(st *engine.State, _ *fakeCluster) { st.Cluster.Overrides = []string{"expected_votes 1"} },
			"quorum override", LevelFail,
			"corosync.conf overrides the quorum: expected_votes 1; two parts of the cluster may both have quorum, and pco relies on the sentinel of its writer alone",
			"remove expected_votes 1 from the quorum section of /etc/pve/corosync.conf"},
		{"a node the cluster lost", func(st *engine.State, _ *fakeCluster) { st.Cluster.Members = []string{"pve1", "pve2"} },
			"cluster members", LevelFail, "the cluster does not list pve3, which the install still counts", "pco node forget pve3"},
		{"a node online without status", func(st *engine.State, _ *fakeCluster) { st.Nodes[2].StatusAge = time.Minute },
			"agent channel", LevelFail, ".members lists 1 node online that has sent no status: pve3",
			"check cluster.agentNetwork and that the datacenter firewall admits tcp/8644 from the ipset pco-nodes; " +
				"pco doctor on each of them says whether its agent reaches the controller"},
		{"a node online never heard", func(st *engine.State, c *fakeCluster) {
			c.members.Nodes = append(c.members.Nodes, members.Member{Name: "pve4", ID: 4, Online: true})
		}, "agent channel", LevelFail, ".members lists 1 node online that has sent no status: pve4", ""},
		{"an agent on a corosync link", func(st *engine.State, _ *fakeCluster) { st.Nodes[1].AgentAddress = netip.MustParseAddr("192.168.10.2") },
			"agent address", LevelWarn, "the agent of pve2 (192.168.10.2) listens on a link of corosync, whose traffic it then competes with",
			"set cluster.agentNetwork to another network in the settings, then restart pco on every node"},
		{"a clock 5 s off", func(st *engine.State, _ *fakeCluster) { st.Nodes[2].ClockSkew = -5 * time.Second },
			"clock skew", LevelWarn, "the clock of pve3 is 5s off the controller's, more than 2s", "check the time service on pve3: timedatectl; chronyc tracking"},
		{"a clock 15 s off", func(st *engine.State, _ *fakeCluster) { st.Nodes[1].ClockSkew = 15 * time.Second },
			"clock skew", LevelFail, "the clock of pve2 is 15s off the controller's, more than 10s", "check the time service on pve2: timedatectl; chronyc tracking"},
		{"another build", func(st *engine.State, _ *fakeCluster) { st.Nodes[2].Build = "0.2.0" },
			"node versions", LevelWarn, "the nodes run different builds: 0.2.0 on pve3; 0.3.0 on pve1, pve2", "upgrade the nodes that run an older build"},
		{"a certificate about to end", func(st *engine.State, _ *fakeCluster) { st.Nodes[1].CertExpires = now.Add(10 * 24 * time.Hour) },
			"certificates", LevelWarn, "certificates of nodes end within 30 days: pve2's on 2026-10-11", ""},
		{"the CA within a year", func(_ *engine.State, c *fakeCluster) { c.ca = now.AddDate(0, 6, 0) },
			"certificates", LevelWarn, "the CA of the install ends on 2027-04-01, in less than a year", ""},
		{"a rotation under way", func(st *engine.State, _ *fakeCluster) {
			st.Cluster.CARotation = &engine.CARotationView{Node: "pve3", StartedAt: now.Add(-time.Hour), Next: "rotate tunnels"}
		}, "ca rotation", LevelWarn, "the rotation for pve3 is under way since 2026-10-01T11:00:00Z, at the step rotate tunnels", "pco cluster status shows its steps"},
		{"nodes refused after a rotation", func(st *engine.State, _ *fakeCluster) {
			st.Cluster.CARotation = &engine.CARotationView{Node: "pve3", Refused: []string{"pve2"}}
		}, "ca rotation", LevelFail, "after the rotation for pve3, the controller refuses the certificates of pve2", ""},
		{"an unfinished upgrade", func(st *engine.State, _ *fakeCluster) {
			st.Cluster.Upgrade = &engine.UpgradeView{From: "0.2.0", Since: now.Add(-48 * time.Hour)}
		}, "upgrade", LevelWarn, "the upgrade from 0.2.0 is not finished since 2026-09-29", "pco cluster finish-upgrade once every node runs this release"},
		{"a flapping node", func(st *engine.State, _ *fakeCluster) {
			st.Cluster.Coverage.Flapping = []string{"pve2"}
			st.Routes = []engine.RouteView{{RouteStatus: planner.RouteStatus{Hostname: "app.example.com"}, Placement: &engine.PlacementView{Tunnel: "pco-abc123-pve2", Kind: engine.TunnelPinned, Node: "pve2"}}}
		}, "coverage", LevelWarn, "the watch of pve2 was lost twice within ten minutes, which keeps app.example.com pinned", ""},
		{"a node without an agent", func(st *engine.State, _ *fakeCluster) { st.Cluster.Coverage.Uncovered = []string{"pve3"} },
			"coverage", LevelWarn, "pve3 has no agent whose watch runs, so no port another node proves counts", ""},
		{"one ready replica", func(_ *engine.State, c *fakeCluster) { c.facts.ReadyReplicas[sharedID] = 1 },
			"replicas pco-abc123 in account acc1", LevelWarn, "one node reports a ready connector: the loss of that node takes the tunnel down", ""},
		{"a pinned tunnel without routes", func(_ *engine.State, c *fakeCluster) {
			c.facts.PinnedEmpty = []engine.PinnedEmpty{{Name: "pco-abc123-pve2", AccountID: "acc1", Since: now.Add(-11 * time.Minute)}}
		}, "pinned tunnels", LevelWarn, "tunnels pinned to a node serve no route and are not deleted: pco-abc123-pve2 in account acc1 since 2026-10-01T11:49:00Z", ""},
		{"Cloudflare silent", func(_ *engine.State, c *fakeCluster) { c.facts.CloudflareAt = now.Add(-11 * time.Minute) },
			"cloudflare reach", LevelWarn, "the controller has not reached Cloudflare since 2026-10-01T11:49:00Z",
			"pco status says why; if another node reaches Cloudflare, pco cluster handoff moves the controller"},
		{"Cloudflare never reached", func(_ *engine.State, c *fakeCluster) { c.facts.CloudflareAt = time.Time{} },
			"cloudflare reach", LevelWarn, "the controller has not reached Cloudflare since 2026-10-01T11:00:00Z", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, env := healthyCluster()
			tt.break_(&st, env.fakeCluster)

			f := clusterFindings(RunWith(t.Context(), st, env, Options{}))[tt.check]

			require.Equal(t, tt.level, f.Level, f.Detail)
			require.Equal(t, tt.detail, f.Detail)
			if tt.fix != "" {
				require.Equal(t, tt.fix, f.Fix)
			}
		})
	}
}

// Whatever Proxmox cannot say is a warning, never a check left out.
func TestTheChecksOfAClusterSayWhatTheyCouldNotRead(t *testing.T) {
	st, env := healthyCluster()
	env.memErr, env.csErr, env.caErr = errors.New("no quorum"), errors.New("gone"), errors.New("unreadable")

	got := clusterFindings(RunWith(t.Context(), st, env, Options{}))

	require.Equal(t, LevelWarn, got["agent channel"].Level)
	require.Equal(t, LevelWarn, got["agent address"].Level)
	require.Equal(t, "the CA of the install could not be read: unreadable", got["certificates"].Detail)
}

// pco doctor --cluster asks the other Active nodes, waits 30 seconds for
// them, and names their findings with the node; an agent that does not
// answer is listed.
// What an agent answers is its own word: only findings of the checks an
// agent makes, by their exact names, which the controller names with the
// node itself; bounded in number and length, and of the three levels.
func TestTheAnswerOfAnAgentIsBounded(t *testing.T) {
	st, env := healthyCluster()
	long := strings.Repeat("é", 400)
	findings := []Finding{{Check: "cloudflared", Level: "fine", Detail: long, Fix: long}}
	for _, spoof := range []string{
		"store on pve3", "cloudflared\u00a0on pve1", "cloudflared on pve1\u200b", "cloudflared ", "cloudflared\non pve1",
		"c\u043eudflared", "lease", "answer",
	} {
		findings = append(findings, Finding{Check: spoof, Level: LevelFail, Detail: "a word about something else"})
	}
	for range maxAnswerFindings + 5 {
		findings = append(findings, Finding{Check: "egress", Level: LevelOK})
	}
	env.answers = map[string]AgentAnswer{"pve2": {Answered: true, Findings: findings}}

	got := RunWith(t.Context(), st, env, Options{Cluster: true})

	var mine []Finding
	for _, f := range got {
		if strings.HasSuffix(f.Check, " on pve2") && !strings.HasPrefix(f.Check, "answer") {
			mine = append(mine, f)
			require.Contains(t, []string{"cloudflared on pve2", "egress on pve2"}, f.Check)
		}
		require.NotContains(t, f.Detail, "a word about something else")
	}
	require.Len(t, mine, maxAnswerFindings)
	first := clusterFindings(got)["cloudflared on pve2"]
	require.Equal(t, LevelWarn, first.Level, "a level it does not know")
	require.LessOrEqual(t, len(first.Detail), maxAnswerText)
	require.True(t, utf8.ValidString(first.Detail))
	require.LessOrEqual(t, len(first.Fix), maxAnswerText)
	answer := clusterFindings(got)["answer on pve2"]
	require.Equal(t, LevelWarn, answer.Level)
	require.Equal(t, "the agent answered with 78 findings; 14 were left out: 8 of no check an agent makes, 6 over the 64 an answer may carry", answer.Detail)
}

// Every check AgentChecks makes is one an answer may carry.
func TestTheChecksOfAnAgentAreTheOnesItMayAnswerWith(t *testing.T) {
	st, env := healthyCluster()
	env.agent.Remote = true
	names := agentCheckNames()
	for _, f := range AgentChecks(t.Context(), st, env) {
		require.True(t, names[f.Check], f.Check)
	}
}

func TestTheClusterRunAsksTheOtherAgents(t *testing.T) {
	st, env := healthyCluster()
	st.Nodes[2].State = store.NodeDraining
	st.Nodes = append(st.Nodes, engine.NodeView{Name: "pve4", State: store.NodeActive, Build: "0.3.0", StatusAge: time.Second})
	env.members.Nodes = append(env.members.Nodes, members.Member{Name: "pve4", ID: 4, Online: true})
	env.answers = map[string]AgentAnswer{
		"pve2": {Answered: true, Findings: []Finding{{Check: "cloudflared", Level: LevelFail, Detail: "cloudflared does not run", Fix: "install it"}}},
	}

	findings := RunWith(t.Context(), st, env, Options{Cluster: true})

	require.Equal(t, []string{"pve2", "pve4"}, env.asked)
	require.Equal(t, 30*time.Second, env.waited)
	got := clusterFindings(findings)
	require.Equal(t, Finding{Check: "cloudflared on pve2", Level: LevelFail, Detail: "cloudflared does not run", Fix: "install it"}, got["cloudflared on pve2"])
	require.Equal(t, Finding{Check: "answer on pve2", Level: LevelOK, Detail: "the agent answered with 1 finding"}, got["answer on pve2"])
	require.Equal(t, Finding{Check: "answer on pve4", Level: LevelWarn, Detail: "the agent did not answer within 30s", Fix: "pco doctor on pve4"}, got["answer on pve4"])
	require.True(t, Failed(findings))
	i := slices.IndexFunc(findings, func(f Finding) bool { return f.Check == "cloudflared" })
	require.Equal(t, "cloudflared on pve2", findings[i+1].Check, "beside the controller's own")

	env.answers = map[string]AgentAnswer{"pve2": {Answered: true, Err: "this agent does not carry out doctor actions"}}
	got = clusterFindings(RunWith(t.Context(), st, env, Options{Cluster: true}))
	require.Equal(t, LevelFail, got["answer on pve2"].Level)
}

// A node that is not the controller checks itself and its agent, and says
// where the checks of the cluster run, or that no controller answers.
func TestANodeThatIsNotTheController(t *testing.T) {
	st, env := healthyCluster()
	st.Node = "pve2"
	st.Lease = &engine.LeaseView{Holder: "pve1", Address: netip.MustParseAddr("10.0.0.1"), Generation: 4}
	env.agent = agentproto.AgentView{Node: "pve2", Kind: pki.KindHost, Controller: "pve1", Remote: true, Heard: true,
		SilentFor: 4 * time.Second, HeartbeatAge: 4 * time.Second, Epoch: agentproto.Epoch{Generation: 4}, Seq: 3,
		CertExpires: now.Add(70 * 24 * time.Hour)}

	got := clusterFindings(RunWith(t.Context(), st, env, Options{}))

	require.Equal(t, Finding{Check: "cluster", Level: LevelOK, Detail: "the checks of the cluster run on the controller, pve1 (10.0.0.1): pco doctor --cluster there"}, got["cluster"])
	require.Equal(t, Finding{Check: "controller", Level: LevelOK, Detail: "the controller, pve1 (10.0.0.1), answered 4s ago"}, got["controller"])
	require.Equal(t, Finding{Check: "agent dial", Level: LevelOK, Detail: "the agent endpoint of the controller, 10.0.0.1:8644, answers"}, got["agent dial"])
	require.Equal(t, LevelOK, got["agent certificate"].Level)
	require.NotContains(t, got, "lease")
	require.NotContains(t, got, "mode", "the state of a node that does not write says nothing of the install")
	require.Contains(t, env.dialed, "tcp 10.0.0.1:8644")

	got = clusterFindings(RunWith(t.Context(), st, env, Options{Cluster: true}))
	require.Equal(t, Finding{Check: "cluster", Level: LevelWarn, Detail: "this node is not the controller; the checks of the cluster run on pve1 (10.0.0.1)",
		Fix: "pco doctor --cluster on pve1 (10.0.0.1)"}, got["cluster"])
}

func TestTheAgentOfANodeFindsFaults(t *testing.T) {
	for _, tt := range []struct {
		name   string
		break_ func(st *engine.State, env *clusterEnv)
		check  string
		level  Level
		detail string
		fix    string
	}{
		{"a controller silent for a minute", func(_ *engine.State, env *clusterEnv) { env.agent.SilentFor = 70 * time.Second },
			"controller", LevelWarn, "the controller, pve1 (10.0.0.1), has been silent for 1m10s", ""},
		{"no controller for three minutes", func(_ *engine.State, env *clusterEnv) { env.agent.SilentFor = 4 * time.Minute },
			"controller", LevelFail, "no controller answered for 4m0s", ""},
		{"no holder and nothing heard", func(st *engine.State, env *clusterEnv) {
			st.Lease = &engine.LeaseView{}
			env.agent.Heard, env.agent.SilentFor = false, 20*time.Second
		}, "controller", LevelFail, "no controller answered for 20s", ""},
		{"8644 cut", func(_ *engine.State, env *clusterEnv) { env.dialErr = errors.New("connection timed out") },
			"agent dial", LevelFail, "the agent endpoint of the controller, 10.0.0.1:8644, cannot be reached: connection timed out", fixDialHost},
		{"8644 cut to an appliance controller", func(st *engine.State, env *clusterEnv) {
			st.Lease = nil
			env.agent.Kind, env.agent.Controller = pki.KindAppliance, "10.0.0.50:8644"
			env.dialErr = errors.New("refused")
		}, "agent dial", LevelFail, "the agent endpoint of the controller, 10.0.0.50:8644, cannot be reached: refused", fixDialAppliance},
		{"no address of the controller", func(st *engine.State, env *clusterEnv) {
			st.Lease = &engine.LeaseView{Holder: "pve1"}
			env.agent.Controller = "pve1"
		}, "agent dial", LevelWarn, "the address of the controller is not known on this node", ""},
		{"an agent that stood down", func(_ *engine.State, env *clusterEnv) { env.agent.StoodDown = "the node was removed from the install" },
			"agent", LevelFail, "the agent stood down: the node was removed from the install", ""},
		{"an agent with problems", func(_ *engine.State, env *clusterEnv) {
			env.agent.Problems = []string{"snapshot refused: out of range"}
		},
			"agent", LevelWarn, "1 problem: snapshot refused: out of range", ""},
		{"a certificate about to end", func(_ *engine.State, env *clusterEnv) { env.agent.CertExpires = now.Add(5 * 24 * time.Hour) },
			"agent certificate", LevelWarn, "the certificate of the agent ends on 2026-10-06, in 5 days",
			"systemctl restart pco: a host node issues its own at the start of the daemon"},
		{"an ended certificate of an appliance", func(_ *engine.State, env *clusterEnv) {
			env.agent.Kind, env.agent.CertExpires = pki.KindAppliance, now.Add(-time.Hour)
		}, "agent certificate", LevelFail, "the certificate of the agent ended on 2026-10-01", "pco node admit pve2 --renew on the controller"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, env := healthyCluster()
			st.Node = "pve2"
			st.Lease = &engine.LeaseView{Holder: "pve1", Address: netip.MustParseAddr("10.0.0.1"), Generation: 4}
			env.agent = agentproto.AgentView{Node: "pve2", Kind: pki.KindHost, Controller: "pve1", Remote: true, Heard: true,
				Epoch: agentproto.Epoch{Generation: 4}, CertExpires: now.Add(70 * 24 * time.Hour)}
			tt.break_(&st, env)

			f := clusterFindings(AgentChecks(t.Context(), st, env))[tt.check]

			require.Equal(t, tt.level, f.Level, f.Detail)
			require.Equal(t, tt.detail, f.Detail)
			if tt.fix != "" {
				require.Equal(t, tt.fix, f.Fix)
			}
		})
	}
}

// What the controller asks an agent for has no checks of the state of the
// controller.
// An agent not asked because another ask ran says so, not that it was
// silent.
func TestAnAgentNotAskedSaysWhy(t *testing.T) {
	st, env := healthyCluster()
	env.answers = map[string]AgentAnswer{"pve2": {Why: "another doctor ask was running"}, "pve3": {Why: "another doctor ask was running"}}

	got := clusterFindings(RunWith(t.Context(), st, env, Options{Cluster: true}))

	require.Equal(t, Finding{Check: "answer on pve2", Level: LevelWarn, Detail: "the agent was not asked: another doctor ask was running",
		Fix: "run pco doctor --cluster again once the other run ended"}, got["answer on pve2"])
}

// An appliance agent's checks, those of its container among them, are all
// ones an answer may carry.
func TestTheChecksOfAnApplianceAgentAreTheOnesItMayAnswerWith(t *testing.T) {
	_, cl := healthyCluster()
	cl.agent.Remote, cl.agent.Kind = true, pki.KindAppliance
	env := struct {
		*applianceEnv
		*fakeCluster
	}{healthyApplianceEnv(), cl.fakeCluster}
	names := agentCheckNames()

	findings := AgentChecks(t.Context(), healthyApplianceState(), env)

	require.Contains(t, clusterFindings(findings), "cmode")
	for _, f := range findings {
		require.True(t, names[f.Check], f.Check)
	}
}

// Without a cluster the checks an agent answers with are still those an
// answer may carry.
func TestTheChecksOfANodeWithoutAClusterAreAnAgentsChecks(t *testing.T) {
	names := agentCheckNames()
	for _, f := range AgentChecks(t.Context(), healthyState(), healthyEnv()) {
		require.True(t, names[f.Check], f.Check)
	}
}

func TestTheChecksOfAnAgentLeaveTheInstallOut(t *testing.T) {
	st, env := healthyCluster()
	env.agent.Remote = true

	var checks []string
	for _, f := range AgentChecks(t.Context(), st, env) {
		checks = append(checks, f.Check)
	}

	require.Equal(t, []string{
		"agent", "agent certificate", "agent dial", "cloudflared", "controller", "egress", "nftables", "node lock", "outbound", "proxmox", "store",
	}, checks)
}

// A run that asks the other nodes is not answered by a plain run that just
// ended, nor the other way round; the options come with the request.
func TestTheRunnerKeepsTheRunOfTheClusterApart(t *testing.T) {
	st, env := healthyCluster()
	r := NewRunner(func() engine.State { return st }, env, nil, func() time.Time { return now }, zerolog.Nop())

	plain := r.Doctor(t.Context())
	require.Nil(t, env.asked)

	all := r.Doctor(WithOptions(t.Context(), Options{Cluster: true}))
	require.Equal(t, []string{"pve2", "pve3"}, env.asked)
	require.Greater(t, len(all), len(plain))

	agent := r.AgentChecks(t.Context())
	require.NotContains(t, clusterFindings(agent), "lease")
}
