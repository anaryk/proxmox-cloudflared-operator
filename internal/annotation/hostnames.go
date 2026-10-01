package annotation

import (
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

// Hostnames returns every hostname written anywhere in description, inside
// route text or not, normalised and sorted. It reads words, not routes: fences,
// comments and the syntax of an entry mean nothing here.
//
// A word is a run of ASCII letters, digits, '.', '_', '*' and '-'; every other
// byte separates words. Dots, hyphens and stars at either end of a word are
// taken off, except the "*." that starts a wildcard, and the word counts when
// what is left normalises as a hostname. '_' stays inside a word, so
// "a_b.example.com" is not read as "b.example.com".
func Hostnames(description string) []string {
	seen := make(map[string]struct{})
	for _, word := range strings.FieldsFunc(description, isWordBreak) {
		if h, err := hostname.Normalize(trimWord(word)); err == nil {
			seen[h] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// isWordBreak works on runes, but every rune outside ASCII, and every byte
// that is not valid UTF-8, breaks a word, so this is the same as splitting on
// bytes.
func isWordBreak(r rune) bool {
	return r >= utf8.RuneSelf || !isWordByte(byte(r))
}

func isWordByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '.' || c == '_' || c == '*' || c == '-'
}

// trimWord takes off the punctuation that prose and markup put around a name:
// "...a.example.com.", "--a.example.com", "**a.example.com**". A leading "*."
// is kept, so "***.example.com**" is the wildcard "*.example.com".
func trimWord(w string) string {
	w = strings.TrimRight(w, ".-*")
	lead := len(w) - len(strings.TrimLeft(w, ".-*"))
	if lead >= 2 && w[lead-2:lead] == "*." {
		lead -= 2
	}
	return w[lead:]
}
