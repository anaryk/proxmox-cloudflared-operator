// Package annotation reads the routes that admins write into the notes of a
// guest: a fenced block tagged cf-tunnel, or one-line "cf-tunnel:" shorthand.
//
// The package is pure. It never fails as a whole; every mistake becomes an
// Error with a position and the rest of the text is still parsed.
package annotation

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	arrow        = "->"
	targetSyntax = "expected [http|https://][ipv4]:port"
)

// Option names as they appear in messages. Options that take a value include
// the '='.
const (
	optNoTLSVerify = "no-tls-verify"
	optHostHeader  = "host-header="
	optSNI         = "sni="
	optVia         = "via="
)

// Entry is one route definition: hostnames that share a target and options.
type Entry struct {
	Hosts   []string // normalised
	Target  model.Target
	Options model.RouteOptions
	Line    int // 1-based position of the first host in the description
	Col     int
}

// Error is a mistake in the annotation. Line and Col are 1-based positions in
// the whole description; Col counts bytes.
type Error struct {
	Line int
	Col  int
	Msg  string
}

func (e Error) Error() string {
	return fmt.Sprintf("line %d, col %d: %s", e.Line, e.Col, e.Msg)
}

// Result is what Parse found. Entries are the valid definitions in the order
// they appear; Errors describe everything that was dropped.
type Result struct {
	Found   bool // a cf-tunnel block or shorthand line exists
	Entries []Entry
	Errors  []Error
}

// Parse extracts and parses the route text of a guest description.
func Parse(description string) Result {
	spans := extract(description)
	p := parser{seen: make(map[string]struct{})}
	if len(spans) > 0 {
		lines := newLineIndex(description)
		for _, sp := range spans {
			p.parseSegment(lex(description, sp, lines))
		}
	}
	p.res.Found = len(spans) > 0
	return p.res
}

type parser struct {
	res  Result
	seen map[string]struct{} // hostnames of the entries kept so far
}

// parseSegment parses the tokens of one block or shorthand line. An entry
// never continues into the next segment.
//
// After an error the entry is dropped and parsing resumes at the first token
// on a later line than the one the error was found on.
func (p *parser) parseSegment(toks []token) {
	c := &cursor{toks: toks}
	for !c.done() {
		d, err := parseEntry(c)
		if err != nil {
			p.res.Errors = append(p.res.Errors, *err)
			c.skipThrough(err.Line)
			continue
		}
		p.commit(d)
	}
}

// commit keeps a complete entry unless it repeats a hostname. A dropped entry
// reserves none of its hostnames.
func (p *parser) commit(d draft) {
	for i, h := range d.Hosts {
		if _, dup := p.seen[h]; dup || slices.Contains(d.Hosts[:i], h) {
			p.res.Errors = append(p.res.Errors, *errAt(d.at[i], "hostname %q is listed twice", h))
			return
		}
	}
	for _, h := range d.Hosts {
		p.seen[h] = struct{}{}
	}
	p.res.Entries = append(p.res.Entries, d.Entry)
}

// draft is an entry under construction.
type draft struct {
	Entry
	at []token // where each of Hosts was written
}

func errAt(t token, format string, args ...any) *Error {
	return &Error{Line: t.line, Col: t.col, Msg: fmt.Sprintf(format, args...)}
}

// parseEntry reads one entry: host+ "->" target option*.
func parseEntry(c *cursor) (draft, *Error) {
	var d draft
	arrowTok, err := d.readHosts(c)
	if err == nil {
		err = d.readTarget(c, arrowTok)
	}
	if err == nil {
		err = d.readOptions(c)
	}
	return d, err
}

// readHosts reads up to and including the arrow and returns the arrow token.
func (d *draft) readHosts(c *cursor) (token, *Error) {
	for {
		t, ok := c.next()
		if !ok {
			// The cursor was not done when the entry started, so there is a host.
			return token{}, errAt(d.at[len(d.at)-1], "expected '->' after hostnames")
		}
		if t.text == arrow {
			if len(d.Hosts) == 0 {
				return token{}, errAt(t, "expected a hostname before '->'")
			}
			return t, nil
		}
		name, err := hostname.Normalize(t.text)
		switch {
		case err == nil:
			if len(d.Hosts) == 0 {
				d.Line, d.Col = t.line, t.col
			}
			d.Hosts = append(d.Hosts, name)
			d.at = append(d.at, t)
		case len(d.Hosts) > 0 && strings.ContainsAny(t.text, ":="):
			// A target or an option where the arrow belongs, not a mistyped host.
			return token{}, errAt(t, "expected '->' after hostnames")
		default:
			return token{}, errAt(t, "%s", err)
		}
	}
}

func (d *draft) readTarget(c *cursor, arrowTok token) *Error {
	t, ok := c.next()
	if !ok {
		return errAt(arrowTok, "invalid target %q: %s", "", targetSyntax)
	}
	target, err := parseTarget(t.text)
	if err != nil {
		return errAt(t, "%s", err)
	}
	d.Target = target
	return nil
}

// parseTarget parses [ scheme "://" ] [ ipv4 ] ":" port [ "/" ].
func parseTarget(s string) (model.Target, error) {
	invalid := fmt.Errorf("invalid target %q: %s", s, targetSyntax)
	t := model.Target{Scheme: model.SchemeHTTP}

	rest := s
	if scheme, after, ok := strings.Cut(s, "://"); ok {
		switch {
		case equalFoldASCII(scheme, "http"):
		case equalFoldASCII(scheme, "https"):
			t.Scheme = model.SchemeHTTPS
		default:
			return t, invalid
		}
		rest = after
	}

	hostPort, path, _ := strings.Cut(rest, "/")
	addr, port, ok := strings.Cut(hostPort, ":")
	if !ok {
		return t, invalid
	}
	if addr != "" {
		a, err := netip.ParseAddr(addr)
		if err != nil || !a.Is4() {
			return t, invalid
		}
		t.Addr = a
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return t, invalid
	}
	t.Port = uint16(n)

	// Only a lone trailing slash is accepted; checked last so that a target
	// that is wrong anyway is reported as such rather than as a path.
	if path != "" {
		return t, fmt.Errorf("invalid target %q: paths are not supported", s)
	}
	return t, nil
}

// readOptions reads the options that follow the target and stops at the first
// token that is not one.
//
// A token that normalises as a hostname starts the next entry. So does any
// other token on a later line than the entry so far: a mistake in the next
// entry is reported there and does not take this valid entry down with it.
// On the same line such a token is a stray option of this entry.
func (d *draft) readOptions(c *cursor) *Error {
	given := make(map[string]bool)
	for {
		t, ok := c.peek()
		if !ok {
			return nil
		}
		name, value, isOption := splitOption(t.text)
		if !isOption {
			if _, err := hostname.Normalize(t.text); err == nil || t.line > c.last().line {
				return nil
			}
			return errAt(t, "unknown option %q", t.text)
		}
		c.next()
		if given[name] {
			return errAt(t, "option %q given twice", name)
		}
		given[name] = true
		if err := d.apply(t, name, value); err != nil {
			return err
		}
	}
}

// splitOption recognises "no-tls-verify", "host-header=v", "sni=v" and
// "via=v", with the key in any case.
func splitOption(tok string) (name, value string, ok bool) {
	key, value, hasValue := strings.Cut(tok, "=")
	switch {
	case !hasValue && equalFoldASCII(key, "no-tls-verify"):
		return optNoTLSVerify, "", true
	case hasValue && equalFoldASCII(key, "host-header"):
		return optHostHeader, value, true
	case hasValue && equalFoldASCII(key, "sni"):
		return optSNI, value, true
	case hasValue && equalFoldASCII(key, "via"):
		return optVia, value, true
	}
	return "", "", false
}

func (d *draft) apply(t token, name, value string) *Error {
	switch name {
	case optNoTLSVerify:
		if d.Target.Scheme != model.SchemeHTTPS {
			return errAt(t, "%s only applies to https targets", name)
		}
		d.Options.NoTLSVerify = true
	case optHostHeader:
		if value == "" {
			return errAt(t, "invalid value for %s", name)
		}
		d.Options.HostHeader = value
	case optSNI:
		if d.Target.Scheme != model.SchemeHTTPS {
			return errAt(t, "%s only applies to https targets", name)
		}
		sni, err := hostname.Normalize(value)
		if err != nil || hostname.IsWildcard(sni) {
			return errAt(t, "invalid value for %s", name)
		}
		d.Options.SNI = sni
	case optVia:
		if d.Target.Addr.IsValid() {
			return errAt(t, "via= cannot be combined with an address in the target")
		}
		via, ok := parseVia(value)
		if !ok {
			return errAt(t, "invalid value for %s", name)
		}
		d.Options.Via = via
	}
	return nil
}

// parseVia accepts a NIC name ("net" and digits) or an IPv4 address and
// returns it in canonical form.
func parseVia(v string) (string, bool) {
	if len(v) > len("net") && equalFoldASCII(v[:3], "net") && isDigits(v[3:]) {
		return "net" + v[3:], true
	}
	addr, err := netip.ParseAddr(v)
	if err != nil || !addr.Is4() {
		return "", false
	}
	return addr.String(), true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// cursor walks the tokens of one segment.
type cursor struct {
	toks []token
	pos  int
}

func (c *cursor) done() bool { return c.pos >= len(c.toks) }

func (c *cursor) peek() (token, bool) {
	if c.done() {
		return token{}, false
	}
	return c.toks[c.pos], true
}

func (c *cursor) next() (token, bool) {
	t, ok := c.peek()
	if ok {
		c.pos++
	}
	return t, ok
}

// last is the token consumed most recently; only valid after a next.
func (c *cursor) last() token { return c.toks[c.pos-1] }

// skipThrough drops every token up to and including the given line.
func (c *cursor) skipThrough(line int) {
	for !c.done() && c.toks[c.pos].line <= line {
		c.pos++
	}
}
