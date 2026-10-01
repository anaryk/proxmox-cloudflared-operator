package reconcile

import (
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// JudgeWriter applies the decision table. remote is the sentinel found in the
// remote config (ok=false when there is none); stored is leader.json as re-read
// just now. A sentinel of another install id counts as none.
func JudgeWriter(us planner.Writer, remote planner.Writer, remoteOK bool, stored planner.Writer) WriterVerdict {
	switch {
	case !remoteOK || remote.InstallID != us.InstallID || remote.Generation < us.Generation:
		return WriterProceed
	case remote.Generation == us.Generation:
		if remote.Nonce == us.Nonce {
			return WriterProceed
		}
		return WriterForeign
	case stored.Generation > remote.Generation,
		stored.Generation == remote.Generation && stored.Nonce == remote.Nonce:
		// leader.json knows the writer of the remote config, or one after it.
		return WriterStale
	}
	return WriterForeign
}

// EqualIngress compares rule lists semantically (order matters, empty option
// fields equal to absent).
//
// An IngressRule holds every option as a plain value, so an option that is
// absent and one that is empty are the same value and the rules compare field
// by field.
func EqualIngress(a, b []planner.IngressRule) bool {
	return slices.Equal(a, b)
}

// judgeConfig judges every sentinel of this install in rules and returns the
// gravest verdict with the writer that caused it. pco writes one sentinel, but
// an edited configuration may hold several.
func judgeConfig(us, stored planner.Writer, rules []planner.IngressRule) (WriterVerdict, planner.Writer) {
	verdict, by := WriterProceed, planner.Writer{}
	for _, rule := range rules {
		remote, ok := planner.ParseSentinel(rule.Hostname)
		v := JudgeWriter(us, remote, ok, stored)
		// Another installation is an alert, which outranks re-acquiring.
		if v == WriterForeign && verdict != WriterForeign || v == WriterStale && verdict == WriterProceed {
			verdict, by = v, remote
		}
	}
	return verdict, by
}
