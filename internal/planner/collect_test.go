package planner

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func block(lines ...string) string {
	return "```cf-tunnel\n" + strings.Join(lines, "\n") + "\n```"
}

func ref(kind model.GuestKind, vmid int) model.GuestRef {
	return model.GuestRef{Kind: kind, VMID: vmid}
}

// tagged is a guest that carries the default gate tag.
func tagged(kind model.GuestKind, vmid int, notes string) model.Guest {
	return model.Guest{
		Ref:         ref(kind, vmid),
		Name:        fmt.Sprintf("guest-%d", vmid),
		Tags:        []string{"cf-tunnel"},
		Description: notes,
	}
}

func notAllowed(host string) string {
	return fmt.Sprintf("hostname %q is not allowed by policy", host)
}

// routeKeys lists "hostname owner" for every route, in order.
func routeKeys(routes []model.Route) []string {
	var keys []string
	for _, r := range routes {
		keys = append(keys, r.Hostname+" "+r.Owner())
	}
	return keys
}

func TestCollectTagGate(t *testing.T) {
	notes := block("app.example.com -> :80")
	untagged := tagged(model.KindQEMU, 101, notes)
	untagged.Tags = []string{"prod"}
	template := tagged(model.KindQEMU, 101, notes)
	template.Template = true
	upperTag := tagged(model.KindQEMU, 101, notes)
	upperTag.Tags = []string{"CF-Tunnel"}
	custom := tagged(model.KindQEMU, 101, notes)
	custom.Tags = []string{"expose"}
	templateNoNotes := tagged(model.KindQEMU, 101, "")
	templateNoNotes.Template = true

	tests := []struct {
		name     string
		guest    model.Guest
		settings Settings
		want     []string
	}{
		{"tagged", tagged(model.KindQEMU, 101, notes), Settings{}, []string{"app.example.com qemu/101"}},
		{"tag in another case does not pass the gate", upperTag, Settings{}, nil},
		{"untagged", untagged, Settings{}, nil},
		{"template", template, Settings{}, nil},
		{"template without notes raises no issue", templateNoNotes, Settings{}, nil},
		{"custom gate tag", custom, Settings{GateTag: "expose"}, []string{"app.example.com qemu/101"}},
		{"default tag is ignored with a custom gate tag", tagged(model.KindQEMU, 101, notes), Settings{GateTag: "expose"}, nil},
		{"custom tag is ignored with the default gate tag", custom, Settings{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Collect([]model.Guest{tt.guest}, nil, tt.settings)
			require.Equal(t, tt.want, routeKeys(got.Routes))
			require.Empty(t, got.Issues)
		})
	}
}

func TestCollectExpandsEntriesIntoRoutes(t *testing.T) {
	notes := block(
		"app.example.com www.example.com -> https://:8443 no-tls-verify",
		"api.example.com -> 10.0.0.5:9000 host-header=api.internal",
	)
	guestRef := ref(model.KindQEMU, 101)

	got := Collect([]model.Guest{tagged(model.KindQEMU, 101, notes)}, nil, Settings{})

	require.Empty(t, got.Issues)
	require.Equal(t, []model.Route{
		{
			Hostname: "api.example.com",
			Target:   model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.0.0.5"), Port: 9000},
			Options:  model.RouteOptions{HostHeader: "api.internal"},
			Source:   model.SourceAnnotation,
			Guest:    &guestRef,
		},
		{
			Hostname: "app.example.com",
			Target:   model.Target{Scheme: model.SchemeHTTPS, Port: 8443},
			Options:  model.RouteOptions{NoTLSVerify: true},
			Source:   model.SourceAnnotation,
			Guest:    &guestRef,
		},
		{
			Hostname: "www.example.com",
			Target:   model.Target{Scheme: model.SchemeHTTPS, Port: 8443},
			Options:  model.RouteOptions{NoTLSVerify: true},
			Source:   model.SourceAnnotation,
			Guest:    &guestRef,
		},
	}, got.Routes)
}

func TestCollectParserErrorBecomesIssue(t *testing.T) {
	notes := block(
		"good.example.com -> :80",
		"bad.example.com -> :abc",
		"also.example.com -> :81",
	)
	g := tagged(model.KindLXC, 200, notes)

	got := Collect([]model.Guest{g}, nil, Settings{})

	require.Equal(t, []string{"also.example.com lxc/200", "good.example.com lxc/200"}, routeKeys(got.Routes))
	require.Equal(t, []Issue{{
		Guest: g.Ref,
		Line:  3,
		Col:   20,
		Msg:   `invalid target ":abc": expected [http|https://][ipv4]:port`,
	}}, got.Issues)
}

func TestCollectPolicy(t *testing.T) {
	tests := []struct {
		name     string
		settings Settings
		host     string
		allowed  bool
	}{
		{"no policy allows everything", Settings{}, "a.example.com", true},
		{"deny exact", Settings{DenyHosts: []string{"a.example.com"}}, "a.example.com", false},
		{"deny leaves other hosts", Settings{DenyHosts: []string{"a.example.com"}}, "b.example.com", true},
		{"deny wildcard", Settings{DenyHosts: []string{"*.internal.example.com"}}, "x.internal.example.com", false},
		{"deny wildcard covers depth", Settings{DenyHosts: []string{"*.example.com"}}, "x.y.example.com", false},
		{"deny wildcard leaves the apex", Settings{DenyHosts: []string{"*.example.com"}}, "example.com", true},
		{"deny everything", Settings{DenyHosts: []string{"*"}}, "a.example.com", false},
		{"allow hit", Settings{AllowHosts: []string{"*.example.com"}}, "a.example.com", true},
		{"allow miss", Settings{AllowHosts: []string{"*.example.com"}}, "a.other.org", false},
		{"allow exact", Settings{AllowHosts: []string{"a.example.com"}}, "a.example.com", true},
		{
			"deny beats allow",
			Settings{AllowHosts: []string{"*.example.com"}, DenyHosts: []string{"secret.example.com"}},
			"secret.example.com",
			false,
		},
		{
			"allow still applies next to deny",
			Settings{AllowHosts: []string{"*.example.com"}, DenyHosts: []string{"secret.example.com"}},
			"a.example.com",
			true,
		},
		{"allow pattern is lower-cased and loses its dot", Settings{AllowHosts: []string{"*.Example.COM."}}, "a.example.com", true},
		{"deny pattern is lower-cased and loses its dot", Settings{DenyHosts: []string{"Secret.Example.com."}}, "secret.example.com", false},
		{"allow pattern for a wildcard host", Settings{AllowHosts: []string{"*.example.com"}}, "*.example.com", true},
		{"deny pattern for a wildcard host", Settings{DenyHosts: []string{"*.example.com"}}, "*.example.com", false},
		{"allow a top level domain", Settings{AllowHosts: []string{"*.com"}}, "a.example.com", true},
		{"deny a top level domain", Settings{DenyHosts: []string{"*.com"}}, "a.example.com", false},
		{"deny a name blocks a wildcard that would serve it", Settings{DenyHosts: []string{"secret.example.com"}}, "*.example.com", false},
		{"deny a wildcard blocks a wider wildcard", Settings{DenyHosts: []string{"*.internal.example.com"}}, "*.example.com", false},
		{"deny star blocks a wildcard", Settings{DenyHosts: []string{"*"}}, "*.example.com", false},
		{"deny leaves a wildcard in another zone", Settings{DenyHosts: []string{"secret.example.com"}}, "*.other.com", true},
		{"deny leaves a wildcard below the denied name", Settings{DenyHosts: []string{"secret.example.com"}}, "*.secret.example.com", true},
		{
			"allow a name does not allow a wildcard over it",
			Settings{AllowHosts: []string{"a.example.com"}},
			"*.example.com",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := tagged(model.KindQEMU, 101, block(tt.host+" -> :80"))

			got := Collect([]model.Guest{g}, nil, tt.settings)

			require.False(t, got.PolicyInvalid)
			require.Empty(t, got.Held, "a valid policy decision holds nothing")
			if tt.allowed {
				require.Equal(t, []string{tt.host + " qemu/101"}, routeKeys(got.Routes))
				require.Empty(t, got.Issues)
				return
			}
			require.Empty(t, got.Routes)
			require.Equal(t, []Issue{{Guest: g.Ref, Line: 2, Col: 1, Msg: notAllowed(tt.host)}}, got.Issues)
		})
	}
}

func TestCollectPolicyFiltersHostsOfOneEntry(t *testing.T) {
	g := tagged(model.KindQEMU, 101, block("ok.example.com secret.example.com -> :80"))

	got := Collect([]model.Guest{g}, nil, Settings{DenyHosts: []string{"secret.example.com"}})

	require.Equal(t, []string{"ok.example.com qemu/101"}, routeKeys(got.Routes))
	require.Equal(t, []Issue{{Guest: g.Ref, Line: 2, Col: 16, Msg: notAllowed("secret.example.com")}}, got.Issues)
}

func TestCollectPolicyIssuePointsAtTheHost(t *testing.T) {
	g := tagged(model.KindQEMU, 101, block("ok.example.com", "  secret.example.com", "  -> :80"))

	got := Collect([]model.Guest{g}, nil, Settings{DenyHosts: []string{"secret.example.com"}})

	require.Equal(t, []Issue{{Guest: g.Ref, Line: 3, Col: 3, Msg: notAllowed("secret.example.com")}}, got.Issues)
}

func TestCollectInvalidPatterns(t *testing.T) {
	const host = "a.example.com"
	g := tagged(model.KindQEMU, 101, block(host+" -> :80"))
	hostIssue := Issue{Guest: g.Ref, Line: 2, Col: 1, Msg: notAllowed(host)}
	held := []HeldName{{Hostname: host, Owner: "qemu/101"}}
	denyIssue := func(pattern, reason string) Issue {
		return Issue{Msg: fmt.Sprintf("invalid deny pattern %q: %s; all hostnames are denied until it is fixed", pattern, reason)}
	}
	allowIssue := func(pattern, reason string) Issue {
		return Issue{Msg: fmt.Sprintf("invalid allow pattern %q: %s", pattern, reason)}
	}

	tests := []struct {
		name     string
		settings Settings
		routes   []string
		issues   []Issue
	}{
		{
			name:     "an invalid deny pattern denies everything",
			settings: Settings{DenyHosts: []string{"secret.example.com/"}},
			issues:   []Issue{hostIssue, denyIssue("secret.example.com/", `label "com/" contains '/'`)},
		},
		{
			name:     "an invalid deny pattern next to valid ones denies everything",
			settings: Settings{DenyHosts: []string{"other.example.com", " secret.example.com"}},
			issues:   []Issue{hostIssue, denyIssue(" secret.example.com", `label " secret" contains ' '`)},
		},
		{
			name:     "a star glued to a label is not a wildcard",
			settings: Settings{DenyHosts: []string{"*internal.example.com"}},
			issues:   []Issue{hostIssue, denyIssue("*internal.example.com", `label "*internal" contains '*'`)},
		},
		{
			name:     "an empty deny pattern denies everything",
			settings: Settings{DenyHosts: []string{""}},
			issues:   []Issue{hostIssue, denyIssue("", "empty")},
		},
		{
			name:     "an invalid allow pattern alone allows nothing",
			settings: Settings{AllowHosts: []string{"*.example.com "}},
			issues:   []Issue{hostIssue, allowIssue("*.example.com ", `label "com " contains ' '`)},
		},
		{
			name:     "an invalid allow pattern matches nothing next to a valid one",
			settings: Settings{AllowHosts: []string{"*.example.com", "a.example.com/"}},
			routes:   []string{host + " qemu/101"},
			issues:   []Issue{allowIssue("a.example.com/", `label "com/" contains '/'`)},
		},
		{
			name:     "every invalid pattern is reported",
			settings: Settings{AllowHosts: []string{"x y"}, DenyHosts: []string{"*.", "a b"}},
			issues: []Issue{
				hostIssue,
				denyIssue("*.", "a bare * takes no trailing dot"),
				denyIssue("a b", "needs at least two labels"),
				allowIssue("x y", "needs at least two labels"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Collect([]model.Guest{g}, nil, tt.settings)

			require.Equal(t, tt.routes, routeKeys(got.Routes))
			require.Equal(t, tt.issues, got.Issues)
			require.True(t, got.PolicyInvalid)
			if tt.routes == nil {
				require.Equal(t, held, got.Held, "a policy that cannot be read holds what it drops")
			} else {
				require.Empty(t, got.Held)
			}
		})
	}

	t.Run("one issue per run, not per guest", func(t *testing.T) {
		guests := []model.Guest{g, tagged(model.KindLXC, 200, block("b.example.com -> :80"))}

		got := Collect(guests, nil, Settings{DenyHosts: []string{"bad/"}})

		require.Empty(t, got.Routes)
		require.Len(t, got.Issues, 3)
		require.Equal(t, denyIssue("bad/", "needs at least two labels"), got.Issues[2])
		require.Equal(t, []HeldName{{Hostname: host, Owner: "qemu/101"}, {Hostname: "b.example.com", Owner: "lxc/200"}}, got.Held)
	})

	t.Run("reported without any guest", func(t *testing.T) {
		got := Collect(nil, nil, Settings{DenyHosts: []string{"bad/"}})

		require.Equal(t, []Issue{denyIssue("bad/", "needs at least two labels")}, got.Issues)
		require.True(t, got.PolicyInvalid)
	})

	t.Run("manual routes are not affected", func(t *testing.T) {
		manual := []model.Route{{Hostname: "grafana.example.com", Source: model.SourceManual, ManualID: "grafana"}}

		got := Collect(nil, manual, Settings{AllowHosts: []string{"bad/"}, DenyHosts: []string{"bad/"}})

		require.Equal(t, manual, got.Routes)
		require.Len(t, got.Issues, 2)
	})
}

func TestCollectHeld(t *testing.T) {
	const owner = "qemu/101"
	held := func(hosts ...string) []HeldName {
		out := []HeldName{}
		for _, h := range hosts {
			out = append(out, HeldName{Hostname: h, Owner: owner})
		}
		return out
	}

	tests := []struct {
		name     string
		notes    string
		settings Settings
		routes   []string
		held     []HeldName
	}{
		{
			name:   "a routed hostname is not held",
			notes:  block("a.example.com -> :80"),
			routes: []string{"a.example.com qemu/101"},
			held:   held(),
		},
		{
			name:   "an entry with a typo",
			notes:  block("a.example.com -> :80800", "b.example.com -> :80"),
			routes: []string{"b.example.com qemu/101"},
			held:   held("a.example.com"),
		},
		{
			name:  "hostnames in the text skipped after an error",
			notes: block("a_b.example.com c.example.com -> :80", "  d.example.com"),
			held:  held("c.example.com", "d.example.com"),
		},
		{
			name:  "a block with a stray fence",
			notes: "````cf-tunnel\na.example.com -> :80\n```\nb.example.com -> :81\n````",
			held:  held("a.example.com", "b.example.com"),
		},
		{
			name:  "a closing fence in a comment",
			notes: block("a.example.com -> :80 # was ```:81```"),
			held:  held("a.example.com"),
		},
		{
			name:  "a shorthand line with a fence",
			notes: "cf-tunnel: a.example.com -> :80 ```",
			held:  held("a.example.com"),
		},
		{
			name:   "a hostname listed twice keeps its first route",
			notes:  block("a.example.com -> :80", "a.example.com -> :81"),
			routes: []string{"a.example.com qemu/101"},
			held:   held(),
		},
		{
			name:     "a broken entry denied by a valid pattern",
			notes:    block("a.example.com -> :abc", "b.example.com -> :abc"),
			settings: Settings{DenyHosts: []string{"a.example.com"}},
			held:     held("b.example.com"),
		},
		{
			name:     "a valid entry denied by a valid pattern",
			notes:    block("a.example.com -> :80"),
			settings: Settings{DenyHosts: []string{"*.example.com"}},
			held:     held(),
		},
		{
			name:     "hostnames outside a valid allow list",
			notes:    block("a.example.com -> :80", "b.other.org -> :80", "c.other.org -> :abc"),
			settings: Settings{AllowHosts: []string{"*.example.com"}},
			routes:   []string{"a.example.com qemu/101"},
			held:     held(),
		},
		{
			name:     "an invalid deny pattern holds everything it drops",
			notes:    block("a.example.com -> :80", "b.example.com -> :abc"),
			settings: Settings{DenyHosts: []string{"a.example.com", "bad/"}},
			held:     held("a.example.com", "b.example.com"),
		},
		{
			name:     "an invalid allow pattern holds what the valid ones leave out",
			notes:    block("a.example.com -> :80", "b.other.org -> :80"),
			settings: Settings{AllowHosts: []string{"*.example.com", "*.other.org/"}},
			routes:   []string{"a.example.com qemu/101"},
			held:     held("b.other.org"),
		},
		{
			name:     "an invalid allow pattern holds what a valid deny pattern drops",
			notes:    block("a.example.com -> :80"),
			settings: Settings{AllowHosts: []string{"bad/"}, DenyHosts: []string{"a.example.com"}},
			held:     held("a.example.com"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Collect([]model.Guest{tagged(model.KindQEMU, 101, tt.notes)}, nil, tt.settings)

			require.Equal(t, tt.routes, routeKeys(got.Routes))
			require.Equal(t, tt.held, got.Held)
		})
	}

	t.Run("guests outside the gate hold nothing", func(t *testing.T) {
		notes := block("a.example.com -> :abc")
		untagged := tagged(model.KindQEMU, 101, notes)
		untagged.Tags = nil
		template := tagged(model.KindQEMU, 102, notes)
		template.Template = true

		got := Collect([]model.Guest{untagged, template}, nil, Settings{})

		require.Empty(t, got.Held)
	})

	t.Run("sorted by hostname, then owner", func(t *testing.T) {
		notes := block("b.example.com -> :x", "a.example.com -> :y")
		guests := []model.Guest{
			tagged(model.KindLXC, 5, notes),
			tagged(model.KindQEMU, 20, notes),
			tagged(model.KindQEMU, 3, notes),
		}

		got := Collect(guests, nil, Settings{})

		require.Equal(t, []HeldName{
			{Hostname: "a.example.com", Owner: "qemu/3"},
			{Hostname: "a.example.com", Owner: "qemu/20"},
			{Hostname: "a.example.com", Owner: "lxc/5"},
			{Hostname: "b.example.com", Owner: "qemu/3"},
			{Hostname: "b.example.com", Owner: "qemu/20"},
			{Hostname: "b.example.com", Owner: "lxc/5"},
		}, got.Held)

		rng := rand.New(rand.NewPCG(9, 4))
		for range 10 {
			g := slices.Clone(guests)
			rng.Shuffle(len(g), func(i, j int) { g[i], g[j] = g[j], g[i] })

			require.Equal(t, got, Collect(g, nil, Settings{}))
		}
	})
}

func TestCollectManualRouteWithoutID(t *testing.T) {
	guest := ref(model.KindQEMU, 101)
	grafana := model.Route{Hostname: "grafana.example.com", Source: model.SourceManual, ManualID: "grafana"}
	manual := []model.Route{
		{Hostname: "z.example.com", Source: model.SourceManual},
		grafana,
		// Without an id this would belong to the guest it names.
		{Hostname: "a.example.com", Source: model.SourceAnnotation, Guest: &guest},
	}

	got := Collect(nil, manual, Settings{})

	require.Equal(t, []model.Route{grafana}, got.Routes)
	require.Equal(t, []Issue{
		{Msg: `manual route for "a.example.com" has no id`},
		{Msg: `manual route for "z.example.com" has no id`},
	}, got.Issues)
	require.Empty(t, got.Held)

	rng := rand.New(rand.NewPCG(4, 9))
	for range 10 {
		m := slices.Clone(manual)
		rng.Shuffle(len(m), func(i, j int) { m[i], m[j] = m[j], m[i] })

		require.Equal(t, got, Collect(nil, m, Settings{}))
	}
}

func TestCollectTaggedWithoutRoutes(t *testing.T) {
	const msg = "tagged cf-tunnel but no routes found in Notes"
	tests := []struct {
		name  string
		notes string
	}{
		{"empty notes", ""},
		{"notes without a block", "web server for the shop"},
		{"block of another kind", "```yaml\nkey: value\n```"},
		{"empty block", block()},
		{"block with only comments", block("# app.example.com -> :80", "  # later")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := tagged(model.KindLXC, 200, tt.notes)

			got := Collect([]model.Guest{g}, nil, Settings{})

			require.Empty(t, got.Routes)
			require.Equal(t, []Issue{{Guest: g.Ref, Msg: msg}}, got.Issues)
		})
	}

	t.Run("the message names the configured gate tag", func(t *testing.T) {
		g := tagged(model.KindLXC, 200, "")
		g.Tags = []string{"expose"}

		got := Collect([]model.Guest{g}, nil, Settings{GateTag: "expose"})

		require.Equal(t, []Issue{{Guest: g.Ref, Msg: "tagged expose but no routes found in Notes"}}, got.Issues)
	})

	t.Run("parser errors are reported instead", func(t *testing.T) {
		g := tagged(model.KindLXC, 200, block("oops"))

		got := Collect([]model.Guest{g}, nil, Settings{})

		require.Equal(t, []Issue{{Guest: g.Ref, Line: 2, Col: 1, Msg: `invalid hostname "oops": needs at least two labels`}}, got.Issues)
	})

	t.Run("routes left out by policy are reported instead", func(t *testing.T) {
		g := tagged(model.KindLXC, 200, block("a.example.com -> :80"))

		got := Collect([]model.Guest{g}, nil, Settings{DenyHosts: []string{"*"}})

		require.Empty(t, got.Routes)
		require.Equal(t, []Issue{{Guest: g.Ref, Line: 2, Col: 1, Msg: notAllowed("a.example.com")}}, got.Issues)
	})
}

func TestCollectManualRoutesSkipPolicy(t *testing.T) {
	manual := []model.Route{{
		Hostname: "secret.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTPS, Addr: netip.MustParseAddr("10.0.0.9"), Port: 3000},
		Source:   model.SourceManual,
		ManualID: "grafana",
	}}
	s := Settings{AllowHosts: []string{"*.other.org"}, DenyHosts: []string{"secret.example.com"}}

	got := Collect(nil, manual, s)

	require.Equal(t, manual, got.Routes)
	require.Empty(t, got.Issues)
}

func TestCollectOrderDoesNotDependOnInput(t *testing.T) {
	guests := []model.Guest{
		tagged(model.KindLXC, 5, block("b.example.com -> :80", "a.example.com -> :80", "oops")),
		tagged(model.KindQEMU, 20, ""),
		tagged(model.KindQEMU, 3, block("bad.example.com -> :x", "a.example.com -> :82")),
	}
	manual := []model.Route{
		{Hostname: "a.example.com", Source: model.SourceManual, ManualID: "x"},
		{Hostname: "a.example.com", Source: model.SourceManual, ManualID: "a"},
	}

	got := Collect(guests, manual, Settings{})

	require.Equal(t, []string{
		"a.example.com qemu/3",
		"a.example.com lxc/5",
		"a.example.com manual/a",
		"a.example.com manual/x",
		"b.example.com lxc/5",
	}, routeKeys(got.Routes))
	var issues []string
	for _, is := range got.Issues {
		issues = append(issues, fmt.Sprintf("%s %d:%d", is.Guest, is.Line, is.Col))
	}
	require.Equal(t, []string{"qemu/3 2:20", "qemu/20 0:0", "lxc/5 4:1"}, issues)
	require.Equal(t, []HeldName{{Hostname: "bad.example.com", Owner: "qemu/3"}}, got.Held)

	rng := rand.New(rand.NewPCG(1, 2))
	for range 30 {
		g := slices.Clone(guests)
		m := slices.Clone(manual)
		rng.Shuffle(len(g), func(i, j int) { g[i], g[j] = g[j], g[i] })
		rng.Shuffle(len(m), func(i, j int) { m[i], m[j] = m[j], m[i] })

		require.Equal(t, got, Collect(g, m, Settings{}))
	}
}
