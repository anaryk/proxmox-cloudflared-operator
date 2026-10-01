package annotation

import "strings"

const (
	fence           = "```" // the shortest run of backticks that is a fence
	tildeFence      = "~~~"
	blockTag        = "cf-tunnel"
	shorthandPrefix = "cf-tunnel:"
)

// span is a byte range of the description that holds route text.
type span struct {
	from, to  int
	shorthand bool // a "cf-tunnel:" line rather than a fenced block
	fenceAt   int  // shorthand only: offset of a code fence on the line; 0 if none
}

// extract finds the text that carries routes: fenced cf-tunnel blocks and
// one-line "cf-tunnel:" shorthands, in the order they appear.
//
// Every fence is read, not only cf-tunnel ones, so that route text that is
// merely quoted in another block (a code sample, a nested example) is not
// mistaken for configuration:
//   - A run of three or more backticks opens a block anywhere in a line; its
//     tag is the text up to the next whitespace. The block ends at the next run
//     that is at least as long. A cf-tunnel block is parsed; any other block,
//     with or without a tag, is skipped whole.
//   - A line whose first non-blank text is a run of three or more tildes opens
//     a skipped block that ends at the next line that starts with a run at
//     least as long.
//   - A block that is never closed runs to the end of the description.
//   - Shorthand lines count only outside every block, and a shorthand line is
//     taken as a whole line. One that holds a code fence is rejected, and the
//     fence does not open a block.
//
// A cf-tunnel block may open mid-line, so a description that was flattened
// onto one line still works.
func extract(src string) []span {
	var spans []span
	eol := -1
	for pos := 0; pos < len(src); {
		if pos > eol {
			// Looked up once per line: after a fence the scan continues
			// mid-line, and searching again would be quadratic on long lines.
			eol = lineEnd(src, pos)
		}
		if pos == 0 || src[pos-1] == '\n' {
			first := skipBlanks(src, pos, eol)
			if n := runLen(src[first:eol], '~'); n >= len(tildeFence) {
				pos = tildeFenceEnd(src, eol+1, n)
				continue
			}
			if from, ok := shorthandStart(src, first, eol); ok {
				sp := span{from: from, to: eol, shorthand: true}
				if i := strings.Index(src[from:eol], fence); i >= 0 {
					sp.fenceAt = from + i
				}
				spans = append(spans, sp)
				pos = eol + 1
				continue
			}
		}
		open, n := openingFence(src, pos, eol)
		if open < 0 {
			pos = eol + 1
			continue
		}
		sp, route, next := readFence(src, open, n)
		if route {
			spans = append(spans, sp)
		}
		pos = next
	}
	return spans
}

// readFence reads the block whose opening run of n backticks starts at
// src[open:]. It returns the text span and true for a cf-tunnel block, and in
// every case the offset where scanning resumes.
func readFence(src string, open, n int) (sp span, route bool, next int) {
	// The tag is the text up to the next whitespace. A tag that is not exactly
	// cf-tunnel makes the block opaque. The closing fence is then searched
	// right after the opening one, so "```cf-tunnel```" is a block that is
	// closed at once.
	tagStart := open + n
	tagEnd := tagStart + len(blockTag)
	textStart := tagStart
	if tagEnd <= len(src) && equalFoldASCII(src[tagStart:tagEnd], blockTag) && (tagEnd == len(src) || isSpace(src[tagEnd])) {
		route = true
		textStart = tagEnd
	}
	closeAt, closeEnd := closingFence(src, textStart, n)
	if closeAt < 0 {
		return span{from: textStart, to: len(src)}, route, len(src)
	}
	next = closeEnd
	if route && commentIn(src, closingLineStart(src, textStart, closeAt), closeAt) {
		// The fence closes the block, but it sits in a comment, so the rest of
		// the line is not trusted to be anything but more comment.
		next = lineEnd(src, closeEnd) + 1
	}
	return span{from: textStart, to: closeAt}, route, next
}

// openingFence returns the start and length of the first run of at least three
// backticks in src[from:to], or -1.
func openingFence(src string, from, to int) (start, n int) {
	for i := from; i < to; {
		if src[i] != '`' {
			i++
			continue
		}
		n = runLen(src[i:to], '`')
		if n >= len(fence) {
			return i, n
		}
		i += n
	}
	return -1, 0
}

// closingFence returns the bounds of the first run of at least n backticks at
// or after from, or -1.
func closingFence(src string, from, n int) (start, end int) {
	for i := from; i < len(src); {
		if src[i] != '`' {
			i++
			continue
		}
		run := runLen(src[i:], '`')
		if run >= n {
			return i, i + run
		}
		i += run
	}
	return -1, -1
}

// commentIn reports whether a comment starts within src[from:to]. It is the
// rule of the lexer: a '#' at the start of a line or after whitespace.
func commentIn(src string, from, to int) bool {
	for i := from; i < to; i++ {
		if src[i] == '#' && (i == 0 || isSpace(src[i-1])) {
			return true
		}
	}
	return false
}

// tildeFenceEnd returns the offset after the next line that starts with a run
// of at least n tildes, searching from the line at pos, or the end of src.
func tildeFenceEnd(src string, pos, n int) int {
	for pos < len(src) {
		eol := lineEnd(src, pos)
		if runLen(src[skipBlanks(src, pos, eol):eol], '~') >= n {
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

// runLen is the number of leading bytes of s that equal c.
func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// closingLineStart is where the line of the closing fence at closeAt starts,
// or textStart if the line began before the block's text did.
func closingLineStart(src string, textStart, closeAt int) int {
	return textStart + strings.LastIndexByte(src[textStart:closeAt], '\n') + 1
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
