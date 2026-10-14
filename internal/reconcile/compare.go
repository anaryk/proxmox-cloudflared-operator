package reconcile

import (
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// keepServed is planned with every rule of remote that serves a hostname
// planned leaves out or answers 503 for, in the order the planner gives
// rules, and the rules it took from remote. The sentinels of remote are not
// taken: planned carries the one of this writer.
func keepServed(planned, remote []planner.IngressRule) (rules, kept []planner.IngressRule) {
	at := map[string]int{}
	var body, tail []planner.IngressRule
	for _, r := range planned {
		if !servesHost(r) {
			tail = append(tail, r)
			continue
		}
		at[strings.ToLower(r.Hostname)] = len(body)
		body = append(body, r)
	}
	for _, r := range remote {
		if !servesHost(r) || r.Service == planner.BlockedService {
			continue
		}
		host := strings.ToLower(r.Hostname)
		i, ok := at[host]
		switch {
		case !ok:
			at[host] = len(body)
			body = append(body, r)
		case body[i].Service == planner.BlockedService:
			body[i] = r
		default:
			continue
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return planned, nil
	}
	slices.SortStableFunc(body, func(x, y planner.IngressRule) int { return hostname.Compare(x.Hostname, y.Hostname) })
	return append(body, tail...), kept
}

// servesHost says whether a rule is one of a hostname: not the catch-all,
// and not a sentinel, whose names are in the .invalid domain.
func servesHost(r planner.IngressRule) bool {
	return r.Hostname != "" && !strings.HasSuffix(strings.ToLower(r.Hostname), ".invalid")
}

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
