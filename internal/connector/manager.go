// Package connector keeps one cloudflared systemd unit running per tunnel. A
// tunnel's token, metrics address and configuration live in files the unit
// reads, so the manager's job is to keep those files and the unit's state in
// line with the tunnels it is told about.
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

var (
	tunnelIDPattern  = regexp.MustCompile(`^[0-9a-f-]{36}$`)
	installIDPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)
)

// Manager keeps the connector of each tunnel running. It is the only writer
// of the files in its directory, and it serialises Ensure and Prune, which
// share those files and the metrics ports.
type Manager struct {
	sd    Systemd
	dir   string
	httpc *http.Client
	log   zerolog.Logger

	firstPort int                               // where metrics ports are allocated from
	lstat     func(string) (fs.FileInfo, error) // os.Lstat, replaceable so that a test can make a stat fail
	readFile  func(string) ([]byte, error)      // os.ReadFile, replaceable so that a test can see what a read holds

	mu     sync.Mutex              // guards the fields below and serialises the work on the files
	queued map[string]pendingState // by tunnel id: the marker a start or restart was queued for
	stuck  map[string]struct{}     // stale temporary files that could not be removed, by path
}

// NewManager returns a manager that keeps the token, env and config files of
// the connectors in dir and probes their metrics endpoints with httpc. A nil
// httpc means a default client.
func NewManager(sd Systemd, dir string, httpc *http.Client, log zerolog.Logger) *Manager {
	if httpc == nil {
		httpc = &http.Client{}
	}
	return &Manager{
		sd: sd, dir: dir, httpc: httpc, log: log,
		firstPort: defaultFirstPort,
		lstat:     os.Lstat,
		readFile:  os.ReadFile,
		queued:    make(map[string]pendingState),
	}
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

// Ensure makes the connector for a tunnel of an install run with the given
// token. It writes the token, env and config files when their content
// differs, starts a unit that does not run, and restarts a running unit whose
// files changed, since cloudflared reads them only at start. The env file names
// the install; one that names another, or none, is rewritten like any other
// change.
//
// A change is recorded in a marker file before the first file is replaced and
// the marker is removed only after the start or restart was queued, so that a
// failure, or a restart of this process, in between does not leave a unit
// running with files it was not started with. A marker that is there makes
// Ensure restart the unit whatever the files hold, once per marker and
// process: when it cannot be removed, Ensure keeps returning that error
// rather than restarting a healthy connector on every call.
//
// The metrics port is the lowest from 20300 that no other env file names. A
// port that an unrelated process on the host holds is not detected: the
// connector then fails to start, and Status shows it not ready.
func (m *Manager) Ensure(ctx context.Context, installID, tunnelID, token string) error {
	if err := checkID(tunnelID); err != nil {
		return err
	}
	if err := checkInstall(installID); err != nil {
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
	m.sweepStaleTemps()
	wrote, err := m.writeFiles(installID, tunnelID, token)
	if err != nil {
		return fmt.Errorf("tunnel %s: %w", tunnelID, err)
	}
	return m.apply(ctx, tunnelID, wrote)
}

// prepareDir creates the directory and restores its mode.
func (m *Manager) prepareDir() error {
	if err := os.MkdirAll(m.dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", m.dir, err)
	}
	if err := fixMode(m.dir, dirMode); err != nil {
		return fmt.Errorf("setting mode of %s: %w", m.dir, err)
	}
	return nil
}

// sweepStaleTemps removes the leftovers of interrupted writes. A leftover that
// cannot be removed is no reason to leave the connector as it is; it is logged,
// by name and never by content, the first time it is seen and again only after
// it was gone and came back, since Ensure runs for every tunnel on every poll.
func (m *Manager) sweepStaleTemps() {
	failing := make(map[string]struct{})
	for _, f := range removeStaleTemps(m.dir) {
		failing[f.name] = struct{}{}
		if _, known := m.stuck[f.name]; !known {
			m.log.Warn().Err(f.err).Str("file", f.name).Msg("could not remove a stale temporary file")
		}
	}
	m.stuck = failing
}

// writeFiles brings the env, config and token file of a tunnel to the wanted
// content and mode, and reports whether it replaced one of them. Their content
// is replaced only when it differs; a mode is fixed in place and is no change
// that needs a restart.
func (m *Manager) writeFiles(installID, id, token string) (wrote bool, err error) {
	envPath, configPath, tokenPath := m.path(envFile(id)), m.path(configFile(id)), m.path(tokenFile(id))
	env, replaceEnv, err := m.wantedEnv(installID, id)
	if err != nil {
		return false, err
	}
	replaceConfig := !hasContent(configPath, []byte(configContent))
	replaceToken := !hasContent(tokenPath, []byte(token))
	if replaceEnv || replaceConfig || replaceToken {
		// What was queued for an earlier marker says nothing about this
		// change, however this call ends: it may fail before it restarts, and
		// the next one must not take the marker for one that was dealt with.
		delete(m.queued, id)
		if err := m.markPending(id); err != nil {
			return false, fmt.Errorf("marking the change as pending: %w", err)
		}
	}
	for _, f := range []struct {
		path    string
		data    []byte
		mode    fs.FileMode
		replace bool
	}{
		{envPath, env, envMode, replaceEnv},
		{configPath, []byte(configContent), configMode, replaceConfig},
		{tokenPath, []byte(token), tokenMode, replaceToken},
	} {
		if f.replace {
			err = writeAtomic(f.path, f.data, f.mode)
		} else {
			err = fixMode(f.path, f.mode)
		}
		if err != nil {
			return false, fmt.Errorf("writing %s: %w", filepath.Base(f.path), err)
		}
	}
	return replaceEnv || replaceConfig || replaceToken, nil
}

// wantedEnv returns the content of the env file of a tunnel and whether the
// file has to be replaced to hold it. An address that is already there keeps
// its port, so that a tunnel's port does not move between runs; it is only
// replaced when it does not name the loopback address. A file that says the
// same in other words, such as with quotes, stays as it is; one written before
// the edge IP version or the install was set is replaced.
func (m *Manager) wantedEnv(installID, id string) (data []byte, replace bool, err error) {
	values, readErr := readEnv(m.path(envFile(id)))
	addr, port, err := metricsOf(values)
	if readErr != nil || err != nil {
		if port, err = m.freePort(id); err != nil {
			return nil, false, err
		}
		return envContent(port, installID), true, nil
	}
	replace = addr != metricsAddr(port) || values[edgeKey] != edgeIPVersion || values[installKey] != installID
	return envContent(port, installID), replace, nil
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
		port, err := readMetricsPort(m.path(name))
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

// Prune stops and removes the connectors of tunnels not in keep, whichever
// install they are of: it is what pco uninstall does. The daemon prunes with
// PruneInstall, which leaves the connectors of other installs alone.
//
// It finds them by their files and by their loaded units, so that a connector
// is removed even when some of them are gone. An empty keep removes every
// connector; the caller decides when it knows enough to ask for that. An id in
// keep that is no tunnel id is an error and nothing is removed, since it could
// be a tunnel that was meant to stay.
//
// A failure on one connector does not stop the others; the errors come back
// joined. A connector whose stop could not be queued keeps its files, since
// its unit still reads them. Names that are no tunnel id are never passed to
// systemd and never removed; they are reported.
func (m *Manager) Prune(ctx context.Context, keep []string) error {
	return m.prune(ctx, keep, func(string) (bool, error) { return true, nil })
}

// PruneInstall is Prune for the connectors whose env file names installID: a
// connector of another install, or of none, is never touched, as it may serve
// tunnels this install knows nothing of. List finds those.
func (m *Manager) PruneInstall(ctx context.Context, installID string, keep []string) error {
	if err := checkInstall(installID); err != nil {
		return err
	}
	return m.prune(ctx, keep, func(id string) (bool, error) {
		values, err := readEnv(m.path(envFile(id)))
		if err != nil {
			return false, fmt.Errorf("tunnel %s: reading env file: %w", id, err)
		}
		return values[installKey] == installID, nil
	})
}

// prune removes the connectors not in keep that ours says are to go.
func (m *Manager) prune(ctx context.Context, keep []string, ours func(id string) (bool, error)) error {
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
	for _, f := range removeStaleTemps(m.dir) {
		errs = append(errs, f.err)
	}
	ids, ignored, err := m.discover(ctx)
	errs = append(errs, ignored...)
	errs = append(errs, err)
	for _, id := range ids {
		if slices.Contains(keep, id) {
			continue
		}
		switch remove, err := ours(id); {
		case err != nil:
			errs = append(errs, err)
			continue
		case !remove:
			continue
		}
		if err := m.remove(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// List returns the sorted ids of the connectors on the node, of every
// install: those that have files, a loaded unit or a marker. The error says
// what could not be looked at; what the rest showed is returned all the same.
// A name that looks like a connector's but is not is left to Prune to report.
func (m *Manager) List(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids, _, err := m.discover(ctx)
	return ids, err
}

// discover returns the sorted ids of the connectors that have files or a
// loaded unit or a marker, an error for each name that looked like one but is
// not, and what could not be listed. Other hidden files are the manager's own
// and not connectors.
func (m *Manager) discover(ctx context.Context) (ids []string, ignored []error, err error) {
	var failed []error
	found := make(map[string]struct{})

	entries, err := os.ReadDir(m.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		failed = append(failed, fmt.Errorf("listing %s: %w", m.dir, err))
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		if id, ok := markerID(name); ok {
			// The marker of a change that never got its files or its unit.
			if checkID(id) == nil {
				found[id] = struct{}{}
			}
			continue
		}
		var id string
		ok := false
		for _, ext := range []string{tokenExt, envExt, configExt} {
			if id, ok = strings.CutSuffix(name, ext); ok {
				break
			}
		}
		if !ok || hidden(name) {
			continue
		}
		if err := checkID(id); err != nil {
			ignored = append(ignored, fmt.Errorf("ignoring %s: %w", m.path(name), err))
			continue
		}
		found[id] = struct{}{}
	}

	units, err := m.sd.ListUnits(ctx, unitGlob)
	if err != nil {
		failed = append(failed, fmt.Errorf("listing units: %w", err))
	}
	for _, unit := range units {
		id := strings.TrimSuffix(strings.TrimPrefix(unit, unitPrefix), unitSuffix)
		if err := checkID(id); err != nil || UnitName(id) != unit {
			ignored = append(ignored, fmt.Errorf("ignoring unit %s: not the connector of a tunnel id", unit))
			continue
		}
		found[id] = struct{}{}
	}
	return slices.Sorted(maps.Keys(found)), ignored, errors.Join(failed...)
}

// remove queues the stop of the connector and then removes its files. A
// connector that is half removed is found again through whichever file is
// left, and the env file, which names its install, goes only once the others
// are gone, so that what is left is still known to be of that install.
func (m *Manager) remove(ctx context.Context, id string) error {
	unit := UnitName(id)
	if err := m.sd.DisableNow(ctx, unit); err != nil {
		return fmt.Errorf("stopping %s: %w", unit, err)
	}
	delete(m.queued, id)
	var errs []error
	for _, name := range []string{pendingFile(id), configFile(id), tokenFile(id)} {
		if err := m.removeFile(name); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		errs = append(errs, m.removeFile(envFile(id)))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	m.log.Info().Str("tunnel", id).Msg("removed connector")
	return nil
}

func (m *Manager) removeFile(name string) error {
	if err := os.Remove(m.path(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", m.path(name), err)
	}
	return nil
}
