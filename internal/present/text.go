package present

import (
	"strings"
	"unicode"
)

// Printable returns s with every character that a terminal acts on replaced by
// a question mark: the control characters of C0 and C1, which include escape,
// bell, newline and tab, DEL, the characters that change the direction of the
// text, which can make a hostname read as another, and the other characters
// of format that show as nothing, as the zero-width ones, the byte order mark,
// the soft hyphen and the tags, which can hide a difference between two names.
// Names and messages come from guests, from the DNS records of other parties
// and from Cloudflare, and none of them may write to the terminal of the
// admin. JSON is exempt, as it is escaped.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || IsBidi(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}

// IsBidi reports whether r is a control of the direction of text, or one of
// the separators of lines that a terminal may take for a line break.
func IsBidi(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, // embeddings and overrides
		r >= 0x2066 && r <= 0x2069,            // isolates
		r == 0x200E, r == 0x200F, r == 0x061C, // marks of direction
		r == 0x2028, r == 0x2029: // line and paragraph separators
		return true
	}
	return false
}
