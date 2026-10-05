package planner

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	sentinelSuffix = ".invalid"
	installPrefix  = "pco-"
	maxLabelLen    = 63
)

// Writer identifies one process that writes the tunnel configuration. Its
// generation and nonce travel inside the configuration in the sentinel rule,
// so a process reading it back can tell a stale or duplicate writer.
type Writer struct {
	InstallID  string `json:"installId"`
	Generation int    `json:"generation"`
	Nonce      string `json:"nonce"`
	// Incarnation is the start of the appliance container that drew the
	// writer, "<boot id>/<start of PID 1>". It stays in leader.json and never
	// reaches the sentinel. A host leaves it empty.
	Incarnation string `json:"incarnation,omitempty"`
}

// Validate reports whether w can be written into a sentinel that ParseSentinel
// reads back: install id and nonce are non-empty lower-case letters and
// digits, the generation is not negative, and every label of the sentinel
// fits the 63 characters DNS allows. The incarnation is not looked at.
func (w Writer) Validate() error {
	switch {
	case !isLowerAlnum(w.InstallID):
		return fmt.Errorf("writer install id %q: want lower-case letters and digits", w.InstallID)
	case !isLowerAlnum(w.Nonce):
		return fmt.Errorf("writer nonce %q: want lower-case letters and digits", w.Nonce)
	case w.Generation < 0:
		return fmt.Errorf("writer generation %d is negative", w.Generation)
	}
	for _, label := range strings.Split(SentinelHostname(w), ".") {
		if len(label) > maxLabelLen {
			return fmt.Errorf("writer sentinel label %q is longer than %d characters", label, maxLabelLen)
		}
	}
	return nil
}

// TunnelName is the name of the tunnel an install owns.
func TunnelName(installID string) string { return installPrefix + installID }

// DNSMarker is what an install writes into the comment of its DNS records.
func DNSMarker(installID string) string { return "pco:" + installID }

// SentinelHostname returns "g<generation>.<nonce>.pco-<installID>.invalid".
// The .invalid top level domain never resolves, so the rule cannot match real
// traffic. The labels fit DNS only for a writer that passes Validate.
func SentinelHostname(w Writer) string {
	return fmt.Sprintf("g%d.%s.%s%s%s", w.Generation, w.Nonce, installPrefix, w.InstallID, sentinelSuffix)
}

// reserved reports whether host lies in the .invalid top level domain, which
// holds every sentinel. Such a name is never served nor published.
func reserved(host string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), sentinelSuffix)
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
