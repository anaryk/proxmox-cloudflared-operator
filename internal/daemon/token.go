package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
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

// applianceState is what the daemon of an appliance needs of its volume and
// of the container to start: the install with its appliance block, the
// Proxmox API token and the incarnation of this start.
type applianceState struct {
	install     store.Install
	token       store.PVEToken
	incarnation string
}

// waitForState looks at the volume until it holds a state the appliance can
// run on. Until it does, the socket answers through a noStateEngine with the
// line that says what is missing, the volume is looked at again every
// StateRetry, and nothing is written: no install id is made, and no epoch.
// It returns once the state is there, with the stub stopped, or with ctx's
// error.
func waitForState(ctx context.Context, cfg Config, deps Deps, app ApplianceDeps, st *store.Store) (applianceState, error) {
	var stub *stubServer
	defer func() {
		if stub != nil {
			stub.stop(cfg.Log)
		}
	}()
	said := ""
	for {
		s, line := lookForState(st, app, cfg.Paths.Local)
		if line == "" {
			return s, nil
		}
		if line != said {
			cfg.Log.Warn().Msg(line)
			said = line
		}
		if stub == nil {
			stub = startStub(ctx, cfg, deps, line)
		} else {
			stub.eng.look(line, deps.Now())
		}
		if err := deps.Sleep(ctx, app.StateRetry); err != nil {
			return applianceState{}, err
		}
	}
}

// lookForState reads the state of the appliance, or says in a line what it
// lacks and what to do about it.
func lookForState(st *store.Store, app ApplianceDeps, local string) (applianceState, string) {
	vmid := app.System.VMIDHint(local)
	noState := "pco has no state on its volume (restore?): run pco appliance repair --vmid " + vmid + " on the node"
	inst, found, err := st.Install()
	switch {
	case err != nil:
		return applianceState{}, fmt.Sprintf("reading the install on %s: %v", local, err)
	case !found:
		return applianceState{}, noState
	case inst.Appliance == nil:
		return applianceState{}, fmt.Sprintf("the install on %s is not one of an appliance: run pco appliance repair --vmid %s on the node", local, vmid)
	}
	token, found, err := st.PVEToken()
	switch {
	case err != nil:
		return applianceState{}, fmt.Sprintf("reading the Proxmox API token on %s: %v", local, err)
	case !found:
		return applianceState{}, noState
	}
	incarnation, err := app.System.Incarnation()
	if err != nil {
		return applianceState{}, fmt.Sprintf("the incarnation of this container cannot be read (%v); nothing is done until it can", err)
	}
	return applianceState{install: inst, token: token, incarnation: incarnation}, ""
}

// stubServer is the API on the socket while the appliance has no state.
type stubServer struct {
	eng    *noStateEngine
	cancel context.CancelFunc
	done   chan error
}

// startStub serves the socket through a noStateEngine that says line. Its
// readiness is the daemon's: the unit has started, and says why it does
// nothing.
func startStub(ctx context.Context, cfg Config, deps Deps, line string) *stubServer {
	eng := newNoStateEngine(line, deps.Now(), deps.Appliance.withDefaults().StateRetry)
	gid, uids := socketAccess(deps.Accounts, cfg.Log)
	srv := api.New(eng, cfg.Version, uids, cfg.Log)
	srv.SetShutdownTimeout(deps.ShutdownTimeout)
	srv.OnListening(func() {
		if err := deps.Notifier.Ready(); err != nil {
			cfg.Log.Warn().Err(err).Msg("telling systemd that the daemon is ready failed")
		}
	})
	ctx, cancel := context.WithCancel(ctx)
	s := &stubServer{eng: eng, cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- srv.Serve(ctx, cfg.SocketPath, gid) }()
	return s
}

// stop ends the stub and waits until its socket is given up.
func (s *stubServer) stop(log zerolog.Logger) {
	s.cancel()
	if err := <-s.done; err != nil && !errors.Is(err, context.DeadlineExceeded) {
		log.Warn().Err(err).Msg("the socket of a daemon without state stopped with an error")
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
