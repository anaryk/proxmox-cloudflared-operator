package connector

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	tokenExt = ".token"
	envExt   = ".env"

	dirMode   fs.FileMode = 0o700
	tokenMode fs.FileMode = 0o600
	envMode   fs.FileMode = 0o644

	metricsKey  = "METRICS_ADDR"
	metricsHost = "127.0.0.1"
)

// errNoAddress says that an env file gives no usable metrics address: it is
// missing, or it does not hold a valid host and port.
var errNoAddress = errors.New("no metrics address")

func tokenFile(id string) string { return id + tokenExt }

func envFile(id string) string { return id + envExt }

func envContent(port int) []byte {
	return []byte(metricsKey + "=" + net.JoinHostPort(metricsHost, strconv.Itoa(port)) + "\n")
}

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
			value = v
		}
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return "", 0, errNoAddress
	}
	port, err = strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errNoAddress
	}
	return value, port, nil
}

// replaceIfChanged makes the file at path hold data, and reports whether it
// had to write it. A file that cannot be read counts as different.
func replaceIfChanged(path string, data []byte, mode fs.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	if err := writeAtomic(path, data, mode); err != nil {
		return false, err
	}
	return true, nil
}

// writeAtomic writes through a temporary file in the same directory, so that
// a reader sees the old or the new content, never a part of it.
func writeAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
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
