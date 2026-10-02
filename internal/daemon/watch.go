package daemon

import (
	"context"
	"net/netip"
	"slices"
	"sync"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// wired is what the daemon takes from the settings once, when it starts: the
// gate tag the inventory watches and the settings of the resolver. The rest of
// the settings is read afresh by every cycle.
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

// settingsWatch is the inventory of the engine, which looks at the settings
// before every refresh: a cycle refreshes the inventory right after it has read
// the settings, so this is the moment a cycle sees a change. A change to what
// was wired at start is not applied; the watch says that a restart is needed,
// once for each change.
type settingsWatch struct {
	inner engine.Inventory
	store *store.Store
	log   zerolog.Logger
	start wired

	mu     sync.Mutex
	warned []string // what the last warning was about
}

func newSettingsWatch(inner engine.Inventory, s *store.Store, start wired, log zerolog.Logger) *settingsWatch {
	return &settingsWatch{inner: inner, store: s, log: log, start: start}
}

func (w *settingsWatch) Refresh(ctx context.Context) inventory.Snapshot {
	w.check()
	return w.inner.Refresh(ctx)
}

// check reads the settings. Settings that cannot be read are the cycle's to
// report.
func (w *settingsWatch) check() {
	s, err := w.store.Settings()
	if err != nil {
		return
	}
	changed := w.start.differences(wiredFrom(s))

	w.mu.Lock()
	defer w.mu.Unlock()
	if slices.Equal(changed, w.warned) {
		return
	}
	w.warned = changed
	if len(changed) > 0 {
		w.log.Warn().Strs("settings", changed).
			Msg("settings that are read only at start have changed; restart pco for them to take effect")
	}
}
