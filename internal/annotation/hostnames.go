package annotation

import (
	"maps"
	"slices"
	"strings"
	"unicode"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

// Hostnames returns every hostname written anywhere in description, inside
// route text or not, normalised and sorted. It reads words, not routes: fences
// and comments mean nothing here.
//
// Words are separated by whitespace, commas and backticks. A word counts when
// it normalises as a hostname, wildcards included, after one trailing '.',
// ';', ':' or ')' and one leading '(' are taken off, the way prose wraps a
// name.
func Hostnames(description string) []string {
	seen := make(map[string]struct{})
	for _, word := range strings.FieldsFunc(description, isWordBreak) {
		word = strings.TrimPrefix(word, "(")
		if n := len(word); n > 0 && strings.IndexByte(".;:)", word[n-1]) >= 0 {
			word = word[:n-1]
		}
		if h, err := hostname.Normalize(word); err == nil {
			seen[h] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func isWordBreak(r rune) bool {
	return r == ',' || r == '`' || unicode.IsSpace(r)
}
