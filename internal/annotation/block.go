package annotation

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Block returns the route text of a description as it stands there: the
// fenced cf-tunnel blocks with their fences and the "cf-tunnel:" lines, from
// the line the first one begins on to where the last one ends, and the line it
// begins on, 1-based. Nothing else of the description is in it: lines between
// them that are not route text are empty, and the text before a block on its
// first line is blanked, one space for each character, so that the line and
// column of an Error of Parse point at the same text in the block. A
// description without route text has none, at line 0.
func Block(description string) (text string, line int) {
	spans := extract(description)
	if len(spans) == 0 {
		return "", 0
	}
	var b strings.Builder
	first := spans[0].start
	lineStart := strings.LastIndexByte(description[:first], '\n') + 1
	blank(&b, description[lineStart:first])
	for i, sp := range spans {
		if i > 0 {
			gap := description[spans[i-1].end:sp.start]
			if n := strings.Count(gap, "\n"); n > 0 {
				b.WriteString(strings.Repeat("\n", n))
				gap = gap[strings.LastIndexByte(gap, '\n')+1:]
			}
			blank(&b, gap)
		}
		b.WriteString(description[sp.start:sp.end])
	}
	return b.String(), strings.Count(description[:first], "\n") + 1
}

// blank writes a space for every character of s.
func blank(b *strings.Builder, s string) {
	b.WriteString(strings.Repeat(" ", utf8.RuneCountInString(s)))
}

// OptionError is an option of a route that the rules of the Notes refuse.
// Option is its name in the JSON of the options, such as "via".
type OptionError struct {
	Option string
	Msg    string
}

func (e *OptionError) Error() string { return e.Option + ": " + e.Msg }

// CheckOptions checks the options of a route written elsewhere than in the
// Notes, such as a manual route, by the rules the Notes have for them, and
// returns them in normal form. AllowNode is no option of the Notes and is
// left as it is.
func CheckOptions(t model.Target, o model.RouteOptions) (model.RouteOptions, error) {
	https := t.Scheme == model.SchemeHTTPS
	if o.NoTLSVerify && !https {
		return o, &OptionError{"noTLSVerify", "only applies to https targets"}
	}
	if o.HostHeader != "" && !validHostHeader(o.HostHeader) {
		return o, &OptionError{"hostHeader", fmt.Sprintf("%q is not a host name with an optional port", o.HostHeader)}
	}
	if o.SNI != "" {
		if !https {
			return o, &OptionError{"sni", "only applies to https targets"}
		}
		sni, err := hostname.Normalize(o.SNI)
		if err != nil || hostname.IsWildcard(sni) {
			return o, &OptionError{"sni", fmt.Sprintf("%q is not a host name", o.SNI)}
		}
		o.SNI = sni
	}
	if o.Via != "" {
		if t.Addr.IsValid() {
			return o, &OptionError{"via", "cannot be combined with an address in the target"}
		}
		via, ok := parseVia(o.Via)
		if !ok {
			return o, &OptionError{"via", fmt.Sprintf("%q is neither a NIC from net0 to net%d nor an IPv4 address a guest can have", o.Via, maxNIC)}
		}
		o.Via = via
	}
	return o, nil
}
