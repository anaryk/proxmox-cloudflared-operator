package annotation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostnames(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty", input: ""},
		{name: "prose without hostnames", input: "Web server, retired. See the wiki (page 3)."},
		{
			name: "fences and comments mean nothing",
			input: "```yaml\na.example.com\n```\n# b.example.com\n" +
				block("c.example.com -> :80") + "\n```\nd.example.com -> :81\n```",
			want: hosts("a.example.com", "b.example.com", "c.example.com", "d.example.com"),
		},
		{
			name:  "whitespace, commas and backticks separate",
			input: "a.example.com,b.example.com`c.example.com`\td.example.com e.example.com\r\nf.example.com",
			want:  hosts("a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com"),
		},
		{
			name:  "punctuation of prose",
			input: "See (a.example.com), b.example.com. Then c.example.com; d.example.com: (e.example.com) f.example.com)",
			want:  hosts("a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com"),
		},
		{
			name:  "any amount of punctuation around a name",
			input: "((a.example.com b.example.com); c.example.com:) ...d.example.com... --e.example.com--",
			want:  hosts("a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com"),
		},
		{
			name:  "normalised, wildcards included, each once",
			input: "*.Example.COM A.example.com a.example.com. a.example.com",
			want:  hosts("*.example.com", "a.example.com"),
		},
		{
			name:  "names inside other text",
			input: "https://a.example.com/x b.example.com:8080 host-header=c.example.com user@d.example.com",
			want:  hosts("a.example.com", "b.example.com", "c.example.com", "d.example.com"),
		},
		{
			name:  "words that are not hostnames",
			input: "10.0.0.5 a_b.example.com example a..b.example.com -.- * *. 1.2.3",
		},
		{name: "a typo in the shorthand prefix", input: "cf-tunel:www.example.com -> :8080", want: hosts("www.example.com")},
		{name: "missing spaces around the arrow", input: "www.example.com->:8080", want: hosts("www.example.com")},
		{name: "commented out", input: "#www.example.com -> :8080", want: hosts("www.example.com")},
		{name: "bold", input: "**www.example.com**", want: hosts("www.example.com")},
		{name: "italic", input: "*www.example.com*", want: hosts("www.example.com")},
		{name: "a wildcard in bold", input: "***.example.com**", want: hosts("*.example.com")},
		{name: "a URL", input: "https://www.example.com", want: hosts("www.example.com")},
		{name: "quoted", input: `"www.example.com" 'a.example.com'`, want: hosts("a.example.com", "www.example.com")},
		{name: "in parentheses before a full stop", input: "(see www.example.com).", want: hosts("www.example.com")},
		{name: "with a port", input: "www.example.com:8080", want: hosts("www.example.com")},
		{name: "next to letters that are not ASCII", input: "žwww.example.comž", want: hosts("www.example.com")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Hostnames(tt.input))
		})
	}
}

func TestHostnamesHoldEverythingMentioned(t *testing.T) {
	for _, input := range []string{
		block("a.example.com b_c.example.com -> :80", "  d.example.com"),
		"````cf-tunnel\na.example.com -> :80\n```b.example.com```\n````",
		"cf-tunnel: a.example.com -> :80 ```",
		block("a.example.com -> b.example.com"),
	} {
		require.Subset(t, Hostnames(input), Parse(input).Mentioned, "input %q", input)
	}
}

func TestHostnamesLongInput(t *testing.T) {
	word := strings.Repeat("a", 1<<20)
	input := strings.Repeat("x.example.com ", 1<<14) + word + "." + word + " (" + word + ".com)"

	require.Equal(t, hosts("x.example.com"), Hostnames(input))
}
