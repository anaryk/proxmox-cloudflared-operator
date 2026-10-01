// Package connector keeps one cloudflared systemd unit running per tunnel. A
// tunnel's token and metrics address live in files the unit reads, so the
// manager's job is to keep those files and the unit's state in line with the
// tunnels it is told about.
package connector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/rs/zerolog"
)

const (
	unitPrefix = "pco-cloudflared@"
	unitSuffix = ".service"
	unitGlob   = unitPrefix + "*" + unitSuffix

	// firstPort is where metrics ports are allocated from.
	firstPort = 20300
)

var tunnelIDPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// Manager keeps the connector of each tunnel running. It is the only writer
// of the files in its directory.
type Manager struct {
	sd    Systemd
	dir   string
	httpc *http.Client
	log   zerolog.Logger

	mu sync.Mutex // serialises Ensure and Prune, which share the files and the ports
	// unapplied holds the tunnels whose token was written but that no unit has
	// been started or restarted with since. The files alone cannot tell, once
	// the new token is on disk, so a failure between the write and the restart
	// would otherwise leave an old token running for good.
	unapplied map[string]bool
}

// NewManager returns a manager that keeps the token and env files of the
// connectors in dir and probes their metrics endpoints with httpc. A nil httpc
// means a default client.
func NewManager(sd Systemd, dir string, httpc *http.Client, log zerolog.Logger) *Manager {
	if httpc == nil {
		httpc = &http.Client{}
	}
	return &Manager{sd: sd, dir: dir, httpc: httpc, log: log, unapplied: make(map[string]bool)}
}

// UnitName is the systemd unit that runs the connector of a tunnel.
func UnitName(tunnelID string) string { return unitPrefix + tunnelID + unitSuffix }

func checkID(id string) error {
	if !tunnelIDPattern.MatchString(id) {
		return fmt.Errorf("invalid tunnel id %q", id)
	}
	return nil
}

func (m *Manager) path(name string) string { return filepath.Join(m.dir, name) }

// Ensure makes the connector for a tunnel run with the given token. It writes
// the token and env files when their content differs, starts a unit that does
// not run and restarts one that runs with another token.
func (m *Manager) Ensure(ctx context.Context, tunnelID, token string) error {
	if err := checkID(tunnelID); err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("tunnel %s: empty token", tunnelID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(m.dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", m.dir, err)
	}
	// The env file comes first: when the token cannot be written, nothing the
	// running unit depends on has changed yet.
	if err := m.writeEnv(tunnelID); err != nil {
		return fmt.Errorf("tunnel %s: %w", tunnelID, err)
	}
	changed, err := replaceIfChanged(m.path(tokenFile(tunnelID)), []byte(token), tokenMode)
	if err != nil {
		return fmt.Errorf("tunnel %s: writing token file: %w", tunnelID, err)
	}
	if changed {
		m.unapplied[tunnelID] = true
	}
	return m.apply(ctx, tunnelID)
}

// writeEnv gives the tunnel its env file. An address that is already there
// keeps its port, so that a tunnel's port does not move between runs.
func (m *Manager) writeEnv(id string) error {
	path := m.path(envFile(id))
	_, port, err := readMetricsAddr(path)
	if err != nil {
		if port, err = m.freePort(id); err != nil {
			return err
		}
	}
	if _, err := replaceIfChanged(path, envContent(port), envMode); err != nil {
		return fmt.Errorf("writing env file: %w", err)
	}
	return nil
}

// freePort returns the lowest port from firstPort that no env file of another
// tunnel names. An env file without a valid address names none and is left as
// it is; one that cannot be read may hold any port, so allocating fails.
func (m *Manager) freePort(id string) (int, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0, fmt.Errorf("listing %s: %w", m.dir, err)
	}
	used := make(map[int]bool)
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, envExt) || name == envFile(id) {
			continue
		}
		_, port, err := readMetricsAddr(m.path(name))
		switch {
		case errors.Is(err, errNoAddress):
		case err != nil:
			return 0, fmt.Errorf("allocating a metrics port: %w", err)
		default:
			used[port] = true
		}
	}
	port := firstPort
	for used[port] {
		port++
	}
	return port, nil
}

// apply brings the unit in line with the files: it starts a unit that does not
// run and restarts one that runs with a token that is not the one on disk.
func (m *Manager) apply(ctx context.Context, id string) error {
	unit := UnitName(id)
	active, err := m.sd.IsActive(ctx, unit)
	if err != nil {
		return fmt.Errorf("checking %s: %w", unit, err)
	}
	switch {
	case !active:
		if err := m.sd.EnableNow(ctx, unit); err != nil {
			return fmt.Errorf("starting %s: %w", unit, err)
		}
		m.log.Info().Str("tunnel", id).Msg("started connector")
	case m.unapplied[id]:
		if err := m.sd.Restart(ctx, unit); err != nil {
			return fmt.Errorf("restarting %s: %w", unit, err)
		}
		m.log.Info().Str("tunnel", id).Msg("restarted connector with a new token")
	}
	delete(m.unapplied, id)
	return nil
}

// Prune stops and removes the connectors of tunnels not in keep. It finds them
// by their token and env files and by their loaded units, so that a connector
// is removed even when one of the two is gone. An empty keep removes every
// connector; the caller decides when it knows enough to ask for that.
//
// A failure on one connector does not stop the others; the errors come back
// joined. A connector whose unit would not stop keeps its files, since the
// unit still reads them. Names that are no tunnel id are never passed to
// systemd and never removed; they are reported.
func (m *Manager) Prune(ctx context.Context, keep []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids, errs := m.discover(ctx)
	for _, id := range ids {
		if slices.Contains(keep, id) {
			continue
		}
		if err := m.remove(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// discover returns the sorted ids of the connectors that have files or a
// loaded unit, and an error for each name that looked like one but is not.
func (m *Manager) discover(ctx context.Context) ([]string, []error) {
	var errs []error
	found := make(map[string]struct{})

	entries, err := os.ReadDir(m.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("listing %s: %w", m.dir, err))
	}
	for _, e := range entries {
		name := e.Name()
		id, ok := strings.CutSuffix(name, tokenExt)
		if !ok {
			id, ok = strings.CutSuffix(name, envExt)
		}
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if err := checkID(id); err != nil {
			errs = append(errs, fmt.Errorf("ignoring %s: %w", m.path(name), err))
			continue
		}
		found[id] = struct{}{}
	}

	units, err := m.sd.ListUnits(ctx, unitGlob)
	if err != nil {
		errs = append(errs, fmt.Errorf("listing units: %w", err))
	}
	for _, unit := range units {
		id := strings.TrimSuffix(strings.TrimPrefix(unit, unitPrefix), unitSuffix)
		if err := checkID(id); err != nil || UnitName(id) != unit {
			errs = append(errs, fmt.Errorf("ignoring unit %s: not the connector of a tunnel id", unit))
			continue
		}
		found[id] = struct{}{}
	}
	return slices.Sorted(maps.Keys(found)), errs
}

// remove stops the connector and then removes its files, the token last: a
// connector that is half removed is found again through whichever file is left.
func (m *Manager) remove(ctx context.Context, id string) error {
	unit := UnitName(id)
	if err := m.sd.DisableNow(ctx, unit); err != nil {
		return fmt.Errorf("stopping %s: %w", unit, err)
	}
	var errs []error
	for _, name := range []string{envFile(id), tokenFile(id)} {
		if err := os.Remove(m.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing %s: %w", m.path(name), err))
		}
	}
	delete(m.unapplied, id)
	m.log.Info().Str("tunnel", id).Msg("removed connector")
	return errors.Join(errs...)
}
