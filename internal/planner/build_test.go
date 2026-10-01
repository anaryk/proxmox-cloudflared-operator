package planner

import (
	"encoding/json"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

var (
	buildWriter = Writer{InstallID: "3f9a2c1e77b0", Generation: 4, Nonce: "k3x9q1"}

	shopZone    = Zone{ID: "z-shop", Name: "shop.cz", AccountID: "acc1", CredentialID: "cred1"}
	exampleZone = Zone{ID: "z-example", Name: "example.com", AccountID: "acc2", CredentialID: "cred2"}
)

const (
	tunnelName   = "pco-3f9a2c1e77b0"
	deepWarning  = "more than one level below shop.cz: needs an advanced certificate"
	originAddr   = "10.20.0.15"
	noAddrReason = "no verified address yet"
)

// winner builds a route for a guest or manual owner that serves http on port
// 8080 and leaves the address to resolution.
func winner(t *testing.T, host, owner string, mods ...func(*model.Route)) model.Route {
	t.Helper()
	r := routes(t, host, owner)[0]
	r.Target.Port = 8080
	for _, mod := range mods {
		mod(&r)
	}
	return r
}

func withAddr(addr string) func(*model.Route) {
	return func(r *model.Route) { r.Target.Addr = netip.MustParseAddr(addr) }
}

func verified(addr string) ResolvedTarget {
	return ResolvedTarget{Addr: netip.MustParseAddr(addr), Reachable: true}
}

// allVerified resolves every host to originAddr.
func allVerified(hosts ...string) map[string]ResolvedTarget {
	out := make(map[string]ResolvedTarget, len(hosts))
	for _, h := range hosts {
		out[h] = verified(originAddr)
	}
	return out
}

func httpRule(host, addr string) IngressRule {
	return IngressRule{Hostname: host, Service: "http://" + addr + ":8080"}
}

func blockRule(host string) IngressRule {
	return IngressRule{Hostname: host, Service: "http_status:503"}
}

// withSentinel appends the rules every tunnel ends with.
func withSentinel(rules ...IngressRule) []IngressRule {
	return append(rules,
		IngressRule{Hostname: SentinelHostname(buildWriter), Service: "http_status:404"},
		IngressRule{Service: "http_status:404"},
	)
}

func recordFor(z Zone, name string) RecordPlan {
	return RecordPlan{
		ZoneID:       z.ID,
		ZoneName:     z.Name,
		AccountID:    z.AccountID,
		CredentialID: z.CredentialID,
		Name:         name,
		TunnelName:   tunnelName,
	}
}

func ruleHosts(rules []IngressRule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Hostname)
	}
	return out
}

func recordNames(records []RecordPlan) []string {
	var out []string
	for _, r := range records {
		out = append(out, r.Name)
	}
	return out
}

func warningsByHost(plan Plan) map[string][]string {
	out := make(map[string][]string)
	for _, st := range plan.Routes {
		out[st.Hostname] = st.Warnings
	}
	return out
}

func TestBuildExactBeforeWildcard(t *testing.T) {
	in := BuildInput{
		Winners: []model.Route{
			winner(t, "*.shop.cz", "qemu/1"),
			winner(t, "api.shop.cz", "qemu/1"),
			winner(t, "shop.cz", "qemu/1"),
		},
		Targets: allVerified("*.shop.cz", "api.shop.cz", "shop.cz"),
		Zones:   []Zone{shopZone},
		Writer:  buildWriter,
	}

	plan := Build(in)

	require.Len(t, plan.Tunnels, 1)
	require.Equal(t, []string{
		"api.shop.cz", "shop.cz", "*.shop.cz", SentinelHostname(buildWriter), "",
	}, ruleHosts(plan.Tunnels[0].Rules))
	require.Equal(t, withSentinel(
		httpRule("api.shop.cz", originAddr),
		httpRule("shop.cz", originAddr),
		httpRule("*.shop.cz", originAddr),
	), plan.Tunnels[0].Rules)
}

func TestBuildServiceAndTLSOptions(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		scheme model.Scheme
		addr   string
		opts   model.RouteOptions
		want   IngressRule
	}{
		{
			name: "plain http", host: "app.shop.cz", scheme: model.SchemeHTTP, addr: "10.20.0.15",
			want: IngressRule{Hostname: "app.shop.cz", Service: "http://10.20.0.15:8080"},
		},
		{
			name: "http takes only the host header", host: "app.shop.cz", scheme: model.SchemeHTTP, addr: "10.20.0.15",
			opts: model.RouteOptions{NoTLSVerify: true, SNI: "inner.shop.cz", HostHeader: "inner.shop.cz"},
			want: IngressRule{Hostname: "app.shop.cz", Service: "http://10.20.0.15:8080", HTTPHostHeader: "inner.shop.cz"},
		},
		{
			name: "https names the hostname as server name", host: "app.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			want: IngressRule{Hostname: "app.shop.cz", Service: "https://10.20.0.15:8080", OriginServerName: "app.shop.cz"},
		},
		{
			name: "https with sni", host: "app.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			opts: model.RouteOptions{SNI: "inner.shop.cz"},
			want: IngressRule{Hostname: "app.shop.cz", Service: "https://10.20.0.15:8080", OriginServerName: "inner.shop.cz"},
		},
		{
			name: "https wildcard matches sni to host", host: "*.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			want: IngressRule{Hostname: "*.shop.cz", Service: "https://10.20.0.15:8080", MatchSNIToHost: true},
		},
		{
			name: "https wildcard with sni", host: "*.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			opts: model.RouteOptions{SNI: "inner.shop.cz"},
			want: IngressRule{Hostname: "*.shop.cz", Service: "https://10.20.0.15:8080", OriginServerName: "inner.shop.cz"},
		},
		{
			name: "https without verification sets no name", host: "app.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			opts: model.RouteOptions{NoTLSVerify: true},
			want: IngressRule{Hostname: "app.shop.cz", Service: "https://10.20.0.15:8080", NoTLSVerify: true},
		},
		{
			name: "https wildcard without verification sets no name", host: "*.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			opts: model.RouteOptions{NoTLSVerify: true},
			want: IngressRule{Hostname: "*.shop.cz", Service: "https://10.20.0.15:8080", NoTLSVerify: true},
		},
		{
			name: "https without verification keeps sni", host: "app.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			opts: model.RouteOptions{NoTLSVerify: true, SNI: "inner.shop.cz"},
			want: IngressRule{
				Hostname: "app.shop.cz", Service: "https://10.20.0.15:8080", NoTLSVerify: true, OriginServerName: "inner.shop.cz",
			},
		},
		{
			name: "https with host header", host: "app.shop.cz", scheme: model.SchemeHTTPS, addr: "10.20.0.15",
			opts: model.RouteOptions{HostHeader: "inner.shop.cz"},
			want: IngressRule{
				Hostname: "app.shop.cz", Service: "https://10.20.0.15:8080", OriginServerName: "app.shop.cz", HTTPHostHeader: "inner.shop.cz",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := winner(t, tt.host, "qemu/1")
			rt.Target.Scheme = tt.scheme
			rt.Options = tt.opts

			plan := Build(BuildInput{
				Winners: []model.Route{rt},
				Targets: map[string]ResolvedTarget{tt.host: verified(tt.addr)},
				Zones:   []Zone{shopZone},
				Writer:  buildWriter,
			})

			require.Len(t, plan.Tunnels, 1)
			require.Equal(t, withSentinel(tt.want), plan.Tunnels[0].Rules)
			require.Equal(t, tt.want.Service, plan.Routes[0].Service)
		})
	}
}

func TestBuildStates(t *testing.T) {
	in := BuildInput{
		Winners: []model.Route{
			winner(t, "never.shop.cz", "qemu/1"),
			winner(t, "silent.shop.cz", "qemu/2"),
			winner(t, "down.shop.cz", "qemu/3"),
			winner(t, "quiet.shop.cz", "qemu/4"),
			winner(t, "gone.shop.cz", "qemu/5"),
			winner(t, "lost.shop.cz", "qemu/6"),
			winner(t, "banned.shop.cz", "qemu/7"),
			winner(t, "refused.shop.cz", "qemu/8"),
			winner(t, "ok.shop.cz", "qemu/9"),
		},
		Targets: map[string]ResolvedTarget{
			"silent.shop.cz": {Reason: "guest agent not running"},
			"down.shop.cz":   {Addr: netip.MustParseAddr("10.20.0.30"), Reason: "probe timed out"},
			"quiet.shop.cz":  {Addr: netip.MustParseAddr("10.20.0.35")},
			"gone.shop.cz": {
				Addr: netip.MustParseAddr("10.20.0.40"), Withdrawn: true, Reason: "identity changed",
			},
			"lost.shop.cz": {Addr: netip.MustParseAddr("10.20.0.45"), Withdrawn: true},
			"banned.shop.cz": {
				Addr: netip.MustParseAddr("10.20.0.50"), Reachable: true, Rejected: true, Reason: "address belongs to another guest",
			},
			"refused.shop.cz": {Addr: netip.MustParseAddr("10.20.0.55"), Reachable: true, Withdrawn: true, Rejected: true},
			"ok.shop.cz":      verified("10.20.0.60"),
		},
		Zones:  []Zone{shopZone},
		Writer: buildWriter,
	}

	plan := Build(in)

	require.Equal(t, []RouteStatus{
		{
			Hostname: "banned.shop.cz", Owner: "qemu/7", State: StateUnreachable, Reason: "address belongs to another guest", Zone: "shop.cz",
		},
		{
			Hostname: "down.shop.cz", Owner: "qemu/3", State: StateUnreachable, Reason: "probe timed out",
			Service: "http://10.20.0.30:8080", Zone: "shop.cz",
		},
		{
			Hostname: "gone.shop.cz", Owner: "qemu/5", State: StateWithdrawn, Reason: "identity changed", Zone: "shop.cz",
		},
		{
			Hostname: "lost.shop.cz", Owner: "qemu/6", State: StateWithdrawn, Reason: "identity check failed", Zone: "shop.cz",
		},
		{
			Hostname: "never.shop.cz", Owner: "qemu/1", State: StateUnreachable, Reason: noAddrReason, Zone: "shop.cz",
		},
		{
			Hostname: "ok.shop.cz", Owner: "qemu/9", State: StateActive, Service: "http://10.20.0.60:8080", Zone: "shop.cz",
		},
		{
			Hostname: "quiet.shop.cz", Owner: "qemu/4", State: StateUnreachable, Reason: "target is not answering",
			Service: "http://10.20.0.35:8080", Zone: "shop.cz",
		},
		{
			Hostname: "refused.shop.cz", Owner: "qemu/8", State: StateUnreachable, Reason: "address must never be served", Zone: "shop.cz",
		},
		{
			Hostname: "silent.shop.cz", Owner: "qemu/2", State: StateUnreachable, Reason: "guest agent not running", Zone: "shop.cz",
		},
	}, plan.Routes)

	require.Equal(t, []TunnelPlan{{
		AccountID:    "acc1",
		CredentialID: "cred1",
		Name:         tunnelName,
		Rules: withSentinel(
			blockRule("banned.shop.cz"),
			httpRule("down.shop.cz", "10.20.0.30"),
			blockRule("gone.shop.cz"),
			blockRule("lost.shop.cz"),
			blockRule("never.shop.cz"),
			httpRule("ok.shop.cz", "10.20.0.60"),
			httpRule("quiet.shop.cz", "10.20.0.35"),
			blockRule("refused.shop.cz"),
			blockRule("silent.shop.cz"),
		),
	}}, plan.Tunnels)

	require.Equal(t, []string{
		"down.shop.cz", "gone.shop.cz", "lost.shop.cz", "ok.shop.cz", "quiet.shop.cz",
	}, recordNames(plan.Records))
	require.Equal(t, recordFor(shopZone, "down.shop.cz"), plan.Records[0])
}

func TestBuildServesOnlyTheResolvedAddress(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "pinned.shop.cz", "qemu/1", withAddr("10.20.0.50")),
			winner(t, "unknown.shop.cz", "qemu/2", withAddr("10.20.0.60")),
			winner(t, "zero.shop.cz", "manual/web", withAddr("10.20.0.70")),
		},
		Targets: map[string]ResolvedTarget{
			"pinned.shop.cz": verified(originAddr),
			"zero.shop.cz":   {Reachable: true},
		},
		Zones:  []Zone{shopZone},
		Writer: buildWriter,
	})

	require.Equal(t, withSentinel(
		httpRule("pinned.shop.cz", originAddr),
		blockRule("unknown.shop.cz"),
		blockRule("zero.shop.cz"),
	), plan.Tunnels[0].Rules)
	require.Equal(t, []string{"pinned.shop.cz"}, recordNames(plan.Records))
	require.Equal(t, []RouteStatus{
		{Hostname: "pinned.shop.cz", Owner: "qemu/1", State: StateActive, Service: "http://10.20.0.15:8080", Zone: "shop.cz"},
		{Hostname: "unknown.shop.cz", Owner: "qemu/2", State: StateUnreachable, Reason: noAddrReason, Zone: "shop.cz"},
		{Hostname: "zero.shop.cz", Owner: "manual/web", State: StateUnreachable, Reason: noAddrReason, Zone: "shop.cz"},
	}, plan.Routes)
}

func TestBuildTargetVerifiedForAnotherOwnerIsBlocked(t *testing.T) {
	const reason = "address was verified for another owner"
	addr := netip.MustParseAddr(originAddr)

	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "mine.shop.cz", "qemu/1"),
			winner(t, "taken.shop.cz", "qemu/2"),
			winner(t, "gone.shop.cz", "qemu/3"),
			winner(t, "anyone.shop.cz", "manual/web"),
		},
		Targets: map[string]ResolvedTarget{
			"mine.shop.cz":   {Addr: addr, Reachable: true, Owner: "qemu/1"},
			"taken.shop.cz":  {Addr: addr, Reachable: true, Owner: "qemu/1", Reason: "probe passed"},
			"gone.shop.cz":   {Addr: addr, Withdrawn: true, Owner: "qemu/9"},
			"anyone.shop.cz": verified(originAddr),
		},
		Zones:  []Zone{shopZone},
		Writer: buildWriter,
	})

	require.Equal(t, withSentinel(
		httpRule("anyone.shop.cz", originAddr),
		blockRule("gone.shop.cz"),
		httpRule("mine.shop.cz", originAddr),
		blockRule("taken.shop.cz"),
	), plan.Tunnels[0].Rules)
	require.Equal(t, []string{"anyone.shop.cz", "mine.shop.cz"}, recordNames(plan.Records))
	require.Equal(t, []RouteStatus{
		{Hostname: "anyone.shop.cz", Owner: "manual/web", State: StateActive, Service: "http://10.20.0.15:8080", Zone: "shop.cz"},
		{Hostname: "gone.shop.cz", Owner: "qemu/3", State: StateUnreachable, Reason: reason, Zone: "shop.cz"},
		{Hostname: "mine.shop.cz", Owner: "qemu/1", State: StateActive, Service: "http://10.20.0.15:8080", Zone: "shop.cz"},
		{Hostname: "taken.shop.cz", Owner: "qemu/2", State: StateUnreachable, Reason: reason, Zone: "shop.cz"},
	}, plan.Routes)
}

func TestBuildAccountWithOnlyBlocksGetsNoTunnel(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "never.shop.cz", "qemu/1"),
			winner(t, "banned.shop.cz", "qemu/2"),
			winner(t, "api.example.com", "qemu/3"),
		},
		Conflicts: []model.Route{winner(t, "held.shop.cz", "qemu/6")},
		Holders: map[string]string{
			"held.shop.cz":     "qemu/4",
			"held.example.com": "qemu/5",
		},
		Targets: map[string]ResolvedTarget{
			"banned.shop.cz":  {Addr: netip.MustParseAddr(originAddr), Reachable: true, Rejected: true},
			"api.example.com": verified(originAddr),
		},
		Zones:  []Zone{shopZone, exampleZone},
		Writer: buildWriter,
	})

	require.Equal(t, []TunnelPlan{{
		AccountID: "acc2", CredentialID: "cred2", Name: tunnelName,
		Rules: withSentinel(httpRule("api.example.com", originAddr), blockRule("held.example.com")),
	}}, plan.Tunnels)
	require.Equal(t, []RecordPlan{recordFor(exampleZone, "api.example.com")}, plan.Records)
	require.Equal(t, []RouteStatus{
		{Hostname: "api.example.com", Owner: "qemu/3", State: StateActive, Service: "http://10.20.0.15:8080", Zone: "example.com"},
		{Hostname: "banned.shop.cz", Owner: "qemu/2", State: StateUnreachable, Reason: "address must never be served", Zone: "shop.cz"},
		{Hostname: "held.shop.cz", Owner: "qemu/6", State: StateConflict, Reason: "hostname is held by qemu/4"},
		{Hostname: "never.shop.cz", Owner: "qemu/1", State: StateUnreachable, Reason: noAddrReason, Zone: "shop.cz"},
	}, plan.Routes)
}

func TestBuildWithdrawnKeepsItsRecordBehindABlock(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{winner(t, "gone.shop.cz", "qemu/4")},
		Targets: map[string]ResolvedTarget{
			"gone.shop.cz": {Addr: netip.MustParseAddr(originAddr), Withdrawn: true},
		},
		Zones:  []Zone{shopZone},
		Writer: buildWriter,
	})

	require.Equal(t, []TunnelPlan{{
		AccountID: "acc1", CredentialID: "cred1", Name: tunnelName, Rules: withSentinel(blockRule("gone.shop.cz")),
	}}, plan.Tunnels)
	require.Equal(t, []RecordPlan{recordFor(shopZone, "gone.shop.cz")}, plan.Records)
}

func TestBuildBlocksClaimedNamesFromOtherWildcards(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, "*.shop.cz", "qemu/1"),
			winner(t, "api.shop.cz", "qemu/2"),
		},
		Conflicts: []model.Route{winner(t, "held.shop.cz", "qemu/1")},
		Holders: map[string]string{
			"*.shop.cz":          "qemu/1",
			"api.shop.cz":        "qemu/2",
			"held.shop.cz":       "qemu/3",
			"*.held.shop.cz":     "qemu/3",
			"held.nowhere.org":   "qemu/3",
			"x.pco-ab12.invalid": "qemu/3",
		},
		Targets: map[string]ResolvedTarget{"*.shop.cz": verified(originAddr)},
		Zones:   []Zone{shopZone},
		Writer:  buildWriter,
	})

	require.Equal(t, []TunnelPlan{{
		AccountID: "acc1", CredentialID: "cred1", Name: tunnelName,
		Rules: withSentinel(
			blockRule("api.shop.cz"),
			blockRule("held.shop.cz"),
			blockRule("*.held.shop.cz"),
			httpRule("*.shop.cz", originAddr),
		),
	}}, plan.Tunnels)
	require.Equal(t, []string{"*.shop.cz"}, recordNames(plan.Records))
	require.Equal(t, []RouteStatus{
		{
			Hostname: "*.shop.cz", Owner: "qemu/1", State: StateActive, Service: "http://10.20.0.15:8080", Zone: "shop.cz",
			Warnings: []string{"*.shop.cz overlaps api.shop.cz owned by qemu/2"},
		},
		{
			Hostname: "api.shop.cz", Owner: "qemu/2", State: StateUnreachable, Reason: noAddrReason, Zone: "shop.cz",
			Warnings: []string{"api.shop.cz overlaps *.shop.cz owned by qemu/1"},
		},
		{Hostname: "held.shop.cz", Owner: "qemu/1", State: StateConflict, Reason: "hostname is held by qemu/3"},
	}, plan.Routes)
}

func TestBuildReservedHostnames(t *testing.T) {
	sentinel := SentinelHostname(Writer{InstallID: "ab12", Generation: 1, Nonce: "n0"})
	invalidZone := Zone{ID: "z-invalid", Name: "invalid", AccountID: "acc1", CredentialID: "cred1"}

	plan := Build(BuildInput{
		Winners: []model.Route{
			winner(t, sentinel, "qemu/1"),
			winner(t, "app.invalid", "manual/web"),
			winner(t, "*.other.invalid", "qemu/2"),
		},
		Holders: map[string]string{"held.invalid": "qemu/3"},
		Targets: allVerified(sentinel, "app.invalid", "*.other.invalid"),
		Zones:   []Zone{invalidZone},
		Writer:  buildWriter,
	})

	require.Empty(t, plan.Tunnels)
	require.Empty(t, plan.Records)
	require.Equal(t, []RouteStatus{
		{Hostname: "*.other.invalid", Owner: "qemu/2", State: StateNoZone, Reason: "reserved hostname"},
		{Hostname: "app.invalid", Owner: "manual/web", State: StateNoZone, Reason: "reserved hostname"},
		{Hostname: sentinel, Owner: "qemu/1", State: StateNoZone, Reason: "reserved hostname"},
	}, plan.Routes)
}

func TestBuildZones(t *testing.T) {
	const noZone = "no Cloudflare zone for this hostname in any credential"

	t.Run("two accounts get one tunnel each", func(t *testing.T) {
		plan := Build(BuildInput{
			Winners: []model.Route{winner(t, "a.shop.cz", "qemu/1"), winner(t, "b.example.com", "qemu/2")},
			Targets: allVerified("a.shop.cz", "b.example.com"),
			Zones:   []Zone{shopZone, exampleZone},
			Writer:  buildWriter,
		})

		require.Equal(t, []TunnelPlan{
			{AccountID: "acc1", CredentialID: "cred1", Name: tunnelName, Rules: withSentinel(httpRule("a.shop.cz", originAddr))},
			{AccountID: "acc2", CredentialID: "cred2", Name: tunnelName, Rules: withSentinel(httpRule("b.example.com", originAddr))},
		}, plan.Tunnels)
		require.Equal(t, []RecordPlan{
			recordFor(exampleZone, "b.example.com"),
			recordFor(shopZone, "a.shop.cz"),
		}, plan.Records)
	})

	t.Run("no zone", func(t *testing.T) {
		plan := Build(BuildInput{
			Winners: []model.Route{winner(t, "x.nowhere.org", "qemu/1")},
			Targets: allVerified("x.nowhere.org"),
			Zones:   []Zone{shopZone},
			Writer:  buildWriter,
		})

		require.Equal(t, []RouteStatus{
			{Hostname: "x.nowhere.org", Owner: "qemu/1", State: StateNoZone, Reason: noZone},
		}, plan.Routes)
		require.Empty(t, plan.Tunnels)
		require.Empty(t, plan.Records)
	})

	t.Run("no zone and no address get no blocking rule", func(t *testing.T) {
		plan := Build(BuildInput{
			Winners: []model.Route{winner(t, "x.nowhere.org", "qemu/1")},
			Zones:   []Zone{shopZone},
			Writer:  buildWriter,
		})

		require.Equal(t, StateNoZone, plan.Routes[0].State)
		require.Empty(t, plan.Tunnels)
	})

	t.Run("a zone visible through several credentials is ambiguous", func(t *testing.T) {
		other := shopZone
		other.CredentialID = "cred9"

		plan := Build(BuildInput{
			Winners: []model.Route{winner(t, "a.shop.cz", "qemu/1"), winner(t, "*.shop.cz", "qemu/1")},
			Targets: allVerified("a.shop.cz", "*.shop.cz"),
			Zones:   []Zone{shopZone, other},
			Writer:  buildWriter,
		})

		const reason = "zone shop.cz is visible through several credentials; pin it to one"
		require.Equal(t, []RouteStatus{
			{Hostname: "*.shop.cz", Owner: "qemu/1", State: StateNoZone, Reason: reason},
			{Hostname: "a.shop.cz", Owner: "qemu/1", State: StateNoZone, Reason: reason},
		}, plan.Routes)
		require.Empty(t, plan.Tunnels)
		require.Empty(t, plan.Records)
	})

	t.Run("ambiguity of the longest zone does not fall back to a shorter one", func(t *testing.T) {
		parent := Zone{ID: "z-parent", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"}
		child := Zone{ID: "z-child", Name: "dev.example.com", AccountID: "acc1", CredentialID: "cred1"}
		childAgain := Zone{ID: "z-child2", Name: "dev.example.com", AccountID: "acc1", CredentialID: "cred2"}

		plan := Build(BuildInput{
			Winners: []model.Route{winner(t, "app.dev.example.com", "qemu/1"), winner(t, "www.example.com", "qemu/2")},
			Targets: allVerified("app.dev.example.com", "www.example.com"),
			Zones:   []Zone{parent, child, childAgain},
			Writer:  buildWriter,
		})

		require.Equal(t, StateNoZone, plan.Routes[0].State)
		require.Equal(t, "app.dev.example.com", plan.Routes[0].Hostname)
		require.Equal(t, StateActive, plan.Routes[1].State)
		require.Equal(t, []string{"www.example.com"}, recordNames(plan.Records))
	})

	t.Run("the same zone listed twice by one credential is not ambiguous", func(t *testing.T) {
		plan := Build(BuildInput{
			Winners: []model.Route{winner(t, "a.shop.cz", "qemu/1")},
			Targets: allVerified("a.shop.cz"),
			Zones:   []Zone{shopZone, shopZone},
			Writer:  buildWriter,
		})

		require.Equal(t, StateActive, plan.Routes[0].State)
		require.Equal(t, []RecordPlan{recordFor(shopZone, "a.shop.cz")}, plan.Records)
	})

	t.Run("the longest suffix picks the zone", func(t *testing.T) {
		parent := Zone{ID: "z-parent", Name: "example.com", AccountID: "acc1", CredentialID: "cred1"}
		child := Zone{ID: "z-child", Name: "dev.example.com", AccountID: "acc2", CredentialID: "cred2"}

		plan := Build(BuildInput{
			Winners: []model.Route{
				winner(t, "www.example.com", "qemu/1"),
				winner(t, "app.dev.example.com", "qemu/2"),
				winner(t, "*.dev.example.com", "qemu/2"),
			},
			Targets: allVerified("www.example.com", "app.dev.example.com", "*.dev.example.com"),
			Zones:   []Zone{parent, child},
			Writer:  buildWriter,
		})

		require.Equal(t, []RecordPlan{
			recordFor(child, "*.dev.example.com"),
			recordFor(child, "app.dev.example.com"),
			recordFor(parent, "www.example.com"),
		}, plan.Records)
		zones := make(map[string]string)
		for _, st := range plan.Routes {
			zones[st.Hostname] = st.Zone
		}
		require.Equal(t, map[string]string{
			"www.example.com":     "example.com",
			"app.dev.example.com": "dev.example.com",
			"*.dev.example.com":   "dev.example.com",
		}, zones)
		require.Equal(t, []string{"acc1", "acc2"}, []string{plan.Tunnels[0].AccountID, plan.Tunnels[1].AccountID})
	})
}

func TestBuildTunnelCredentialDoesNotDependOnZoneOrder(t *testing.T) {
	shop := Zone{ID: "z-shop", Name: "shop.cz", AccountID: "acc1", CredentialID: "cred-b"}
	example := Zone{ID: "z-example", Name: "example.com", AccountID: "acc1", CredentialID: "cred-a"}

	for _, zones := range [][]Zone{{shop, example}, {example, shop}} {
		plan := Build(BuildInput{
			Winners: []model.Route{winner(t, "a.shop.cz", "qemu/1")},
			Targets: allVerified("a.shop.cz"),
			Zones:   zones,
			Writer:  buildWriter,
		})

		require.Len(t, plan.Tunnels, 1)
		require.Equal(t, "cred-a", plan.Tunnels[0].CredentialID)
		require.Equal(t, "cred-b", plan.Records[0].CredentialID)
	}
}

func TestBuildWarnings(t *testing.T) {
	build := func(t *testing.T, winners ...model.Route) map[string][]string {
		t.Helper()
		targets := make(map[string]ResolvedTarget)
		for _, w := range winners {
			targets[w.Hostname] = verified(originAddr)
		}
		return warningsByHost(Build(BuildInput{
			Winners: winners, Targets: targets, Zones: []Zone{shopZone}, Writer: buildWriter,
		}))
	}

	t.Run("more than one level below the zone", func(t *testing.T) {
		got := build(t,
			winner(t, "shop.cz", "qemu/1"),
			winner(t, "www.shop.cz", "qemu/1"),
			winner(t, "*.shop.cz", "qemu/1"),
			winner(t, "deep.sub.shop.cz", "qemu/1"),
			winner(t, "*.sub.shop.cz", "qemu/1"),
		)

		require.Equal(t, map[string][]string{
			"shop.cz":          nil,
			"www.shop.cz":      nil,
			"*.shop.cz":        nil,
			"deep.sub.shop.cz": {deepWarning},
			"*.sub.shop.cz":    {deepWarning},
		}, got)
	})

	t.Run("a wildcard overlapping another owner warns on both", func(t *testing.T) {
		got := build(t,
			winner(t, "*.shop.cz", "qemu/1"),
			winner(t, "api.shop.cz", "qemu/2"),
			winner(t, "www.shop.cz", "manual/web"),
			winner(t, "mine.shop.cz", "qemu/1"),
		)

		require.Equal(t, map[string][]string{
			"*.shop.cz": {
				"*.shop.cz overlaps api.shop.cz owned by qemu/2",
				"*.shop.cz overlaps www.shop.cz owned by manual/web",
			},
			"api.shop.cz":  {"api.shop.cz overlaps *.shop.cz owned by qemu/1"},
			"www.shop.cz":  {"www.shop.cz overlaps *.shop.cz owned by qemu/1"},
			"mine.shop.cz": nil,
		}, got)
	})

	t.Run("a wildcard and an exact name of one owner do not warn", func(t *testing.T) {
		got := build(t,
			winner(t, "*.shop.cz", "manual/web"),
			winner(t, "api.shop.cz", "manual/web"),
		)

		require.Equal(t, map[string][]string{"*.shop.cz": nil, "api.shop.cz": nil}, got)
	})

	t.Run("a wildcard does not overlap the zone apex", func(t *testing.T) {
		got := build(t, winner(t, "*.shop.cz", "qemu/1"), winner(t, "shop.cz", "qemu/2"))

		require.Equal(t, map[string][]string{"*.shop.cz": nil, "shop.cz": nil}, got)
	})

	t.Run("wildcards overlap each other", func(t *testing.T) {
		got := build(t, winner(t, "*.shop.cz", "qemu/1"), winner(t, "*.a.shop.cz", "qemu/2"))

		require.Equal(t, map[string][]string{
			"*.shop.cz":   {"*.shop.cz overlaps *.a.shop.cz owned by qemu/2"},
			"*.a.shop.cz": {"*.a.shop.cz overlaps *.shop.cz owned by qemu/1", deepWarning},
		}, got)
	})

	t.Run("warnings are sorted", func(t *testing.T) {
		got := build(t, winner(t, "*.shop.cz", "qemu/1"), winner(t, "zz.a.shop.cz", "qemu/2"))

		require.Equal(t, []string{
			deepWarning,
			"zz.a.shop.cz overlaps *.shop.cz owned by qemu/1",
		}, got["zz.a.shop.cz"])
	})
}

func TestBuildConflicts(t *testing.T) {
	in := BuildInput{
		Winners: []model.Route{winner(t, "api.shop.cz", "qemu/1")},
		Conflicts: []model.Route{
			winner(t, "api.shop.cz", "manual/web"),
			winner(t, "api.shop.cz", "qemu/2"),
			winner(t, "reserved.shop.cz", "qemu/3"),
		},
		Holders: map[string]string{
			"api.shop.cz":      "qemu/1",
			"reserved.shop.cz": "qemu/9",
		},
		Targets: allVerified("api.shop.cz", "reserved.shop.cz"),
		Zones:   []Zone{shopZone},
		Writer:  buildWriter,
	}

	plan := Build(in)

	require.Equal(t, []RouteStatus{
		{
			Hostname: "api.shop.cz", Owner: "qemu/1", State: StateActive,
			Service: "http://10.20.0.15:8080", Zone: "shop.cz",
		},
		{Hostname: "api.shop.cz", Owner: "qemu/2", State: StateConflict, Reason: "hostname is held by qemu/1"},
		{Hostname: "api.shop.cz", Owner: "manual/web", State: StateConflict, Reason: "hostname is held by qemu/1"},
		{Hostname: "reserved.shop.cz", Owner: "qemu/3", State: StateConflict, Reason: "hostname is held by qemu/9"},
	}, plan.Routes)
	require.Equal(t, withSentinel(httpRule("api.shop.cz", originAddr), blockRule("reserved.shop.cz")), plan.Tunnels[0].Rules)
	require.Equal(t, []string{"api.shop.cz"}, recordNames(plan.Records))
}

func TestBuildConflictNamesTheServingOwner(t *testing.T) {
	plan := Build(BuildInput{
		Winners:   []model.Route{winner(t, "api.shop.cz", "qemu/1")},
		Conflicts: []model.Route{winner(t, "api.shop.cz", "qemu/2")},
		Holders:   map[string]string{"api.shop.cz": "qemu/9"},
		Targets:   allVerified("api.shop.cz"),
		Zones:     []Zone{shopZone},
		Writer:    buildWriter,
	})

	require.Equal(t, "hostname is held by qemu/1", plan.Routes[1].Reason)
}

func TestBuildHeldHostnameWithoutWinnerIsOnlyBlocked(t *testing.T) {
	plan := Build(BuildInput{
		Winners:   []model.Route{winner(t, "www.shop.cz", "qemu/3")},
		Conflicts: []model.Route{winner(t, "api.shop.cz", "qemu/2")},
		Holders:   map[string]string{"api.shop.cz": "qemu/1", "www.shop.cz": "qemu/3"},
		Targets:   allVerified("www.shop.cz"),
		Zones:     []Zone{shopZone},
		Writer:    buildWriter,
	})

	require.Equal(t, []TunnelPlan{{
		AccountID: "acc1", CredentialID: "cred1", Name: tunnelName,
		Rules: withSentinel(blockRule("api.shop.cz"), httpRule("www.shop.cz", originAddr)),
	}}, plan.Tunnels)
	require.Equal(t, []string{"www.shop.cz"}, recordNames(plan.Records))
	require.Equal(t, RouteStatus{
		Hostname: "api.shop.cz", Owner: "qemu/2", State: StateConflict, Reason: "hostname is held by qemu/1",
	}, plan.Routes[0])
}

func TestBuildEmpty(t *testing.T) {
	plan := Build(BuildInput{Zones: []Zone{shopZone}, Writer: buildWriter})

	require.Empty(t, plan.Tunnels)
	require.Empty(t, plan.Records)
	require.Empty(t, plan.Routes)
}

func TestBuildDeterministic(t *testing.T) {
	winners := []model.Route{
		winner(t, "*.shop.cz", "qemu/1"),
		winner(t, "api.shop.cz", "qemu/2"),
		winner(t, "shop.cz", "qemu/1"),
		winner(t, "deep.sub.shop.cz", "lxc/7"),
		winner(t, "www.example.com", "manual/web", withAddr("10.20.0.80")),
		winner(t, "*.example.com", "qemu/3"),
		winner(t, "down.example.com", "qemu/4"),
		winner(t, "gone.example.com", "qemu/5"),
		winner(t, "never.example.com", "qemu/6"),
		winner(t, "banned.example.com", "qemu/7"),
		winner(t, "x.nowhere.org", "qemu/8"),
		winner(t, "y.invalid", "qemu/11"),
	}
	conflicts := []model.Route{
		winner(t, "api.shop.cz", "qemu/9"),
		winner(t, "api.shop.cz", "lxc/2"),
		winner(t, "api.shop.cz", "manual/b"),
		winner(t, "www.example.com", "qemu/10"),
		winner(t, "held.example.com", "qemu/12"),
	}
	zones := []Zone{
		shopZone,
		exampleZone,
		{ID: "z-example-2", Name: "example.com", AccountID: "acc2", CredentialID: "cred2"},
		{ID: "z-other", Name: "other.net", AccountID: "acc2", CredentialID: "cred0"},
		{ID: "z-dev", Name: "dev.shop.cz", AccountID: "acc1", CredentialID: "cred1"},
	}
	in := BuildInput{
		Winners:   winners,
		Conflicts: conflicts,
		Holders: map[string]string{
			"api.shop.cz":       "qemu/2",
			"www.example.com":   "manual/web",
			"held.example.com":  "qemu/13",
			"held2.example.com": "qemu/14",
			"held.shop.cz":      "qemu/15",
		},
		Targets: map[string]ResolvedTarget{
			"*.shop.cz":          verified("10.20.0.1"),
			"api.shop.cz":        verified("10.20.0.2"),
			"shop.cz":            verified("10.20.0.3"),
			"deep.sub.shop.cz":   verified("10.20.0.4"),
			"*.example.com":      verified("10.20.0.5"),
			"down.example.com":   {Addr: netip.MustParseAddr("10.20.0.6")},
			"gone.example.com":   {Addr: netip.MustParseAddr("10.20.0.7"), Withdrawn: true},
			"banned.example.com": {Addr: netip.MustParseAddr("10.20.0.8"), Rejected: true},
			"www.example.com":    verified("10.20.0.80"),
		},
		Zones:  zones,
		Writer: buildWriter,
	}
	want := Build(in)
	require.NotEmpty(t, want.Tunnels)
	require.NotEmpty(t, want.Records)
	require.NotEmpty(t, want.Routes)

	rng := rand.New(rand.NewPCG(5, 8))
	for range 50 {
		shuffled := in
		shuffled.Winners = slices.Clone(winners)
		shuffled.Conflicts = slices.Clone(conflicts)
		shuffled.Zones = slices.Clone(zones)
		shuffle(rng, shuffled.Winners)
		shuffle(rng, shuffled.Conflicts)
		shuffle(rng, shuffled.Zones)

		require.Equal(t, want, Build(shuffled))
	}
}

func TestPlanJSONShape(t *testing.T) {
	plan := Build(BuildInput{
		Winners: []model.Route{winner(t, "api.shop.cz", "qemu/1"), winner(t, "x.nowhere.org", "lxc/2")},
		Targets: allVerified("api.shop.cz"),
		Zones:   []Zone{shopZone},
		Writer:  Writer{InstallID: "ab12", Generation: 0, Nonce: "n0"},
	})

	data, err := json.Marshal(plan)
	require.NoError(t, err)

	require.JSONEq(t, `{
		"tunnels": [{
			"accountId": "acc1",
			"credentialId": "cred1",
			"name": "pco-ab12",
			"rules": [
				{"hostname": "api.shop.cz", "service": "http://10.20.0.15:8080"},
				{"hostname": "g0.n0.pco-ab12.invalid", "service": "http_status:404"},
				{"service": "http_status:404"}
			]
		}],
		"records": [{
			"zoneId": "z-shop",
			"zoneName": "shop.cz",
			"accountId": "acc1",
			"credentialId": "cred1",
			"name": "api.shop.cz",
			"tunnelName": "pco-ab12"
		}],
		"routes": [
			{"hostname": "api.shop.cz", "owner": "qemu/1", "state": "active", "service": "http://10.20.0.15:8080", "zone": "shop.cz"},
			{"hostname": "x.nowhere.org", "owner": "lxc/2", "state": "no-zone", "reason": "no Cloudflare zone for this hostname in any credential"}
		]
	}`, string(data))
}

func TestPlannerTypesJSONShape(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "issue of a guest",
			value: Issue{Guest: ref(model.KindQEMU, 101), Line: 3, Col: 7, Msg: "bad"},
			want:  `{"guest": {"kind": "qemu", "vmid": 101}, "line": 3, "col": 7, "msg": "bad"}`,
		},
		{
			name:  "issue of the settings",
			value: Issue{Msg: "bad"},
			want:  `{"msg": "bad"}`,
		},
		{
			name:  "claim event",
			value: ClaimEvent{Kind: ClaimTaken, Hostname: "a.shop.cz", Owner: "qemu/1", Detail: "first claim on the hostname"},
			want:  `{"kind": "claimed", "hostname": "a.shop.cz", "owner": "qemu/1", "detail": "first claim on the hostname"}`,
		},
		{
			name:  "zone",
			value: shopZone,
			want:  `{"id": "z-shop", "name": "shop.cz", "accountId": "acc1", "credentialId": "cred1"}`,
		},
		{
			name:  "writer",
			value: Writer{InstallID: "ab12", Generation: 0, Nonce: "n0"},
			want:  `{"installId": "ab12", "generation": 0, "nonce": "n0"}`,
		},
		{
			name: "resolved target",
			value: ResolvedTarget{
				Addr: netip.MustParseAddr("10.0.0.5"), Reachable: true, Withdrawn: true, Rejected: true, Reason: "why", Owner: "qemu/1",
			},
			want: `{"addr": "10.0.0.5", "reachable": true, "withdrawn": true, "rejected": true, "reason": "why", "owner": "qemu/1"}`,
		},
		{
			name:  "held name",
			value: HeldName{Hostname: "a.shop.cz", Owner: "qemu/1"},
			want:  `{"hostname": "a.shop.cz", "owner": "qemu/1"}`,
		},
		{
			name:  "unresolved target",
			value: ResolvedTarget{},
			want:  `{}`,
		},
		{
			name: "route status with warnings",
			value: RouteStatus{
				Hostname: "a.shop.cz", Owner: "qemu/1", State: StateUnreachable, Reason: "why", Zone: "shop.cz", Warnings: []string{"w"},
			},
			want: `{"hostname": "a.shop.cz", "owner": "qemu/1", "state": "unreachable", "reason": "why", "zone": "shop.cz", "warnings": ["w"]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.value)
			require.NoError(t, err)
			require.JSONEq(t, tt.want, string(data))
		})
	}
}

func TestEmptyResultsMarshalAsEmptyLists(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "plan",
			value: Build(BuildInput{Writer: buildWriter}),
			want:  `{"tunnels": [], "records": [], "routes": []}`,
		},
		{
			name: "plan of an account with only blocks",
			value: Build(BuildInput{
				Holders: map[string]string{"held.shop.cz": "qemu/1"}, Zones: []Zone{shopZone}, Writer: buildWriter,
			}),
			want: `{"tunnels": [], "records": [], "routes": []}`,
		},
		{
			name:  "collected",
			value: Collect(nil, nil, Settings{}),
			want:  `{"Routes": [], "Issues": [], "Held": [], "PolicyInvalid": false}`,
		},
		{
			name:  "claim result",
			value: ResolveClaims(ClaimInput{Now: t0, Grace: grace}),
			want:  `{"Winners": [], "Conflicts": [], "Claims": {}, "Events": []}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.value)
			require.NoError(t, err)
			require.JSONEq(t, tt.want, string(data))
		})
	}
}

func shuffle[T any](rng *rand.Rand, s []T) {
	rng.Shuffle(len(s), func(a, b int) { s[a], s[b] = s[b], s[a] })
}
