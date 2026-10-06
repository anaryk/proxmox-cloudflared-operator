package appliance

import (
	"context"
	"fmt"
	"io"
	"maps"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Self is the self-identification of the running appliance, which the engine
// asks every cycle: it reads the facts of the container and the uptimes of the
// guests, checks them against the snapshot of the cycle, keeps the identity
// flag and, on the first verdict of the process that passes, decides the
// writer epoch of this start. A copy never gets that far, so it draws nothing,
// not even on its own volume (ruling 23).
type Self struct {
	ID          Identity
	InstallID   string
	Incarnation string
	Store       *store.Store
	// Facts reads what the container shows of itself; Uptimes reads the
	// uptime of every running guest from Proxmox.
	Facts   func() (Facts, error)
	Uptimes func(ctx context.Context) (map[model.GuestRef]time.Duration, error)
	// VerifyError is why the certificate of the Proxmox API failed to verify
	// on the last request, nil when it verified; Endpoint is the endpoint of
	// the install it is reached at.
	VerifyError func() error
	Endpoint    store.Endpoint
	Flag        string    // IdentityFlag
	Rand        io.Reader // where the nonce of a new epoch is drawn from
	Now         func() time.Time
	Log         zerolog.Logger

	mu        sync.Mutex
	firstDiff time.Time
	decided   bool // the epoch of this start is decided
	drawn     bool // and was drawn anew
	segments  map[string]resolve.Segment
}

// Check is the verdict on this container for snap. A copy loses the identity
// flag at once; a verdict that is not OK while the certificate of the API does
// not verify says so; the first verdict that passes decides the epoch, and a
// failure to save it makes the verdict not OK until a later one saves it. A
// verdict with a NIC pending decides nothing while only the uptime proves the
// container.
func (s *Self) Check(ctx context.Context, snap inventory.Snapshot) Verdict {
	f, err := s.Facts()
	if err != nil {
		return Verdict{Why: fmt.Sprintf("the facts of this container could not be read: %v", err)}
	}
	uptimes, err := s.Uptimes(ctx)
	if err != nil {
		s.Log.Debug().Err(err).Msg("reading the uptimes of the guests failed")
		uptimes = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.ID.Check(snap, f, uptimes, s.Now(), &s.firstDiff)
	if snap.Complete {
		s.segments = s.ID.Segments(snap, f.Links)
	}
	switch {
	case v.Copy:
		if err := RemoveFlag(s.Flag); err != nil {
			s.Log.Error().Err(err).Msg("removing the identity flag of a copy failed")
		}
	case !v.OK:
		if verr := s.VerifyError(); verr != nil {
			v.Why = fmt.Sprintf("the certificate of %s no longer verifies under %s (the cluster CA or the pveproxy certificate changed?): "+
				"run pco appliance repair --vmid %d on the node", s.Endpoint.Address, s.Endpoint.ServerName, s.ID.VMID)
		}
	case s.decided:
	case v.Pending && f.Mount.VMID != s.ID.VMID:
		// A clone started within the slack of the uptime passes it too, and
		// its NICs tell it apart only once they are past their minute.
		s.Log.Info().Msg("a NIC of this container is pending and only the uptime proves it; the writer epoch waits")
	default:
		w, kept, err := EpochAtStart(s.Store, s.InstallID, s.Incarnation, s.Rand)
		if err != nil {
			return Verdict{Why: fmt.Sprintf("this container is %s, but its writer epoch could not be decided: %v", s.ID.Ref(), err)}
		}
		s.decided, s.drawn = true, !kept
		s.Log.Info().Int("generation", w.Generation).Bool("kept", kept).Msg("self-identification passed; the writer epoch is decided")
	}
	return v
}

// SetFlag writes the identity flag, or removes it.
func (s *Self) SetFlag(serve bool) error {
	if serve {
		return WriteFlag(s.Flag)
	}
	return RemoveFlag(s.Flag)
}

// EpochDrawn reports whether this process drew a new epoch.
func (s *Self) EpochDrawn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drawn
}

// Segments maps the links of the container to the segments of their NICs, as
// the last complete snapshot showed them; empty until there was one.
func (s *Self) Segments() map[string]resolve.Segment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.segments)
}

// CopyBeforeCycle is the check before the first cycle, which has only the
// facts of the container and no snapshot: a mount that names another VMID is
// a copy, and loses the identity flag; anything else waits for the first
// cycle. It says why it is a copy.
func (s *Self) CopyBeforeCycle() (why string, isCopy bool) {
	f, err := s.Facts()
	if err != nil {
		return "", false
	}
	var none time.Time
	v := s.ID.Check(inventory.Snapshot{}, f, nil, s.Now(), &none)
	if !v.Copy {
		return "", false
	}
	if err := RemoveFlag(s.Flag); err != nil {
		s.Log.Error().Err(err).Msg("removing the identity flag of a copy failed")
	}
	return v.Why, true
}
