// Package hostname validates and orders the public hostnames that tunnel
// ingress rules are built from.
//
// Apart from Normalize, every function expects host names that already went
// through Normalize and does not validate them again. Patterns passed to
// MatchPattern are not hostnames ("*" and "*.com" are valid patterns); they
// must be lower case without a trailing dot.
package hostname

import (
	"cmp"
	"errors"
	"fmt"
	"strings"
)

const (
	maxNameLen  = 253
	maxLabelLen = 63
	wildcard    = "*."
)

// Normalize lower-cases, strips one trailing dot and validates. A leading "*."
// label is allowed.
func Normalize(s string) (string, error) {
	h := strings.TrimSuffix(s, ".")
	if err := validate(h); err != nil {
		return "", fmt.Errorf("invalid hostname %q: %w", s, err)
	}
	// Safe to lower-case only now: validate has rejected everything but ASCII.
	return strings.ToLower(h), nil
}

func validate(h string) error {
	if h == "" {
		return errors.New("empty")
	}
	if len(h) > maxNameLen {
		return fmt.Errorf("longer than %d characters", maxNameLen)
	}
	labels := strings.Split(h, ".")
	if labels[0] == "*" {
		labels = labels[1:]
		if len(labels) < 2 {
			return errors.New("a wildcard needs at least two labels after the *")
		}
	} else if len(labels) < 2 {
		return errors.New("needs at least two labels")
	}
	for _, l := range labels {
		if err := validateLabel(l); err != nil {
			return err
		}
	}
	return nil
}

func validateLabel(l string) error {
	if l == "" {
		return errors.New("empty label")
	}
	if len(l) > maxLabelLen {
		return fmt.Errorf("label %q is longer than %d characters", l, maxLabelLen)
	}
	for _, r := range l {
		if !isLabelChar(r) {
			return fmt.Errorf("label %q contains %q", l, r)
		}
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return fmt.Errorf("label %q starts or ends with a hyphen", l)
	}
	return nil
}

func isLabelChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// IsWildcard reports whether h starts with a "*." label.
func IsWildcard(h string) bool {
	return strings.HasPrefix(h, wildcard)
}

// Compare orders ingress rules: exact names before wildcards, more labels
// first, then lexical. Returns -1, 0 or 1.
//
// The tunnel uses the first matching rule, so sorting with Compare puts a
// specific name ahead of any wildcard that would also match it.
func Compare(a, b string) int {
	if wa, wb := IsWildcard(a), IsWildcard(b); wa != wb {
		if wb {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(labelCount(b), labelCount(a)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// Covers reports whether wildcard pattern "*.x" matches host (any depth below x).
// A wildcard host such as "*.a.x" is covered, but a wildcard never covers itself.
func Covers(pattern, host string) bool {
	if !IsWildcard(pattern) || host == pattern {
		return false
	}
	suffix := pattern[1:]
	return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
}

// MatchZone returns the zone with the longest label-suffix match. For a
// wildcard host the leading "*." label is ignored.
func MatchZone(host string, zones []string) (string, bool) {
	host = strings.TrimPrefix(host, wildcard)
	best := ""
	for _, z := range zones {
		if len(z) > len(best) && inZone(host, z) {
			best = z
		}
	}
	return best, best != ""
}

func inZone(host, zone string) bool {
	return host == zone || strings.HasSuffix(host, "."+zone)
}

// Depth is the number of labels host has below zone; 0 when host is the zone.
// host must belong to zone.
func Depth(host, zone string) int {
	return labelCount(host) - labelCount(zone)
}

// MatchPattern implements the allow/deny policy: "*" matches everything,
// "*.x" matches names below x, anything else must be equal.
func MatchPattern(pattern, host string) bool {
	switch {
	case pattern == "*":
		return true
	case IsWildcard(pattern):
		return Covers(pattern, host)
	default:
		return pattern == host
	}
}

func labelCount(h string) int {
	return strings.Count(h, ".") + 1
}
