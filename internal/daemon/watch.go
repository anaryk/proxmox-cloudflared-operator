package daemon

import (
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// wired is what the daemon takes from the settings once, when it starts: the
// gate tag the inventory watches, the settings of the resolver and the budget
// of the Cloudflare clients. The rest of the settings is read afresh by every
// cycle, and a cycle reports a change to these as a problem until the daemon
// is restarted.
type wired struct {
	gateTag          string
	trustStatic      bool
	trustedCIDRs     []netip.Prefix
	cloudflareBudget int
}

func wiredFrom(s store.Settings) wired {
	return wired{gateTag: s.GateTag, trustStatic: s.TrustStatic, trustedCIDRs: slices.Clone(s.TrustedCIDRs), cloudflareBudget: s.CloudflareBudget}
}

// wiredFields names every setting that is wired at start, as differences
// names them: those in which a wired that differs from the zero one in every
// field differs from it.
func wiredFields() []string {
	every := wired{gateTag: "changed", trustStatic: true, trustedCIDRs: []netip.Prefix{{}}, cloudflareBudget: 1}
	return wired{}.differences(every)
}

// differences names the settings in which o is not what w was started with.
func (w wired) differences(o wired) []string {
	var out []string
	if w.gateTag != o.gateTag {
		out = append(out, "gateTag")
	}
	if w.trustStatic != o.trustStatic {
		out = append(out, "trustStatic")
	}
	if !slices.Equal(w.trustedCIDRs, o.trustedCIDRs) {
		out = append(out, "trustedCIDRs")
	}
	if w.cloudflareBudget != o.cloudflareBudget {
		out = append(out, "cloudflareBudget")
	}
	return out
}
