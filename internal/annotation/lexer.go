package annotation

import (
	"slices"
	"unicode/utf8"
)

// token is a run of text without separators, with the position of its first
// byte in the description.
type token struct {
	text   string
	at     Position
	indent string // the blanks the token's line starts with
}

// lex splits src[sp.from:sp.to] into tokens. Spaces, tabs, line breaks and
// commas separate tokens. A '#' that follows whitespace or starts a line begins
// a comment that runs to the end of the line; any other '#' is part of its
// token, so "a,#b" and "cf-tunnel:#b" contain no comment.
func lex(src string, sp span, lines lineIndex) []token {
	var toks []token
	for i := sp.from; i < sp.to; {
		switch {
		case isSeparator(src[i]):
			i++
		case src[i] == '#' && (i == 0 || isSpace(src[i-1])):
			for i < sp.to && src[i] != '\n' {
				i++
			}
		default:
			start := i
			for i < sp.to && !isSeparator(src[i]) {
				i++
			}
			at := lines.position(start)
			toks = append(toks, token{text: src[start:i], at: at, indent: lines.indent(at.Line)})
		}
	}
	return toks
}

// lineIndex locates byte offsets of the description. Only '\n' ends a line, so
// CRLF counts as one break and the '\r' is ordinary whitespace at the end of
// a line.
type lineIndex struct {
	src     string
	starts  []int // offset at which each line starts
	indents []int // offset at which the leading blanks of each line end
	cols    []int // 1-based column, in characters, of every byte offset
}

func newLineIndex(src string) lineIndex {
	x := lineIndex{src: src, starts: []int{0}, indents: []int{0}, cols: make([]int, len(src)+1)}
	col, leading := 1, true
	for i := 0; i < len(src); {
		c := src[i]
		x.cols[i] = col
		if c == '\n' {
			x.starts = append(x.starts, i+1)
			x.indents = append(x.indents, i+1)
			col, leading = 1, true
			i++
			continue
		}
		if leading && (c == ' ' || c == '\t') {
			x.indents[len(x.indents)-1] = i + 1
		} else {
			leading = false
		}
		_, size := utf8.DecodeRuneInString(src[i:])
		for j := i + 1; j < i+size; j++ {
			x.cols[j] = col
		}
		col++
		i += size
	}
	x.cols[len(src)] = col
	return x
}

// position converts a byte offset to a 1-based line and character column.
func (x lineIndex) position(offset int) Position {
	i, found := slices.BinarySearch(x.starts, offset)
	if !found {
		i--
	}
	return Position{Line: i + 1, Col: x.cols[offset]}
}

// indent returns the leading blanks of a line.
func (x lineIndex) indent(line int) string {
	return x.src[x.starts[line-1]:x.indents[line-1]]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isSeparator(c byte) bool {
	return c == ',' || isSpace(c)
}

// equalFoldASCII compares s with an all-lower-case ASCII word, ignoring the
// case of ASCII letters only. strings.EqualFold would also fold characters
// such as the long s into the word.
func equalFoldASCII(s, lower string) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}
