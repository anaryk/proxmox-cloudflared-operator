package reconcile

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// Owned reports whether a DNS record belongs to the install: its comment
// begins with the marker of the install as a whole word, so that the records
// of an install whose id merely begins with this one are not taken for ours.
// A probe record is owned too; see IsProbeRecord.
func Owned(installID string, rec cfapi.Record) bool {
	rest, ok := strings.CutPrefix(rec.Comment, planner.DNSMarker(installID))
	if !ok {
		return false
	}
	next, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || unicode.IsSpace(next)
}

// IsProbeRecord reports whether a DNS record is one the credential check of
// the install made: a TXT record named like a probe that carries exactly the
// probe comment. Anything else with that comment is an ordinary record.
func IsProbeRecord(installID string, rec cfapi.Record) bool {
	return strings.EqualFold(rec.Type, "TXT") &&
		strings.HasPrefix(strings.ToLower(rec.Name), planner.ProbeRecordPrefix) &&
		rec.Comment == planner.ProbeRecordComment(installID)
}
