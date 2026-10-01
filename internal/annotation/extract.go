package annotation

import "strings"

const (
	fence           = "```"
	tildeFence      = "~~~"
	blockTag        = "cf-tunnel"
	shorthandPrefix = "cf-tunnel:"
)

// span is a byte range of the description that holds route text.
type span struct{ from, to int }

// extract finds the text that carries routes: fenced cf-tunnel blocks and
// one-line "cf-tunnel:" shorthands, in the order they appear.
//
// Every fence is read, not only cf-tunnel ones, so that route text that is
// merely quoted in another block (a code sample, a nested example) is not
// mistaken for configuration:
//   - "```" opens a block anywhere in a line; its tag is the text up to the
//     next whitespace. A cf-tunnel block runs to the next "```" and is parsed.
//     Any other block, with or without a tag, runs to the next "```" and is
//     skipped whole.
//   - A line whose first non-blank text is "~~~" opens a skipped block that
//     ends at the next such line.
//   - A block that is never closed runs to the end of the description.
//   - Shorthand lines count only outside every block.
//
// A cf-tunnel block may open mid-line, so a description that was flattened
// onto one line still works.
func extract(src string) []span {
	var spans []span
	for pos := 0; pos < len(src); {
		eol := lineEnd(src, pos)
		if pos == 0 || src[pos-1] == '\n' {
			first := skipBlanks(src, pos, eol)
			if strings.HasPrefix(src[first:eol], tildeFence) {
				pos = tildeFenceEnd(src, eol+1)
				continue
			}
			if from, ok := shorthandStart(src, first, eol); ok {
				// A fence on the line ends the shorthand and opens a block.
				to, next := eol, eol+1
				if i := strings.Index(src[from:eol], fence); i >= 0 {
					to = from + i
					next = to
				}
				spans = append(spans, span{from, to})
				pos = next
				continue
			}
		}
		i := strings.Index(src[pos:eol], fence)
		if i < 0 {
			pos = eol + 1
			continue
		}
		sp, route, next := readFence(src, pos+i)
		if route {
			spans = append(spans, sp)
		}
		pos = next
	}
	return spans
}

// readFence reads the block whose opening fence is at src[open:]. It returns
// the text span and true for a cf-tunnel block, and in every case the offset
// where scanning resumes.
func readFence(src string, open int) (sp span, route bool, next int) {
	tagStart := open + len(fence)
	tagEnd := tagStart
	for tagEnd < len(src) && !isSpace(src[tagEnd]) {
		tagEnd++
	}
	// A tag that is not exactly cf-tunnel makes the block opaque. The closing
	// fence is then searched right after the opening one, so "```cf-tunnel```"
	// is a block that is closed at once.
	textStart := tagStart
	if equalFoldASCII(src[tagStart:tagEnd], blockTag) {
		route = true
		textStart = tagEnd
	}
	end := len(src)
	if i := strings.Index(src[textStart:], fence); i >= 0 {
		end = textStart + i
	}
	return span{textStart, end}, route, end + len(fence)
}

// tildeFenceEnd returns the offset after the next line that starts with "~~~",
// searching from the line at pos, or the end of src.
func tildeFenceEnd(src string, pos int) int {
	for pos < len(src) {
		eol := lineEnd(src, pos)
		if strings.HasPrefix(src[skipBlanks(src, pos, eol):eol], tildeFence) {
			return eol + 1
		}
		pos = eol + 1
	}
	return len(src)
}

// shorthandStart reports where the text after "cf-tunnel:" begins when
// src[from:to], which starts at the first non-blank text of the line, begins
// with that prefix.
func shorthandStart(src string, from, to int) (int, bool) {
	end := from + len(shorthandPrefix)
	if end <= to && equalFoldASCII(src[from:end], shorthandPrefix) {
		return end, true
	}
	return 0, false
}

func lineEnd(src string, pos int) int {
	if i := strings.IndexByte(src[pos:], '\n'); i >= 0 {
		return pos + i
	}
	return len(src)
}

func skipBlanks(src string, from, to int) int {
	for from < to && (src[from] == ' ' || src[from] == '\t' || src[from] == '\r') {
		from++
	}
	return from
}
