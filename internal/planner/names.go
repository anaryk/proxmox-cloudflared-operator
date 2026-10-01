package planner

import "strings"

// The names here are the ones the planner, the reconcilers and the credential
// check must agree on, so that what one side writes the other recognises.

// ProbeRecordPrefix begins the name of every probe record of the credential
// check, so that a probe left behind can be told from the records that
// publish hostnames.
const ProbeRecordPrefix = "_pco-probe-"

// probeTunnelInfix follows the tunnel name of the install in the name of a
// probe tunnel. A node name has no underscore, so a probe tunnel never looks
// like the tunnel of a node, "pco-<id>-<node>".
const probeTunnelInfix = "_probe_"

// CatchAllRule is the last rule of every tunnel: it answers 404 to whatever no
// rule before it matches.
func CatchAllRule() IngressRule { return IngressRule{Service: notFoundService} }

// SentinelRule is the rule that carries the sentinel of w, right before the
// catch-all. It answers 404, and its hostname never resolves.
func SentinelRule(w Writer) IngressRule {
	return IngressRule{Hostname: SentinelHostname(w), Service: notFoundService}
}

// ProbeRecordComment is the comment of a probe record. It starts with the
// marker of the install, so that anything that sweeps the records of the
// install finds a probe left behind.
func ProbeRecordComment(installID string) string { return DNSMarker(installID) + " probe" }

// ProbeTunnelName is the name of a probe tunnel, "pco-<id>_probe_<suffix>".
// With an empty suffix it is the prefix every probe tunnel of the install
// begins with.
func ProbeTunnelName(installID, suffix string) string {
	return TunnelName(installID) + probeTunnelInfix + suffix
}

// IsProbeTunnel reports whether name is the name of a probe tunnel of the
// install: the prefix of its probe tunnels followed by lower-case letters and
// digits, as a probe suffix is made of.
func IsProbeTunnel(installID, name string) bool {
	suffix, ok := strings.CutPrefix(name, ProbeTunnelName(installID, ""))
	return ok && isLowerAlnum(suffix)
}
