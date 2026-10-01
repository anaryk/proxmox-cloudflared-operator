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

	// defaultFirstPort is where metrics ports are allocated from.
	defaultFirstPort = 20300
)

var tunnelIDPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// Manager keeps the connector of each tunnel running. It is the only writer
// of the files in its directory, and it serialises Ensure and Prune, which
// share those files and the metrics ports.
type Manager struct {
	sd    Systemd
	dir   string
	httpc *http.Client
	log   zerolog.Logger

	firstPort int // where metrics ports are allocated from
	mu        sync.Mutex
}

// NewManager returns a manager that keeps the token and env files of the
// connectors in dir and probes their metrics endpoints with httpc. A nil httpc
// means a default client.
func NewManager(sd Systemd, dir string, httpc *http.Client, log zerolog.Logger) *Manager {
	if httpc == nil {
		httpc = &http.Client{}
	}
	return &Manager{sd: sd, dir: dir, httpc: httpc, log: log, firstPort: defaultFirstPort}
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
// not run, and restarts a running unit whose files changed, since cloudflared
// reads them only at start.
//
// A change is recorded in a marker file before the first file is replaced and
// the marker is removed only after the start or restart was queued, so that a
// failure, or a restart of this process, in between does not leave a unit
// running with files it was not started with. A marker that is there makes
// Ensure restart the unit whatever the files hold.
//
// The metrics port is the lowest from 20300 that no other env file names. A
// port that an unrelated process on the host holds is not detected: the
// connector then fails to start, and Status shows it not ready.
func (m *Manager) Ensure(ctx context.Context, tunnelID, token string) error {
	if err := checkID(tunnelID); err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("tunnel %s: empty token", tunnelID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.prepareDir(); err != nil {
		return err
	}
	if err := m.writeFiles(tunnelID, token); err != nil {
		return fmt.Errorf("tunnel %s: %w", tunnelID, err)
	}
	return m.apply(ctx, tunnelID)
}

// prepareDir creates the directory, restores its mode and removes the
// leftovers of interrupted writes.
func (m *Manager) prepareDir() error {
	if err := os.MkdirAll(m.dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", m.dir, err)
	}
	if err := fixMode(m.dir, dirMode); err != nil {
		return fmt.Errorf("setting mode of %s: %w", m.dir, err)
	}
	return removeStaleTemps(m.dir)
}

// writeFiles brings the env and token file of a tunnel to the wanted content
// and mode. Their content is replaced only when it differs; a mode is fixed
// in place and is no change that needs a restart.
func (m *Manager) writeFiles(id, token string) error {
	envPath, tokenPath := m.path(envFile(id)), m.path(tokenFile(id))
	env, replaceEnv, err := m.wantedEnv(id)
	if err != nil {
		return err
	}
	replaceToken := !hasContent(tokenPath, []byte(token))
	if replaceEnv || replaceToken {
		if err := m.markPending(id); err != nil {
			return fmt.Errorf("marking the change as pending: %w", err)
		}
	}
	for _, f := range []struct {
		path    string
		data    []byte
		mode    fs.FileMode
		replace bool
	}{
		{envPath, env, envMode, replaceEnv},
		{tokenPath, []byte(token), tokenMode, replaceToken},
	} {
		if f.replace {
			err = writeAtomic(f.path, f.data, f.mode)
		} else {
			err = fixMode(f.path, f.mode)
		}
		if err != nil {
			return fmt.Errorf("writing %s: %w", filepath.Base(f.path), err)
		}
	}
	return nil
}

// wantedEnv returns the content of the env file of a tunnel and whether the
// file has to be replaced to hold it. An address that is already there keeps
// its port, so that a tunnel's port does not move between runs; it is only
// replaced when it does not name the loopback address. A file that says the
// same address in other words, such as with quotes, stays as it is.
func (m *Manager) wantedEnv(id string) (data []byte, replace bool, err error) {
	addr, port, err := readMetricsAddr(m.path(envFile(id)))
	if err != nil {
		if port, err = m.freePort(id); err != nil {
			return nil, false, err
		}
		return envContent(port), true, nil
	}
	return envContent(port), addr != metricsAddr(port), nil
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
		if !e.Type().IsRegular() || hidden(name) || !strings.HasSuffix(name, envExt) || name == envFile(id) {
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
	port := m.firstPort
	for used[port] {
		port++
	}
	if port > maxPort {
		return 0, errors.New("no free metrics port")
	}
	return port, nil
}

func (m *Manager) markPending(id string) error {
	if m.isPending(id) {
		return nil
	}
	return writeAtomic(m.path(pendingFile(id)), nil, pendingMode)
}

// isPending reports whether the unit of a tunnel still has to be started or
// restarted for the files on disk. A marker that cannot be told to be absent
// counts as there: an extra restart is better than a stale token.
func (m *Manager) isPending(id string) bool {
	_, err := os.Lstat(m.path(pendingFile(id)))
	return !errors.Is(err, fs.ErrNotExist)
}

func (m *Manager) clearPending(id string) error {
	if err := os.Remove(m.path(pendingFile(id))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// apply starts a unit that does not run, restarts one whose files changed,
// and then clears the marker of the change.
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
	case m.isPending(id):
		if err := m.sd.Restart(ctx, unit); err != nil {
			return fmt.Errorf("restarting %s: %w", unit, err)
		}
		m.log.Info().Str("tunnel", id).Msg("restarted connector after a change")
	default:
		return nil
	}
	if err := m.clearPending(id); err != nil {
		return fmt.Errorf("tunnel %s: clearing the pending marker: %w", id, err)
	}
	return nil
}

// Prune stops and removes the connectors of tunnels not in keep. It finds them
// by their token and env files and by their loaded units, so that a connector
// is removed even when one of the two is gone. An empty keep removes every
// connector; the caller decides when it knows enough to ask for that. An id in
// keep that is no tunnel id is an error and nothing is removed, since it could
// be a tunnel that was meant to stay.
//
// A failure on one connector does not stop the others; the errors come back
// joined. A connector whose stop could not be queued keeps its files, since
// its unit still reads them. Names that are no tunnel id are never passed to
// systemd and never removed; they are reported.
func (m *Manager) Prune(ctx context.Context, keep []string) error {
	var invalid []error
	for _, id := range keep {
		if err := checkID(id); err != nil {
			invalid = append(invalid, fmt.Errorf("keep: %w", err))
		}
	}
	if len(invalid) > 0 {
		return errors.Join(invalid...)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	if err := removeStaleTemps(m.dir); err != nil {
		errs = append(errs, err)
	}
	ids, found := m.discover(ctx)
	errs = append(errs, found...)
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
// The hidden files of the manager are not connectors.
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
		if !ok || hidden(name) || !e.Type().IsRegular() {
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

// remove queues the stop of the connector and then removes its files, the
// token last: a connector that is half removed is found again through
// whichever file is left.
func (m *Manager) remove(ctx context.Context, id string) error {
	unit := UnitName(id)
	if err := m.sd.DisableNow(ctx, unit); err != nil {
		return fmt.Errorf("stopping %s: %w", unit, err)
	}
	var errs []error
	for _, name := range []string{pendingFile(id), envFile(id), tokenFile(id)} {
		if err := os.Remove(m.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing %s: %w", m.path(name), err))
		}
	}
	m.log.Info().Str("tunnel", id).Msg("removed connector")
	return errors.Join(errs...)
}
