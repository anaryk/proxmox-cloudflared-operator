// Package annotation reads the routes that admins write into the notes of a
// guest: a fenced block tagged cf-tunnel, or one-line "cf-tunnel:" shorthand.
//
// What gets parsed decides what is published, so doubt is an error: anything
// that is not clearly a route is reported and the entry it belongs to is
// dropped.
//
// The package is pure. It never fails as a whole; every mistake becomes an
// Error with a position and the rest of the text is still parsed.
//
// Layout: an entry starts on a line, and the blanks that line starts with are
// its indent. Following lines continue the entry only when they start with
// that indent and more; any other line starts a new entry. After the target,
// only options may follow on continuation lines, while on the entry's own line
// a hostname starts the next entry, which keeps descriptions that were
// flattened onto one line working.
//
// A hostname may be listed only once in a description. Every hostname that is
// read counts, also in entries that are dropped, and so does every hostname in
// the text skipped after an error; each later mention is an error and drops the
// entry it is in.
package annotation

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	arrow        = "->"
	targetSyntax = "expected [http|https://][ipv4]:port"

	msgShorthandFence    = "code fences are not allowed on a cf-tunnel: line"
	msgShorthandContinue = "a cf-tunnel: line cannot continue on the next line; use a fenced block"
	msgMixedIndent       = "inconsistent indentation: mix of tabs and spaces"

	maxNIC = 31 // the highest netN a guest can have
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
	Col     int // 1-based, counted in characters from the start of the line
}

// Error is a mistake in the annotation. Line and Col are 1-based positions in
// the whole description; Col counts characters from the start of the line, not
// bytes.
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
			p.parseSpan(description, sp, lines)
		}
	}
	p.res.Found = len(spans) > 0
	return p.res
}

type parser struct {
	res  Result
	seen map[string]struct{} // every hostname read so far, kept entry or not
}

// parseSpan parses one block or shorthand line.
func (p *parser) parseSpan(src string, sp span, lines lineIndex) {
	if sp.fenceAt > 0 {
		line, col := lines.position(sp.fenceAt)
		p.res.Errors = append(p.res.Errors, Error{Line: line, Col: col, Msg: msgShorthandFence})
		return
	}
	kept := len(p.res.Entries)
	p.parseSegment(lex(src, sp, lines))
	if sp.shorthand {
		// The entry is dropped, but its hostnames stay claimed.
		if t, ok := shorthandContinuation(src, sp, lines); ok {
			p.res.Entries = p.res.Entries[:kept]
			p.res.Errors = append(p.res.Errors, errorAt(t, msgShorthandContinue))
		}
	}
}

// shorthandContinuation finds the first token of the line right after a
// shorthand line when that line is indented more and starts with "->" or an
// option. It looks like the rest of the entry, but a shorthand line never
// continues, so the entry is not published without it.
func shorthandContinuation(src string, sp span, lines lineIndex) (token, bool) {
	next := sp.to + 1
	if next >= len(src) {
		return token{}, false
	}
	toks := lex(src, span{from: next, to: lineEnd(src, next)}, lines)
	if len(toks) == 0 {
		return token{}, false
	}
	line, _ := lines.position(sp.from)
	t := toks[0]
	if !continues(lines.indent(line), t.indent) {
		return token{}, false
	}
	_, _, isOption := splitOption(t.text)
	return t, isOption || t.text == arrow
}

// parseSegment parses the tokens of one block or shorthand line. An entry
// never continues into the next segment.
//
// After a syntax error the entry is dropped and parsing resumes at the next
// line that does not continue it. The hostnames in the text skipped that way
// are reserved: they may well belong to the broken entry.
func (p *parser) parseSegment(toks []token) {
	c := &cursor{toks: toks}
	for !c.done() {
		d, failed := parseEntry(c)
		dup := p.claim(d)
		if failed != nil {
			p.res.Errors = append(p.res.Errors, failed.err)
			if failed.skip {
				p.reserve(c.skipEntry(d.indent, failed.err.Line))
			}
			continue
		}
		if !dup {
			p.res.Entries = append(p.res.Entries, d.Entry)
		}
	}
}

// reserve claims every token that is a hostname without reporting it.
func (p *parser) reserve(toks []token) {
	for _, t := range toks {
		if h, err := hostname.Normalize(t.text); err == nil {
			p.seen[h] = struct{}{}
		}
	}
}

// claim records the hostnames of d and reports every one that was already
// taken. All hostnames that were read count, including those of entries that
// are dropped, so one mention that is broken does not make a second one safe.
func (p *parser) claim(d draft) (dup bool) {
	for i, h := range d.Hosts {
		if _, taken := p.seen[h]; taken {
			p.res.Errors = append(p.res.Errors, errorAt(d.at[i], "hostname %q is listed twice", h))
			dup = true
		}
		p.seen[h] = struct{}{}
	}
	return dup
}

// draft is an entry under construction.
type draft struct {
	Entry
	at     []token // where each of Hosts was written
	indent string  // the blanks the entry's first line starts with
	line0  int     // the entry's first line
	taken  int     // tokens read so far
}

// failure is a syntax error. skip is set when the entry may have more text
// that must be skipped; it is clear when the entry just ended too early and
// the next token already belongs to the next entry.
type failure struct {
	err  Error
	skip bool
}

func errorAt(t token, format string, args ...any) Error {
	return Error{Line: t.line, Col: t.col, Msg: fmt.Sprintf(format, args...)}
}

func fail(t token, format string, args ...any) *failure {
	return &failure{err: errorAt(t, format, args...), skip: true}
}

func incomplete(t token, format string, args ...any) *failure {
	return &failure{err: errorAt(t, format, args...)}
}

// parseEntry reads one entry: host+ "->" target option*. The cursor must not
// be at the end.
func parseEntry(c *cursor) (draft, *failure) {
	first, _ := c.peek()
	d := draft{indent: first.indent, line0: first.line}
	arrowTok, err := d.readHosts(c)
	if err == nil {
		err = d.readTarget(c, arrowTok)
	}
	if err == nil {
		err = d.readOptions(c)
	}
	return d, err
}

// lookahead says what the next token means for the entry being read.
type lookahead int

const (
	inEntry     lookahead = iota // the token is part of the entry
	endsEntry                    // end of the segment, or a line that starts a new entry
	mixedIndent                  // a line indented in a way that cannot be compared with the entry's
)

// peek returns the next token and whether it belongs to the entry.
func (d *draft) peek(c *cursor) (token, lookahead) {
	t, ok := c.peek()
	switch {
	case !ok:
		return token{}, endsEntry
	case d.taken == 0 || t.line == c.last().line || continues(d.indent, t.indent):
		return t, inEntry
	case inconsistent(d.indent, t.indent):
		return t, mixedIndent
	}
	return t, endsEntry
}

func (d *draft) take(c *cursor) (token, lookahead) {
	t, state := d.peek(c)
	if state == inEntry {
		c.pos++
		d.taken++
	}
	return t, state
}

// continues reports whether a line that starts with lineIndent belongs to an
// entry whose first line starts with entryIndent. Indents are compared as
// text, so a tab is never taken for some number of spaces.
func continues(entryIndent, lineIndent string) bool {
	return len(lineIndent) > len(entryIndent) && strings.HasPrefix(lineIndent, entryIndent)
}

// inconsistent reports whether two indents cannot be ordered because neither
// is a prefix of the other, as with a tab against spaces.
func inconsistent(a, b string) bool {
	return !strings.HasPrefix(a, b) && !strings.HasPrefix(b, a)
}

// readHosts reads up to and including the arrow and returns the arrow token.
func (d *draft) readHosts(c *cursor) (token, *failure) {
	for {
		t, state := d.take(c)
		switch state {
		case endsEntry:
			// The first token always exists, so there is a host to point at.
			return token{}, incomplete(d.at[len(d.at)-1], "expected '->' after hostnames")
		case mixedIndent:
			return token{}, fail(t, msgMixedIndent)
		}
		if t.text == arrow {
			if len(d.Hosts) == 0 {
				return token{}, fail(t, "expected a hostname before '->'")
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
			return token{}, fail(t, "expected '->' after hostnames")
		default:
			return token{}, fail(t, "%s", err)
		}
	}
}

func (d *draft) readTarget(c *cursor, arrowTok token) *failure {
	t, state := d.take(c)
	switch state {
	case endsEntry:
		return incomplete(arrowTok, "invalid target %q: %s", "", targetSyntax)
	case mixedIndent:
		return fail(t, msgMixedIndent)
	}
	target, err := parseTarget(t.text)
	if err != nil {
		return fail(t, "%s", err)
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
// On the entry's own line a token that normalises as a hostname starts the next
// entry, which is how entries flattened onto one line are told apart. On
// continuation lines nothing but options is accepted: a hostname there is more
// likely a mistake than a new entry, and an entry that silently lost an option
// should not be published.
func (d *draft) readOptions(c *cursor) *failure {
	given := make(map[string]bool)
	for {
		t, state := d.peek(c)
		switch state {
		case endsEntry:
			return nil
		case mixedIndent:
			return fail(t, msgMixedIndent)
		}
		name, value, isOption := splitOption(t.text)
		if !isOption {
			if _, err := hostname.Normalize(t.text); err == nil && t.line == d.line0 {
				return nil
			}
			return fail(t, "unknown option %q", t.text)
		}
		d.take(c)
		if given[name] {
			return fail(t, "option %q given twice", name)
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

func (d *draft) apply(t token, name, value string) *failure {
	switch name {
	case optNoTLSVerify:
		if d.Target.Scheme != model.SchemeHTTPS {
			return fail(t, "%s only applies to https targets", name)
		}
		d.Options.NoTLSVerify = true
	case optHostHeader:
		if !validHostHeader(value) {
			return fail(t, "invalid value for %s", name)
		}
		d.Options.HostHeader = value
	case optSNI:
		if d.Target.Scheme != model.SchemeHTTPS {
			return fail(t, "%s only applies to https targets", name)
		}
		sni, err := hostname.Normalize(value)
		if err != nil || hostname.IsWildcard(sni) {
			return fail(t, "invalid value for %s", name)
		}
		d.Options.SNI = sni
	case optVia:
		if d.Target.Addr.IsValid() {
			return fail(t, "via= cannot be combined with an address in the target")
		}
		via, ok := parseVia(value)
		if !ok {
			return fail(t, "invalid value for %s", name)
		}
		d.Options.Via = via
	}
	return nil
}

// parseVia accepts a NIC name ("net0" to "net31") or an IPv4 address and
// returns it in canonical form.
func parseVia(v string) (string, bool) {
	if len(v) > len("net") && equalFoldASCII(v[:3], "net") && isNICIndex(v[3:]) {
		return "net" + v[3:], true
	}
	addr, err := netip.ParseAddr(v)
	if err != nil || !addr.Is4() {
		return "", false
	}
	return addr.String(), true
}

// isNICIndex accepts 0 to 31, written without leading zeros.
func isNICIndex(s string) bool {
	if s == "" || len(s) > 2 || s != "0" && s[0] == '0' {
		return false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n <= maxNIC
}

const maxHostHeaderLen = 253

// validHostHeader accepts what can appear in a Host header without surprises:
// letters, digits, '.', '_', ':' (for a port) and '-'.
func validHostHeader(v string) bool {
	if v == "" || len(v) > maxHostHeaderLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			c == '.' || c == '_' || c == ':' || c == '-'
		if !ok {
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

// last is the token consumed most recently; only valid after a next.
func (c *cursor) last() token { return c.toks[c.pos-1] }

// skipEntry drops the rest of a broken entry: the tokens on the line of the
// error, then every following line that continues the entry. It returns what
// it dropped.
func (c *cursor) skipEntry(indent string, line int) []token {
	from := c.pos
	for !c.done() && (c.toks[c.pos].line <= line || continues(indent, c.toks[c.pos].indent)) {
		c.pos++
	}
	return c.toks[from:c.pos]
}
