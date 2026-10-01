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
		{"tag in another case", upperTag, Settings{}, []string{"app.example.com qemu/101"}},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := tagged(model.KindQEMU, 101, block(tt.host+" -> :80"))

			got := Collect([]model.Guest{g}, nil, tt.settings)

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
	require.Equal(t, []Issue{{Guest: g.Ref, Line: 2, Col: 1, Msg: notAllowed("secret.example.com")}}, got.Issues)
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := tagged(model.KindLXC, 200, tt.notes)

			got := Collect([]model.Guest{g}, nil, Settings{})

			require.Empty(t, got.Routes)
			require.Equal(t, []Issue{{Guest: g.Ref, Msg: msg}}, got.Issues)
		})
	}

	t.Run("an empty block is not missing", func(t *testing.T) {
		got := Collect([]model.Guest{tagged(model.KindLXC, 200, block())}, nil, Settings{})

		require.Empty(t, got.Routes)
		require.Empty(t, got.Issues)
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

	rng := rand.New(rand.NewPCG(1, 2))
	for range 30 {
		g := slices.Clone(guests)
		m := slices.Clone(manual)
		rng.Shuffle(len(g), func(i, j int) { g[i], g[j] = g[j], g[i] })
		rng.Shuffle(len(m), func(i, j int) { m[i], m[j] = m[j], m[i] })

		require.Equal(t, got, Collect(g, m, Settings{}))
	}
}
