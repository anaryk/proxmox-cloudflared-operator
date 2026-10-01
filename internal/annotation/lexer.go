package annotation

import "slices"

// token is a run of text without separators, with the position of its first
// byte in the description.
type token struct {
	text string
	line int
	col  int
}

// lex splits src[sp.from:sp.to] into tokens. Spaces, tabs, line breaks and
// commas separate tokens. A token that starts with '#' is a comment that runs
// to the end of the line; a '#' inside a token is part of it.
func lex(src string, sp span, lines lineIndex) []token {
	var toks []token
	for i := sp.from; i < sp.to; {
		switch {
		case isSeparator(src[i]):
			i++
		case src[i] == '#':
			for i < sp.to && src[i] != '\n' {
				i++
			}
		default:
			start := i
			for i < sp.to && !isSeparator(src[i]) {
				i++
			}
			line, col := lines.position(start)
			toks = append(toks, token{text: src[start:i], line: line, col: col})
		}
	}
	return toks
}

// lineIndex holds the byte offset at which each line of the description
// starts. Only '\n' ends a line, so CRLF counts as one break and the '\r' is
// ordinary whitespace at the end of a line.
type lineIndex []int

func newLineIndex(src string) lineIndex {
	idx := lineIndex{0}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			idx = append(idx, i+1)
		}
	}
	return idx
}

// position converts a byte offset to a 1-based line and byte column.
func (x lineIndex) position(offset int) (line, col int) {
	i, found := slices.BinarySearch(x, offset)
	if !found {
		i--
	}
	return i + 1, offset - x[i] + 1
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
