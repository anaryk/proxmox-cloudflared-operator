package planner

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	sentinelSuffix = ".invalid"
	installPrefix  = "pco-"
)

// Writer identifies one process that writes the tunnel configuration. Its
// generation and nonce travel inside the configuration in the sentinel rule,
// so a process reading it back can tell a stale or duplicate writer.
type Writer struct {
	InstallID  string
	Generation int
	Nonce      string
}

// TunnelName is the name of the tunnel an install owns.
func TunnelName(installID string) string { return installPrefix + installID }

// DNSMarker is what an install writes into the comment of its DNS records.
func DNSMarker(installID string) string { return "pco:" + installID }

// SentinelHostname returns "g<generation>.<nonce>.pco-<installID>.invalid".
// The .invalid top level domain never resolves, so the rule cannot match real
// traffic, and every label stays well under the 63 characters DNS allows.
func SentinelHostname(w Writer) string {
	return fmt.Sprintf("g%d.%s.%s%s%s", w.Generation, w.Nonce, installPrefix, w.InstallID, sentinelSuffix)
}

// ParseSentinel is the inverse of SentinelHostname; ok is false for any other
// hostname.
func ParseSentinel(host string) (w Writer, ok bool) {
	rest, ok := strings.CutSuffix(host, sentinelSuffix)
	if !ok {
		return Writer{}, false
	}
	labels := strings.Split(rest, ".")
	if len(labels) != 3 {
		return Writer{}, false
	}
	generation, ok := parseGeneration(labels[0])
	if !ok {
		return Writer{}, false
	}
	installID, ok := strings.CutPrefix(labels[2], installPrefix)
	if !ok || !isLowerAlnum(installID) || !isLowerAlnum(labels[1]) {
		return Writer{}, false
	}
	return Writer{InstallID: installID, Generation: generation, Nonce: labels[1]}, true
}

// parseGeneration reads "g<digits>" with no leading zero, except "g0" itself.
func parseGeneration(label string) (int, bool) {
	digits, ok := strings.CutPrefix(label, "g")
	if !ok || !isDigits(digits) || len(digits) > 1 && digits[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

func isDigits(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

func isLowerAlnum(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool {
		return r < '0' || r > '9' && r < 'a' || r > 'z'
	}) < 0
}
