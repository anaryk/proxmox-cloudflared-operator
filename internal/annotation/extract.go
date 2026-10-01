package annotation

import "strings"

const (
	fence           = "```"
	blockTag        = "cf-tunnel"
	shorthandPrefix = "cf-tunnel:"
)

// span is a byte range of the description that holds route text.
type span struct{ from, to int }

// extract finds the text that carries routes: fenced cf-tunnel blocks and
// one-line "cf-tunnel:" shorthands, in the order they appear.
//
// A block may open anywhere in a line, so a description that was flattened
// onto one line still works. A block that is never closed runs to the end of
// the description.
func extract(src string) []span {
	var spans []span
	for pos := 0; pos < len(src); {
		eol := len(src)
		if i := strings.IndexByte(src[pos:], '\n'); i >= 0 {
			eol = pos + i
		}
		// After a closing fence pos is mid-line, and the rest of that line
		// cannot be a shorthand.
		if pos == 0 || src[pos-1] == '\n' {
			if from, ok := shorthandStart(src, pos, eol); ok {
				spans = append(spans, span{from, eol})
				pos = eol + 1
				continue
			}
		}
		open := blockStart(src, pos, eol)
		if open < 0 {
			pos = eol + 1
			continue
		}
		end := len(src)
		if i := strings.Index(src[open:], fence); i >= 0 {
			end = open + i
		}
		spans = append(spans, span{open, end})
		pos = end + len(fence)
	}
	return spans
}

// shorthandStart reports where the text after "cf-tunnel:" begins when the
// first non-blank text of src[from:to] is that prefix.
func shorthandStart(src string, from, to int) (int, bool) {
	i := from
	for i < to && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r') {
		i++
	}
	end := i + len(shorthandPrefix)
	if end <= to && equalFoldASCII(src[i:end], shorthandPrefix) {
		return end, true
	}
	return 0, false
}

// blockStart returns the offset just after the first "```cf-tunnel" in
// src[from:to] that is followed by whitespace or the end of the text, or -1.
func blockStart(src string, from, to int) int {
	for i := from; ; {
		j := strings.Index(src[i:to], fence)
		if j < 0 {
			return -1
		}
		start := i + j + len(fence)
		end := start + len(blockTag)
		if end <= to && equalFoldASCII(src[start:end], blockTag) && (end == len(src) || isSpace(src[end])) {
			return end
		}
		i += j + 1
	}
}
