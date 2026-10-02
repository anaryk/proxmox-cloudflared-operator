package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// The cluster filesystem may not be up yet at boot, so a token that cannot
	// be read for that reason is read again.
	tokenRetryEvery = 10 * time.Second
	tokenRetryFor   = 2 * time.Minute
)

// readPVEToken reads the Proxmox API token of the daemon. Only the token is
// needed to start: a store that is not set up or not mounted is no reason to
// stop anywhere else, as the engine says so in its state, but without the token
// there is nothing to read the guests with.
func readPVEToken(ctx context.Context, s *store.Store, sleep func(context.Context, time.Duration) error, log zerolog.Logger) (store.PVEToken, error) {
	for waited := time.Duration(0); ; waited += tokenRetryEvery {
		tok, found, err := s.PVEToken()
		switch {
		case err == nil && found:
			return tok, nil
		case err == nil:
			return store.PVEToken{}, errors.New("no Proxmox API token found; run pco setup")
		case errors.Is(err, store.ErrNoRoot):
			return store.PVEToken{}, errors.New("pco is not set up on this node; run pco setup")
		case !errors.Is(err, store.ErrNotMounted):
			return store.PVEToken{}, fmt.Errorf("reading the Proxmox API token: %w", err)
		case waited >= tokenRetryFor:
			return store.PVEToken{}, fmt.Errorf("the cluster filesystem is still not mounted after %s; giving up", tokenRetryFor)
		}
		log.Warn().Dur("retry_in", tokenRetryEvery).Msg("the cluster filesystem is not mounted; waiting for it to read the Proxmox API token")
		if err := sleep(ctx, tokenRetryEvery); err != nil {
			return store.PVEToken{}, fmt.Errorf("waiting for the cluster filesystem: %w", err)
		}
	}
}

// sleepContext waits for d, or for ctx to end.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
