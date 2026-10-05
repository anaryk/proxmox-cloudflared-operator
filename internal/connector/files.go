package connector

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
)

const (
	tokenExt   = ".token"
	envExt     = ".env"
	configExt  = ".yml"
	pendingExt = ".pending"
	tempExt    = atomicfile.TempExt

	dirMode     fs.FileMode = 0o700
	tokenMode   fs.FileMode = 0o600
	envMode     fs.FileMode = 0o644
	configMode  fs.FileMode = 0o600
	pendingMode fs.FileMode = 0o600

	metricsKey  = "METRICS_ADDR"
	metricsHost = "127.0.0.1"
	maxPort     = 65535

	// edgeKey names the IP version cloudflared reaches the edge with. It is
	// set explicitly, so that what the egress filter has to allow does not
	// change with the defaults of cloudflared.
	edgeKey       = "EDGE_IP_VERSION"
	edgeIPVersion = "auto"

	// installKey names the install a connector is of, so that a daemon of
	// another install, as after the store was lost and set up anew, never
	// takes the connector for one of its own.
	installKey = "PCO_INSTALL"

	// configContent is the configuration of every connector. A file of pco's
	// own with content in it keeps cloudflared from looking for a
	// configuration of the host, which could send it elsewhere.
	configContent = "# Written by pco. A configuration of its own keeps cloudflared from reading one of the host.\n" +
		"no-autoupdate: true\n"
)

// errNoAddress says that an env file gives no usable metrics address: it is
// missing, or it does not hold a valid host and port.
var errNoAddress = errors.New("no metrics address")

func tokenFile(id string) string { return id + tokenExt }

func envFile(id string) string { return id + envExt }

func configFile(id string) string { return id + configExt }

// hidden reports whether a name in the directory belongs to the manager's own
// bookkeeping rather than to a connector.
func hidden(name string) bool { return strings.HasPrefix(name, ".") }

func metricsAddr(port int) string { return net.JoinHostPort(metricsHost, strconv.Itoa(port)) }

func envContent(port int, installID string) []byte {
	return []byte(metricsKey + "=" + metricsAddr(port) + "\n" + edgeKey + "=" + edgeIPVersion + "\n" + installKey + "=" + installID + "\n")
}

// checkInstall refuses what cannot be an install id: it is written into the
// env file as it is.
func checkInstall(id string) error {
	if !installIDPattern.MatchString(id) {
		return fmt.Errorf("invalid install id %q", id)
	}
	return nil
}

// readEnv returns the values an env file gives the keys the manager writes,
// as systemd reads them. A missing file gives none.
func readEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	values := make(map[string]string)
	for line := range strings.Lines(string(b)) {
		for _, key := range []string{metricsKey, edgeKey, installKey} {
			// systemd lets a later assignment win.
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
				values[key] = unquote(strings.TrimSpace(v))
			}
		}
	}
	return values, nil
}

// readMetricsPort reads the port of the metrics address of an env file. An
// env file that is missing or has no valid address gives errNoAddress; any
// other error is a failure to read it.
func readMetricsPort(path string) (int, error) {
	values, err := readEnv(path)
	if err != nil {
		return 0, err
	}
	_, port, err := metricsOf(values)
	return port, err
}

// metricsOf returns the metrics address among the values of an env file, or
// errNoAddress when they hold no valid one.
func metricsOf(values map[string]string) (addr string, port int, err error) {
	value := values[metricsKey]
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

// staleFailure is a stale temporary file that could not be removed.
type staleFailure struct {
	name string
	err  error
}

// removeStaleTemps removes the temporary files that a write interrupted
// between creating and renaming leaves behind. One of them may hold a token.
// It returns those it could not remove, and an error when the directory could
// not be listed to find them.
func (m *Manager) removeStaleTemps() ([]staleFailure, error) {
	entries, err := m.readDir(m.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s for stale temporary files: %w", m.dir, err)
	}
	var failed []staleFailure
	for _, e := range entries {
		name := e.Name()
		if !hidden(name) || !strings.HasSuffix(name, tempExt) {
			continue
		}
		path := m.path(name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, staleFailure{path, fmt.Errorf("removing %s: %w", path, err)})
		}
	}
	return failed, nil
}
