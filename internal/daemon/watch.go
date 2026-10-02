package daemon

import (
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// wired is what the daemon takes from the settings once, when it starts: the
// gate tag the inventory watches and the settings of the resolver. The rest of
// the settings is read afresh by every cycle, and a cycle reports a change to
// these as a problem until the daemon is restarted.
type wired struct {
	gateTag      string
	trustStatic  bool
	trustedCIDRs []netip.Prefix
}

func wiredFrom(s store.Settings) wired {
	return wired{gateTag: s.GateTag, trustStatic: s.TrustStatic, trustedCIDRs: slices.Clone(s.TrustedCIDRs)}
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
	return out
}
