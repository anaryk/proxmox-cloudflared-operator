package webcert

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
)

// Where the node keeps what pco-web.service loads. The unit reads the three
// files of Dir as its credentials tls.crt, tls.key and pveproxy.crt.
const (
	Dir      = "/etc/pco/web"
	CertName = "tls.crt"
	KeyName  = "tls.key"
	// PinName links to the certificate pveproxy serves, which pco-web pins
	// its connections to the Proxmox API to.
	PinName = "pveproxy.crt"
	EnvFile = "/etc/default/pco-web"
	Port    = "8643"

	// The cluster CA of Proxmox VE. Its key is read by root alone: by setup
	// and by the daemon, never by pco-web.
	ClusterCA    = "/etc/pve/pve-root-ca.pem"
	ClusterCAKey = "/etc/pve/priv/pve-root-ca.key"
)

// KeyOf returns the key file of a certificate pveproxy serves:
// pveproxy-ssl.key for pveproxy-ssl.pem, pve-ssl.key for pve-ssl.pem.
func KeyOf(certFile string) string { return strings.TrimSuffix(certFile, ".pem") + ".key" }

// WriteAtomic writes the pair into dir as tls.crt and tls.key (0644, 0600)
// through temporary files and renames.
func WriteAtomic(dir string, certPEM, keyPEM []byte) error {
	return writeAtomic(dir, certPEM, keyPEM, os.Rename)
}

// writeAtomic puts the key in place first and the certificate next. When the
// second rename fails, the key that was there is put back, so that the pair
// that was there stays a pair.
func writeAtomic(dir string, certPEM, keyPEM []byte, rename func(from, to string) error) error {
	crt, key := filepath.Join(dir, CertName), filepath.Join(dir, KeyName)
	newKey, err := writeTemp(dir, KeyName, keyPEM, 0o600)
	if err != nil {
		return err
	}
	defer removeTemp(newKey)
	newCrt, err := writeTemp(dir, CertName, certPEM, 0o644)
	if err != nil {
		return err
	}
	defer removeTemp(newCrt)
	putBack, err := keepOld(key)
	if err != nil {
		return err
	}
	if err := rename(newKey, key); err != nil {
		return fmt.Errorf("putting %s in place: %w", key, err)
	}
	if err := rename(newCrt, crt); err != nil {
		err = fmt.Errorf("putting %s in place: %w", crt, err)
		if berr := putBack(); berr != nil {
			return errors.Join(err, fmt.Errorf("putting the key that was there back: %w", berr))
		}
		return err
	}
	return nil
}

// keepOld returns what puts back the file, link or nothing that is at path
// now.
func keepOld(path string) (func() error, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return func() error { return removeIfThere(path) }, nil
	case err != nil:
		return nil, err
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		return func() error { return LinkAtomic(path, target) }, nil
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return func() error { return atomicfile.Write(path, old, atomicfile.Options{Mode: info.Mode().Perm()}) }, nil
}

// writeTemp writes data to a new file next to the one named name in dir.
func writeTemp(dir, name string, data []byte, mode fs.FileMode) (path string, err error) {
	f, err := os.CreateTemp(dir, "."+name+".*"+atomicfile.TempExt)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return "", err
	}
	if _, err = f.Write(data); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// removeTemp removes a temporary file that was not renamed into place.
func removeTemp(path string) { _ = removeIfThere(path) }

func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// LinkAtomic makes path a symbolic link to target, replacing what is there
// in one rename.
func LinkAtomic(path, target string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*"+atomicfile.TempExt)
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Close()
	if err := os.Remove(tmp); err != nil {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadPair reads tls.crt and tls.key of dir.
func ReadPair(dir string) (certPEM, keyPEM []byte, err error) {
	if certPEM, err = os.ReadFile(filepath.Join(dir, CertName)); err != nil {
		return nil, nil, err
	}
	if keyPEM, err = os.ReadFile(filepath.Join(dir, KeyName)); err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// Env is what /etc/default/pco-web says of the names of the web interface.
type Env struct {
	Listen string   // PCO_WEB_LISTEN
	Hosts  []string // PCO_WEB_HOSTS, split at the commas
}

// ReadEnv reads the environment file of the unit. found is false when there
// is none.
func ReadEnv(path string) (env Env, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Env{}, false, nil
	}
	if err != nil {
		return Env{}, false, err
	}
	vars := ParseEnv(b)
	env.Listen = vars["PCO_WEB_LISTEN"]
	for h := range strings.SplitSeq(vars["PCO_WEB_HOSTS"], ",") {
		if h = strings.TrimSpace(h); h != "" {
			env.Hosts = append(env.Hosts, h)
		}
	}
	return env, true, nil
}

// ParseEnv reads the assignments of an environment file as systemd does for
// the simple cases: KEY=value lines, comments, and a value in quotes.
func ParseEnv(b []byte) map[string]string {
	vars := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		vars[strings.TrimSpace(key)] = value
	}
	return vars
}
