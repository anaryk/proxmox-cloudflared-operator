package annotation

import (
	"cmp"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func tgt(scheme model.Scheme, addr string, port uint16) model.Target {
	t := model.Target{Scheme: scheme, Port: port}
	if addr != "" {
		t.Addr = netip.MustParseAddr(addr)
	}
	return t
}

func http(port uint16) model.Target  { return tgt(model.SchemeHTTP, "", port) }
func https(port uint16) model.Target { return tgt(model.SchemeHTTPS, "", port) }

// e builds an Entry without positions; they are compared separately.
func e(hosts []string, target model.Target, opts model.RouteOptions) Entry {
	return Entry{Hosts: hosts, Target: target, Options: opts}
}

func hosts(h ...string) []string { return h }

func block(lines ...string) string {
	return "```cf-tunnel\n" + strings.Join(lines, "\n") + "\n```"
}

func withoutPositions(in []Entry) []Entry {
	var out []Entry
	for _, en := range in {
		en.Line, en.Col = 0, 0
		out = append(out, en)
	}
	return out
}

func messages(errs []Error) []string {
	var out []string
	for _, er := range errs {
		out = append(out, er.Msg)
	}
	return out
}

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		input string
		found bool
		want  []Entry
		errs  []string
	}{
		{name: "no annotation", input: "just some notes"},
		{name: "empty description", input: ""},
		{name: "empty block", input: "```cf-tunnel\n```", found: true},
		{
			name:  "basic",
			input: block("app.example.com -> :3000"),
			found: true,
			want:  []Entry{e(hosts("app.example.com"), http(3000), model.RouteOptions{})},
		},
		{
			name:  "https and option",
			input: block("api.example.com -> https://:8443 no-tls-verify"),
			found: true,
			want:  []Entry{e(hosts("api.example.com"), https(8443), model.RouteOptions{NoTLSVerify: true})},
		},
		{
			name:  "many hosts, spaces",
			input: block("shop.cz www.shop.cz *.shop.cz -> :80"),
			found: true,
			want:  []Entry{e(hosts("shop.cz", "www.shop.cz", "*.shop.cz"), http(80), model.RouteOptions{})},
		},
		{
			name:  "many hosts, commas",
			input: block("shop.cz, www.shop.cz,*.shop.cz -> :80"),
			found: true,
			want:  []Entry{e(hosts("shop.cz", "www.shop.cz", "*.shop.cz"), http(80), model.RouteOptions{})},
		},
		{
			name:  "explicit address",
			input: block("a.example.com -> 10.0.0.5:8080"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), tgt(model.SchemeHTTP, "10.0.0.5", 8080), model.RouteOptions{})},
		},
		{
			name:  "trailing slash",
			input: block("a.example.com -> http://:80/"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})},
		},
		{
			name:  "upper case",
			input: block("APP.Example.com -> HTTPS://:443 SNI=Origin.Internal"),
			found: true,
			want:  []Entry{e(hosts("app.example.com"), https(443), model.RouteOptions{SNI: "origin.internal"})},
		},
		{
			name:  "upper case option keys",
			input: block("a.example.com -> https://:443 NO-TLS-VERIFY Host-Header=Internal.LAN"),
			found: true,
			want: []Entry{e(hosts("a.example.com"), https(443),
				model.RouteOptions{NoTLSVerify: true, HostHeader: "Internal.LAN"})},
		},
		{
			name:  "via nic",
			input: block("a.example.com -> :80 via=net1"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{Via: "net1"})},
		},
		{
			name:  "via nic upper case",
			input: block("a.example.com -> :80 VIA=NET12"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{Via: "net12"})},
		},
		{
			name:  "via ip",
			input: block("a.example.com -> :80 via=192.168.1.5"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{Via: "192.168.1.5"})},
		},
		{
			name:  "host-header",
			input: block("a.example.com -> :80 host-header=internal.lan"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{HostHeader: "internal.lan"})},
		},
		{
			name:  "host-header with port stays verbatim",
			input: block("a.example.com -> :80 host-header=Internal.LAN:8080"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{HostHeader: "Internal.LAN:8080"})},
		},
		{
			name:  "comments",
			input: block("# full line", "a.example.com -> :80 # trailing"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})},
		},
		{
			name:  "comment after a comma",
			input: block("a.example.com,# b.example.com", "-> :80"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})},
		},
		{
			name:  "hash inside token",
			input: block("a.example.com -> :80 host-header=a#b"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{HostHeader: "a#b"})},
		},
		{
			name:  "shorthand",
			input: "notes\ncf-tunnel: app.example.com -> :3000\nmore",
			found: true,
			want:  []Entry{e(hosts("app.example.com"), http(3000), model.RouteOptions{})},
		},
		{
			name:  "shorthand upper",
			input: "CF-Tunnel: app.example.com -> :3000",
			found: true,
			want:  []Entry{e(hosts("app.example.com"), http(3000), model.RouteOptions{})},
		},
		{
			name:  "shorthand indented with comment",
			input: "  \t cf-tunnel: app.example.com -> :3000 # why not",
			found: true,
			want:  []Entry{e(hosts("app.example.com"), http(3000), model.RouteOptions{})},
		},
		{
			name:  "shorthand lines are separate entries",
			input: "cf-tunnel: a.example.com -> :80\ncf-tunnel: b.example.com -> :81",
			found: true,
			want: []Entry{
				e(hosts("a.example.com"), http(80), model.RouteOptions{}),
				e(hosts("b.example.com"), http(81), model.RouteOptions{}),
			},
		},
		{
			name:  "shorthand and block together",
			input: "cf-tunnel: a.example.com -> :80\n" + block("b.example.com -> :81"),
			found: true,
			want: []Entry{
				e(hosts("a.example.com"), http(80), model.RouteOptions{}),
				e(hosts("b.example.com"), http(81), model.RouteOptions{}),
			},
		},
		{
			name:  "shorthand not at line start of text",
			input: "see cf-tunnel: a.example.com -> :80",
		},
		{
			name:  "two blocks",
			input: block("a.example.com -> :80") + "\n\n" + block("b.example.com -> :81"),
			found: true,
			want: []Entry{
				e(hosts("a.example.com"), http(80), model.RouteOptions{}),
				e(hosts("b.example.com"), http(81), model.RouteOptions{}),
			},
		},
		{
			name:  "text around",
			input: "Runs the shop.\n\n" + block("a.example.com -> :80") + "\n\nOwner: someone, a.example.org -> :99",
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})},
		},
		{
			name:  "tag is case-insensitive",
			input: "```CF-Tunnel\na.example.com -> :80\n```",
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})},
		},
		{
			name:  "tag must end at whitespace",
			input: "```cf-tunnels\na.example.com -> :80\n```",
		},
		{
			name:  "entry spans lines",
			input: block("a.example.com", "  b.example.com", "  -> :80"),
			found: true,
			want:  []Entry{e(hosts("a.example.com", "b.example.com"), http(80), model.RouteOptions{})},
		},
		{
			name:  "options on later lines",
			input: block("a.example.com -> https://:443", "  no-tls-verify", "  sni=x.example.com", "b.example.com -> :81"),
			found: true,
			want: []Entry{
				e(hosts("a.example.com"), https(443), model.RouteOptions{NoTLSVerify: true, SNI: "x.example.com"}),
				e(hosts("b.example.com"), http(81), model.RouteOptions{}),
			},
		},
		{
			name:  "unterminated block",
			input: "```cf-tunnel\na.example.com -> :80",
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})},
		},
		{
			name:  "other fence ignored",
			input: "```yaml\na.example.com -> :80\n```",
		},
		{
			name:  "missing arrow",
			input: block("a.example.com :80"),
			found: true,
			errs:  []string{"expected '->' after hostnames"},
		},
		{
			name:  "hosts without anything else",
			input: block("a.example.com"),
			found: true,
			errs:  []string{"expected '->' after hostnames"},
		},
		{
			name:  "no host",
			input: block("-> :80"),
			found: true,
			errs:  []string{"expected a hostname before '->'"},
		},
		{
			name:  "bad host",
			input: block("a_b.example.com -> :80"),
			found: true,
			errs:  []string{`invalid hostname "a_b.example.com": label "a_b" contains '_'`},
		},
		{
			name:  "bad host in a list",
			input: block("a.example.com b_c.example.com d.example.com -> :80"),
			found: true,
			errs:  []string{`invalid hostname "b_c.example.com": label "b_c" contains '_'`},
		},
		{
			name:  "target missing",
			input: block("a.example.com ->"),
			found: true,
			errs:  []string{`invalid target "": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "bad port zero",
			input: block("a.example.com -> :0"),
			found: true,
			errs:  []string{`invalid target ":0": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "bad port too large",
			input: block("a.example.com -> :70000"),
			found: true,
			errs:  []string{`invalid target ":70000": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "bad port letters",
			input: block("a.example.com -> :abc"),
			found: true,
			errs:  []string{`invalid target ":abc": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "bad port signed",
			input: block("a.example.com -> :+80"),
			found: true,
			errs:  []string{`invalid target ":+80": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "target without port",
			input: block("a.example.com -> 10.0.0.5"),
			found: true,
			errs:  []string{`invalid target "10.0.0.5": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "target is a hostname",
			input: block("a.example.com -> b.example.com:80"),
			found: true,
			errs:  []string{`invalid target "b.example.com:80": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "unknown scheme",
			input: block("a.example.com -> ftp://:80"),
			found: true,
			errs:  []string{`invalid target "ftp://:80": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "path",
			input: block("a.example.com -> :80/admin"),
			found: true,
			errs:  []string{`invalid target ":80/admin": paths are not supported`},
		},
		{
			name:  "ipv6",
			input: block("a.example.com -> [::1]:80"),
			found: true,
			errs:  []string{`invalid target "[::1]:80": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "ipv4 mapped ipv6",
			input: block("a.example.com -> ::ffff:10.0.0.5:80"),
			found: true,
			errs:  []string{`invalid target "::ffff:10.0.0.5:80": expected [http|https://][ipv4]:port`},
		},
		{
			name:  "unknown option",
			input: block("a.example.com -> :80 fast"),
			found: true,
			errs:  []string{`unknown option "fast"`},
		},
		{
			name:  "unknown key",
			input: block("a.example.com -> :80 retries=3"),
			found: true,
			errs:  []string{`unknown option "retries=3"`},
		},
		{
			name:  "flag given a value",
			input: block("a.example.com -> https://:443 no-tls-verify=true"),
			found: true,
			errs:  []string{`unknown option "no-tls-verify=true"`},
		},
		{
			name:  "valued option without value sign",
			input: block("a.example.com -> :80 via"),
			found: true,
			errs:  []string{`unknown option "via"`},
		},
		{
			name:  "option twice",
			input: block("a.example.com -> https://:443 sni=a.b sni=c.d"),
			found: true,
			errs:  []string{`option "sni=" given twice`},
		},
		{
			name:  "flag twice",
			input: block("a.example.com -> https://:443 no-tls-verify NO-TLS-VERIFY"),
			found: true,
			errs:  []string{`option "no-tls-verify" given twice`},
		},
		{
			name:  "via plus address",
			input: block("a.example.com -> 10.0.0.5:80 via=net0"),
			found: true,
			errs:  []string{"via= cannot be combined with an address in the target"},
		},
		{
			name:  "no-tls-verify on http",
			input: block("a.example.com -> :80 no-tls-verify"),
			found: true,
			errs:  []string{"no-tls-verify only applies to https targets"},
		},
		{
			name:  "sni on http",
			input: block("a.example.com -> :80 sni=x.example.com"),
			found: true,
			errs:  []string{"sni= only applies to https targets"},
		},
		{
			name:  "sni invalid",
			input: block("a.example.com -> https://:443 sni=not_a_host"),
			found: true,
			errs:  []string{"invalid value for sni="},
		},
		{
			name:  "sni wildcard",
			input: block("a.example.com -> https://:443 sni=*.example.com"),
			found: true,
			errs:  []string{"invalid value for sni="},
		},
		{
			name:  "via invalid",
			input: block("a.example.com -> :80 via=net"),
			found: true,
			errs:  []string{"invalid value for via="},
		},
		{
			name:  "via ipv6",
			input: block("a.example.com -> :80 via=::1"),
			found: true,
			errs:  []string{"invalid value for via="},
		},
		{
			name:  "host-header empty",
			input: block("a.example.com -> :80 host-header="),
			found: true,
			errs:  []string{"invalid value for host-header="},
		},
		{
			name:  "duplicate host",
			input: block("A.example.com -> :80", "a.example.com. -> :81"),
			found: true,
			want:  []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})},
			errs:  []string{`hostname "a.example.com" is listed twice`},
		},
		{
			name: "recovery",
			input: block(
				"a_b.example.com -> :80",
				"ok1.example.com -> :81",
				"bad.example.com -> :abc",
				"ok2.example.com -> :82",
			),
			found: true,
			want: []Entry{
				e(hosts("ok1.example.com"), http(81), model.RouteOptions{}),
				e(hosts("ok2.example.com"), http(82), model.RouteOptions{}),
			},
			errs: []string{
				`invalid hostname "a_b.example.com": label "a_b" contains '_'`,
				`invalid target ":abc": expected [http|https://][ipv4]:port`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.input)
			require.Equal(t, tt.found, got.Found)
			require.Equal(t, tt.want, withoutPositions(got.Entries))
			require.Equal(t, tt.errs, messages(got.Errors))
		})
	}
}

func TestParseCRLFAndTabs(t *testing.T) {
	lf := "notes\n```cf-tunnel\n\ta.example.com\t-> :80\n\tb.example.com -> https://:443\tno-tls-verify # c\n```\nmore"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	want := []Entry{
		{
			Hosts: hosts("a.example.com"), Target: http(80),
			Line: 3, Col: 2,
		},
		{
			Hosts: hosts("b.example.com"), Target: https(443),
			Options: model.RouteOptions{NoTLSVerify: true},
			Line:    4, Col: 2,
		},
	}

	for name, input := range map[string]string{"lf": lf, "crlf": crlf} {
		t.Run(name, func(t *testing.T) {
			got := Parse(input)
			require.True(t, got.Found)
			require.Empty(t, got.Errors)
			require.Equal(t, want, got.Entries)
		})
	}
}

func TestParseCRLFErrorPosition(t *testing.T) {
	input := "one\r\ntwo\r\n```cf-tunnel\r\n\r\n  a.example.com -> :80\r\n  b_c.example.com -> :81\r\n```"
	got := Parse(input)
	require.Len(t, got.Entries, 1)
	require.Len(t, got.Errors, 1)
	require.Equal(t, 6, got.Errors[0].Line)
	require.Equal(t, 3, got.Errors[0].Col)
}

func TestParseFlattenedDescription(t *testing.T) {
	t.Run("scheme rule applies to the second entry", func(t *testing.T) {
		got := Parse("```cf-tunnel a.example.com -> :80 b.example.com -> :81 no-tls-verify ```")
		require.True(t, got.Found)
		require.Equal(t, []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})}, withoutPositions(got.Entries))
		require.Equal(t, []string{"no-tls-verify only applies to https targets"}, messages(got.Errors))
		require.Equal(t, Error{Line: 1, Col: 56, Msg: "no-tls-verify only applies to https targets"}, got.Errors[0])
	})

	t.Run("two valid entries", func(t *testing.T) {
		got := Parse("```cf-tunnel a.example.com -> :80 b.example.com -> https://:81 no-tls-verify ```")
		require.True(t, got.Found)
		require.Empty(t, got.Errors)
		require.Equal(t, []Entry{
			{Hosts: hosts("a.example.com"), Target: http(80), Line: 1, Col: 14},
			{
				Hosts: hosts("b.example.com"), Target: https(81),
				Options: model.RouteOptions{NoTLSVerify: true},
				Line:    1, Col: 35,
			},
		}, got.Entries)
	})

	t.Run("prose before and after", func(t *testing.T) {
		got := Parse("Shop VM. ```cf-tunnel a.example.com -> :80``` Thanks!")
		require.True(t, got.Found)
		require.Empty(t, got.Errors)
		require.Equal(t, []Entry{e(hosts("a.example.com"), http(80), model.RouteOptions{})}, withoutPositions(got.Entries))
	})
}

func TestParseDuplicateAfterNormalisation(t *testing.T) {
	input := block("A.example.com -> :80", "a.example.com. -> :81")
	got := Parse(input)
	require.Equal(t, []Entry{{Hosts: hosts("a.example.com"), Target: http(80), Line: 2, Col: 1}}, got.Entries)
	require.Equal(t, []Error{{Line: 3, Col: 1, Msg: `hostname "a.example.com" is listed twice`}}, got.Errors)
}

func TestParseDuplicateHosts(t *testing.T) {
	t.Run("the whole second entry is dropped", func(t *testing.T) {
		input := block(
			"a.example.com -> :80",
			"b.example.com a.example.com c.example.com -> :81",
			"c.example.com -> :82",
		)
		got := Parse(input)
		require.Equal(t, []Entry{
			e(hosts("a.example.com"), http(80), model.RouteOptions{}),
			e(hosts("c.example.com"), http(82), model.RouteOptions{}),
		}, withoutPositions(got.Entries))
		require.Equal(t, []Error{{Line: 3, Col: 15, Msg: `hostname "a.example.com" is listed twice`}}, got.Errors)
	})

	t.Run("within one entry", func(t *testing.T) {
		got := Parse(block("a.example.com A.EXAMPLE.COM -> :80"))
		require.Empty(t, got.Entries)
		require.Equal(t, []Error{{Line: 2, Col: 15, Msg: `hostname "a.example.com" is listed twice`}}, got.Errors)
	})

	t.Run("across blocks and shorthand", func(t *testing.T) {
		input := block("a.example.com -> :80") + "\ncf-tunnel: a.example.com -> :81\n" + block("a.example.com -> :82")
		got := Parse(input)
		require.Len(t, got.Entries, 1)
		require.Equal(t, []Error{
			{Line: 4, Col: 12, Msg: `hostname "a.example.com" is listed twice`},
			{Line: 6, Col: 1, Msg: `hostname "a.example.com" is listed twice`},
		}, got.Errors)
	})

	t.Run("wildcard and exact names are different", func(t *testing.T) {
		got := Parse(block("*.example.com -> :80", "example.com -> :81"))
		require.Len(t, got.Entries, 2)
		require.Empty(t, got.Errors)
	})
}

func TestParsePositions(t *testing.T) {
	t.Run("shorthand", func(t *testing.T) {
		got := Parse("notes\n  cf-tunnel: app.example.com -> :3000\nmore")
		require.Equal(t, []Entry{
			{Hosts: hosts("app.example.com"), Target: http(3000), Line: 2, Col: 14},
		}, got.Entries)
	})

	t.Run("entry that spans lines starts at its first host", func(t *testing.T) {
		got := Parse(block("# note", "  a.example.com", "    b.example.com", "    -> :80"))
		require.Equal(t, []Entry{
			{Hosts: hosts("a.example.com", "b.example.com"), Target: http(80), Line: 3, Col: 3},
		}, got.Entries)
	})

	t.Run("columns count bytes", func(t *testing.T) {
		got := Parse(block("a.example.com -> :80 host-header=\u00e9 fast"))
		require.Empty(t, got.Entries)
		require.Equal(t, []Error{{Line: 2, Col: 37, Msg: `unknown option "fast"`}}, got.Errors)
	})
}

func TestParseErrorPosition(t *testing.T) {
	input := "notes\n\n```cf-tunnel\na.example.com -> :80\n    a_b.example.com -> :80\n```"
	got := Parse(input)
	require.Equal(t, []Error{
		{Line: 5, Col: 5, Msg: `invalid hostname "a_b.example.com": label "a_b" contains '_'`},
	}, got.Errors)

	input = "notes\n\n```cf-tunnel\n    a_b.example.com -> :80\n```"
	got = Parse(input)
	require.Equal(t, 4, got.Errors[0].Line)
	require.Equal(t, 5, got.Errors[0].Col)
}

func TestErrorString(t *testing.T) {
	err := Error{Line: 3, Col: 5, Msg: "expected '->' after hostnames"}
	require.EqualError(t, err, "line 3, col 5: expected '->' after hostnames")
}

func TestParseErrorPlacement(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Error
	}{
		{"arrow without hosts points at the arrow", block("  -> :80"), Error{2, 3, "expected a hostname before '->'"}},
		{"missing arrow points at the stray token", block("a.example.com :80"), Error{2, 15, "expected '->' after hostnames"}},
		{"missing arrow at the end points at the last host", block("a.example.com b.example.com"), Error{2, 15, "expected '->' after hostnames"}},
		{"missing target points at the arrow", block("a.example.com   ->"), Error{2, 17, `invalid target "": expected [http|https://][ipv4]:port`}},
		{"bad target points at the target", block("a.example.com ->   :0"), Error{2, 20, `invalid target ":0": expected [http|https://][ipv4]:port`}},
		{"unknown option points at the option", block("a.example.com -> :80  fast"), Error{2, 23, `unknown option "fast"`}},
		{"second option points at itself", block("a.example.com -> https://:443 sni=a.b sni=c.d"), Error{2, 39, `option "sni=" given twice`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.input)
			require.Equal(t, []Error{tt.want}, got.Errors)
			require.Empty(t, got.Entries)
		})
	}
}

func TestParseRecoveryResumesOnALaterLine(t *testing.T) {
	t.Run("the rest of the broken line is skipped", func(t *testing.T) {
		got := Parse(block(
			"a_b.example.com -> :80 c.example.com -> :81",
			"d.example.com -> :82",
		))
		require.Equal(t, []Entry{e(hosts("d.example.com"), http(82), model.RouteOptions{})}, withoutPositions(got.Entries))
		require.Len(t, got.Errors, 1)
	})

	t.Run("an error on a continuation line skips that line", func(t *testing.T) {
		got := Parse(block(
			"a.example.com",
			"  -> :80 fast b.example.com -> :81",
			"c.example.com -> :82",
		))
		require.Equal(t, []Entry{e(hosts("c.example.com"), http(82), model.RouteOptions{})}, withoutPositions(got.Entries))
		require.Equal(t, []Error{{Line: 3, Col: 10, Msg: `unknown option "fast"`}}, got.Errors)
	})

	t.Run("a broken entry does not take the previous one with it", func(t *testing.T) {
		got := Parse(block(
			"a.example.com -> :80",
			"b_c.example.com -> :81",
			"d.example.com -> :82",
		))
		require.Equal(t, []Entry{
			e(hosts("a.example.com"), http(80), model.RouteOptions{}),
			e(hosts("d.example.com"), http(82), model.RouteOptions{}),
		}, withoutPositions(got.Entries))
		require.Equal(t, []Error{
			{Line: 3, Col: 1, Msg: `invalid hostname "b_c.example.com": label "b_c" contains '_'`},
		}, got.Errors)
	})

	t.Run("errors keep their line numbers across blocks", func(t *testing.T) {
		input := block("a_b.example.com -> :80") + "\n" + block("c.example.com -> :81", "d_e.example.com -> :82")
		got := Parse(input)
		require.Len(t, got.Entries, 1)
		require.Equal(t, []int{2, 6}, []int{got.Errors[0].Line, got.Errors[1].Line})
	})

	t.Run("blocks do not continue each other", func(t *testing.T) {
		got := Parse(block("a.example.com") + "\n" + block("-> :80"))
		require.Empty(t, got.Entries)
		require.Equal(t, []string{
			"expected '->' after hostnames",
			"expected a hostname before '->'",
		}, messages(got.Errors))
	})

	t.Run("shorthand lines do not continue each other", func(t *testing.T) {
		got := Parse("cf-tunnel: a.example.com\ncf-tunnel: -> :80")
		require.Empty(t, got.Entries)
		require.Len(t, got.Errors, 2)
	})
}

func TestParseRecoveryErrorLines(t *testing.T) {
	input := strings.Join([]string{
		"intro",
		"```cf-tunnel",
		"broken_1.example.com -> :80",
		"ok1.example.com -> :81",
		"ok3.example.com -> :abc",
		"ok2.example.com -> :82",
		"```",
	}, "\n")
	got := Parse(input)
	require.Len(t, got.Entries, 2)
	require.Len(t, got.Errors, 2)
	require.Equal(t, 3, got.Errors[0].Line)
	require.Equal(t, 5, got.Errors[1].Line)
	require.Equal(t, 4, got.Entries[0].Line)
	require.Equal(t, 6, got.Entries[1].Line)
}

func TestParseAcceptsHostsExactlyAsHostnameDoes(t *testing.T) {
	for _, h := range []string{"app.example.com", "*.example.com", "XN--bcher-kva.de", "example.com."} {
		got := Parse(block(h + " -> :80"))
		want, err := hostname.Normalize(h)
		require.NoError(t, err)
		require.Equal(t, []string{want}, got.Entries[0].Hosts)
	}
}

func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"just some notes",
		"```cf-tunnel\n```",
		block("app.example.com -> :3000"),
		block("api.example.com -> https://:8443 no-tls-verify"),
		block("shop.cz, www.shop.cz,*.shop.cz -> 10.0.0.5:80 host-header=a#b # c"),
		block("a.example.com -> https://:443 sni=Origin.Internal", "b.example.com -> :80 via=net1"),
		block("a.example.com -> :80 via=192.168.1.5", "a.example.com -> :81"),
		"notes\r\ncf-tunnel: app.example.com -> :3000\r\nmore",
		"```cf-tunnel a.example.com -> :80 b.example.com -> https://:81 no-tls-verify ```",
		"```cf-tunnel\na.example.com\n  b.example.com\n  -> :80",
		block("-> :80", "a.example.com :80", "a_b.example.com -> :0", "a.example.com -> :80/x", "a.example.com -> [::1]:80"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, description string) {
		res := Parse(description)
		for _, en := range res.Entries {
			require.NotEmpty(t, en.Hosts)
			require.NotZero(t, en.Target.Port)
			require.Contains(t, []model.Scheme{model.SchemeHTTP, model.SchemeHTTPS}, en.Target.Scheme)
			require.GreaterOrEqual(t, en.Line, 1)
			require.GreaterOrEqual(t, en.Col, 1)
			for _, h := range en.Hosts {
				norm, err := hostname.Normalize(h)
				require.NoError(t, err)
				require.Equal(t, norm, h)
			}
		}
		for _, er := range res.Errors {
			require.GreaterOrEqual(t, er.Line, 1)
			require.GreaterOrEqual(t, er.Col, 1)
			require.NotEmpty(t, er.Msg)
		}
		for i := 1; i < len(res.Entries); i++ {
			prev, cur := res.Entries[i-1], res.Entries[i]
			require.Negative(t, cmp.Or(cmp.Compare(prev.Line, cur.Line), cmp.Compare(prev.Col, cur.Col)), "entries out of order")
		}
		for i := 1; i < len(res.Errors); i++ {
			prev, cur := res.Errors[i-1], res.Errors[i]
			require.Negative(t, cmp.Or(cmp.Compare(prev.Line, cur.Line), cmp.Compare(prev.Col, cur.Col)), "errors out of order")
		}
		if !res.Found {
			require.Empty(t, res.Entries)
			require.Empty(t, res.Errors)
		}
	})
}
