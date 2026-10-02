package egress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	blockedFile = "egress-blocked.json"
	offFile     = "egress-off.json"

	overrideMode fs.FileMode = 0o600
	overrideDir  fs.FileMode = 0o700
	// maxOverride is the most of an override file that is read.
	maxOverride = 1 << 20
)

// Overrides are what the admin of the node decided about the filter: the
// addresses it never lets a connector reach, and the switch that takes it
// away. They live in two files of the local state root, which only root
// reads and writes, and no command of the API changes them.
type Overrides struct{ dir string }

// NewOverrides returns the overrides kept in the local state root dir.
func NewOverrides(dir string) *Overrides { return &Overrides{dir: dir} }

type blockedDoc struct {
	Addresses []netip.Addr `json:"addresses"`
}

type offDoc struct {
	Since time.Time `json:"since"`
}

func (o *Overrides) path(name string) string { return filepath.Join(o.dir, name) }

// Blocked returns the blocked addresses, sorted. A list that cannot be read
// is an error: it may hold an address that must not be reached.
func (o *Overrides) Blocked() ([]netip.Addr, error) {
	b, err := readLimited(o.path(blockedFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc blockedDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("reading %s: %w", o.path(blockedFile), err)
	}
	return normalizeAddrs(doc.Addresses), nil
}

// Block adds an address to the block list and reports whether it was not
// there yet.
func (o *Overrides) Block(addr netip.Addr) (added bool, err error) {
	return o.changeBlocked(addr, func(list []netip.Addr, a netip.Addr) ([]netip.Addr, bool) {
		if slices.Contains(list, a) {
			return list, false
		}
		return append(list, a), true
	})
}

// Unblock takes an address off the block list and reports whether it was
// there.
func (o *Overrides) Unblock(addr netip.Addr) (removed bool, err error) {
	return o.changeBlocked(addr, func(list []netip.Addr, a netip.Addr) ([]netip.Addr, bool) {
		if !slices.Contains(list, a) {
			return list, false
		}
		return slices.DeleteFunc(list, func(x netip.Addr) bool { return x == a }), true
	})
}

func (o *Overrides) changeBlocked(addr netip.Addr, change func([]netip.Addr, netip.Addr) ([]netip.Addr, bool)) (bool, error) {
	if !addr.IsValid() {
		return false, errors.New("no address to block or unblock")
	}
	list, err := o.Blocked()
	if err != nil {
		return false, err
	}
	list, changed := change(list, normalizeAddr(addr))
	if !changed {
		return false, nil
	}
	return true, o.write(blockedFile, blockedDoc{Addresses: normalizeAddrs(list)})
}

// Off reports whether the filter is switched off, and since when. It is off
// while the file of the switch is there, whatever the file holds; a time that
// cannot be read is the zero time.
func (o *Overrides) Off() (since time.Time, off bool, err error) {
	if off, err = o.switchedOff(); err != nil || !off {
		return time.Time{}, off, err
	}
	b, err := readLimited(o.path(offFile))
	if err != nil {
		return time.Time{}, true, nil
	}
	var doc offDoc
	if json.Unmarshal(b, &doc) != nil {
		return time.Time{}, true, nil
	}
	return doc.Since, true, nil
}

func (o *Overrides) switchedOff() (bool, error) {
	_, err := os.Lstat(o.path(offFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("checking the egress switch: %w", err)
}

// SwitchOff records that the filter is off since at.
func (o *Overrides) SwitchOff(at time.Time) error {
	return o.write(offFile, offDoc{Since: at.UTC()})
}

// SwitchOn removes the record of the switch and reports whether there was one.
func (o *Overrides) SwitchOn() (wasOff bool, err error) {
	err = os.Remove(o.path(offFile))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("switching the egress filter on: %w", err)
}

// write replaces a file through a temporary one in the same directory, so
// that a reader sees the old content or the new one.
func (o *Overrides) write(name string, v any) (err error) {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", name, err)
	}
	if err := os.MkdirAll(o.dir, overrideDir); err != nil {
		return fmt.Errorf("creating %s: %w", o.dir, err)
	}
	tmp, err := os.CreateTemp(o.dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", o.path(name), err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(overrideMode); err == nil {
		if _, err = tmp.Write(data); err == nil {
			err = tmp.Sync()
		}
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err = os.Rename(tmp.Name(), o.path(name)); err != nil {
		return fmt.Errorf("writing %s: %w", o.path(name), err)
	}
	return nil
}

// readLimited reads a file of at most maxOverride bytes.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxOverride+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(b) > maxOverride {
		return nil, fmt.Errorf("reading %s: more than %d bytes", path, maxOverride)
	}
	return b, nil
}
