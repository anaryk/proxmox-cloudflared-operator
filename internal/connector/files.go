package connector

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	tokenExt   = ".token"
	envExt     = ".env"
	pendingExt = ".pending"
	tempExt    = ".tmp"

	dirMode     fs.FileMode = 0o700
	tokenMode   fs.FileMode = 0o600
	envMode     fs.FileMode = 0o644
	pendingMode fs.FileMode = 0o600

	metricsKey  = "METRICS_ADDR"
	metricsHost = "127.0.0.1"
	maxPort     = 65535
)

// errNoAddress says that an env file gives no usable metrics address: it is
// missing, or it does not hold a valid host and port.
var errNoAddress = errors.New("no metrics address")

func tokenFile(id string) string { return id + tokenExt }

func envFile(id string) string { return id + envExt }

// hidden reports whether a name in the directory belongs to the manager's own
// bookkeeping rather than to a connector.
func hidden(name string) bool { return strings.HasPrefix(name, ".") }

func metricsAddr(port int) string { return net.JoinHostPort(metricsHost, strconv.Itoa(port)) }

func envContent(port int) []byte { return []byte(metricsKey + "=" + metricsAddr(port) + "\n") }

// readMetricsAddr reads the metrics address of an env file. An env file that
// is missing or has no valid address gives errNoAddress; any other error is a
// failure to read it.
func readMetricsAddr(path string) (addr string, port int, err error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", 0, errNoAddress
	case err != nil:
		return "", 0, err
	}
	// systemd lets a later assignment win.
	var value string
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), metricsKey+"="); ok {
			value = unquote(strings.TrimSpace(v))
		}
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil || !validHost(host) {
		return "", 0, errNoAddress
	}
	port, err = strconv.Atoi(portText)
	if err != nil || port < 1 || port > maxPort {
		return "", 0, errNoAddress
	}
	return value, port, nil
}

// unquote removes one pair of matching quotes, as systemd does when it reads
// an environment file.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// validHost accepts an IP address or a host name made of the usual characters.
func validHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return host != "" && strings.Trim(host, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-.") == ""
}

// hasContent reports whether the file at path exists and holds data. A file
// that cannot be read does not.
func hasContent(path string, data []byte) bool {
	old, err := os.ReadFile(path)
	return err == nil && bytes.Equal(old, data)
}

// fixMode sets the permission bits of path when they are not mode.
func fixMode(path string, mode fs.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() == mode {
		return nil
	}
	return os.Chmod(path, mode)
}

// writeAtomic writes through a temporary file in the same directory, so that
// a reader sees the old or the new content, never a part of it.
func writeAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*"+tempExt)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("setting mode of %s: %w", tmp.Name(), err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", tmp.Name(), err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	return os.Rename(tmp.Name(), path)
}

// staleFailure is a stale temporary file that could not be removed, or the
// directory that could not be listed to find them.
type staleFailure struct {
	name string
	err  error
}

// removeStaleTemps removes the temporary files that a write interrupted
// between creating and renaming leaves behind. One of them may hold a token.
func removeStaleTemps(dir string) []staleFailure {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []staleFailure{{dir, fmt.Errorf("listing %s: %w", dir, err)}}
	}
	var failed []staleFailure
	for _, e := range entries {
		name := e.Name()
		if !hidden(name) || !strings.HasSuffix(name, tempExt) {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, staleFailure{path, fmt.Errorf("removing %s: %w", path, err)})
		}
	}
	return failed
}
