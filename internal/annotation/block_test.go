package annotation

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestTheBlockIsTheRouteTextAndNothingElse(t *testing.T) {
	for _, tt := range []struct {
		name, notes string
		block       string
		line        int
	}{
		{"none", "a web server\nowned by alice", "", 0},
		{"empty notes", "", "", 0},
		{
			"a fenced block among prose",
			"Web server\nowned by alice\n\n```cf-tunnel\napp.example.com -> :3000\n```\nbackups nightly",
			"```cf-tunnel\napp.example.com -> :3000\n```", 4,
		},
		{
			"shorthand lines, with the prose between them left out",
			"cf-tunnel: a.example.com -> :80\nsecret: hunter2\n  cf-tunnel: b.example.com -> :81\n",
			"cf-tunnel: a.example.com -> :80\n\n  cf-tunnel: b.example.com -> :81", 1,
		},
		{
			"a block that opens mid-line keeps its columns",
			"notes ```cf-tunnel a.example.com -> :80``` after",
			"      ```cf-tunnel a.example.com -> :80```", 1,
		},
		{
			"another kind of block is not route text",
			"```sh\ncf-tunnel: x.example.com -> :1\n```\ncf-tunnel: y.example.com -> :2",
			"cf-tunnel: y.example.com -> :2", 4,
		},
		{
			"a block that is never closed runs to the end",
			"intro\n```cf-tunnel\na.example.com -> :80\n",
			"```cf-tunnel\na.example.com -> :80\n", 2,
		},
		{
			"a rejected block is route text too",
			"````cf-tunnel\na.example.com -> :80 ```x``` \n````",
			"````cf-tunnel\na.example.com -> :80 ```x``` \n````", 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			block, line := Block(tt.notes)

			require.Equal(t, tt.block, block)
			require.Equal(t, tt.line, line)
		})
	}
}

// The positions of the errors of Parse point at the same text in the block,
// counted from the line it begins on.
func TestTheErrorsOfParsePointIntoTheBlock(t *testing.T) {
	notes := "Web server\n\nnotes ```cf-tunnel a.example.com -> :80\n  b.example.com -> :x\n```\nmore prose\ncf-tunnel: c.example.com"
	block, line := Block(notes)
	errs := Parse(notes).Errors
	require.NotEmpty(t, errs)
	lines := strings.Split(block, "\n")
	for _, er := range errs {
		src := strings.Split(notes, "\n")[er.Line-1]
		got := lines[er.Line-line]
		require.Equal(t, []rune(src)[er.Col-1:], []rune(got)[er.Col-1:], "%s", er)
	}
}

func TestOptionsOfARouteWrittenElsewhereFollowTheNotes(t *testing.T) {
	secure := model.Target{Scheme: model.SchemeHTTPS, Port: 443}
	plain := model.Target{Scheme: model.SchemeHTTP, Port: 80}
	addr := plain
	addr.Addr = netip.MustParseAddr("10.0.5.20")

	got, err := CheckOptions(secure, model.RouteOptions{SNI: "App.Example.COM", Via: "NET1", HostHeader: "app:8443", NoTLSVerify: true, AllowNode: true})
	require.NoError(t, err)
	require.Equal(t, model.RouteOptions{SNI: "app.example.com", Via: "net1", HostHeader: "app:8443", NoTLSVerify: true, AllowNode: true}, got)

	for _, tt := range []struct {
		name   string
		target model.Target
		opts   model.RouteOptions
		option string
	}{
		{"no-tls-verify on http", plain, model.RouteOptions{NoTLSVerify: true}, "noTLSVerify"},
		{"sni on http", plain, model.RouteOptions{SNI: "a.example.com"}, "sni"},
		{"a wildcard sni", secure, model.RouteOptions{SNI: "*.example.com"}, "sni"},
		{"a host header with a space", plain, model.RouteOptions{HostHeader: "a b"}, "hostHeader"},
		{"via a nic that cannot be", plain, model.RouteOptions{Via: "net32"}, "via"},
		{"via a loopback address", plain, model.RouteOptions{Via: "127.0.0.1"}, "via"},
		{"via with an address target", addr, model.RouteOptions{Via: "net0"}, "via"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CheckOptions(tt.target, tt.opts)
			var oe *OptionError
			require.ErrorAs(t, err, &oe)
			require.Equal(t, tt.option, oe.Option)
		})
	}
}
