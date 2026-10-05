package appliance

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Incarnation identifies one boot of this container: the container's boot id
// (/proc/sys/kernel/random/boot_id, fresh at every container start) and field
// 22 of /proc/1/stat, PID 1's start in clock ticks since the node booted,
// joined as "<boot id>/<ticks>" and never converted to wall-clock time (ruling
// 25). Lab 80 showed both change on reboot, stop and start, rollback, clone and
// restore, and neither on a daemon restart or a freeze.
func Incarnation() (string, error) { return System{}.Incarnation() }

// Incarnation is the package's Incarnation on s. Either part unreadable is an
// error.
func (s System) Incarnation() (string, error) {
	b, err := os.ReadFile(s.proc("sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", fmt.Errorf("reading the boot id: %w", err)
	}
	boot := strings.TrimSpace(string(b))
	if !bootID(boot) {
		return "", fmt.Errorf("the boot id %q is not one", boot)
	}
	stat, err := os.ReadFile(s.proc("1", "stat"))
	if err != nil {
		return "", fmt.Errorf("reading the start of PID 1: %w", err)
	}
	ticks, err := startTicks(string(stat))
	if err != nil {
		return "", err
	}
	return boot + "/" + ticks, nil
}

// bootID reports whether s is a boot id as the kernel writes it: a UUID in
// lower case.
func bootID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if r != '-' {
				return false
			}
		case (r < '0' || r > '9') && (r < 'a' || r > 'f'):
			return false
		}
	}
	return true
}

// startTicks returns field 22 of a /proc/<pid>/stat, the start of the process.
// The name in field 2 is in parentheses and may hold spaces and parentheses
// itself, so the fields are counted from the last closing one.
func startTicks(stat string) (string, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return "", errors.New("the stat of PID 1 has no name in parentheses")
	}
	rest := strings.Fields(stat[i+1:]) // from field 3 on
	const field = 22 - 3
	if len(rest) <= field {
		return "", errors.New("the stat of PID 1 has fewer than 22 fields")
	}
	ticks := rest[field]
	if strings.Trim(ticks, "0123456789") != "" {
		return "", fmt.Errorf("the start of PID 1 %q is not a number", ticks)
	}
	return ticks, nil
}

// EpochAtStart decides the writer epoch of this start: kept when the stored
// incarnation equals the current one and the install matches; otherwise a
// new nonce and generation+1, saved durably before it is returned. Only ever
// called after a passed self-identification of this process (ruling 23).
//
// A leader.json that is missing, not valid or of another install is no epoch
// to bump: that is for pco appliance recover, and an error here.
func EpochAtStart(st *store.Store, installID, incarnation string, rand io.Reader) (w planner.Writer, kept bool, err error) {
	if incarnation == "" {
		return planner.Writer{}, false, errors.New("the incarnation of this container is not known")
	}
	stored, found, err := st.Writer()
	switch {
	case err != nil:
		return planner.Writer{}, false, fmt.Errorf("reading leader.json: %w", err)
	case !found:
		return planner.Writer{}, false, errors.New("leader.json is missing; run pco appliance recover")
	case stored.Validate() != nil:
		return planner.Writer{}, false, fmt.Errorf("the writer identity in leader.json is not valid: %w; run pco appliance recover", stored.Validate())
	case stored.InstallID != installID:
		return planner.Writer{}, false, fmt.Errorf("leader.json names install %s, but this is install %s; run pco appliance recover", stored.InstallID, installID)
	case stored.Incarnation == incarnation:
		return stored, true, nil
	}
	nonce, err := newNonce(rand)
	if err != nil {
		return planner.Writer{}, false, err
	}
	next := planner.Writer{InstallID: installID, Generation: stored.Generation + 1, Nonce: nonce, Incarnation: incarnation}
	if err := st.SaveWriter(next); err != nil {
		return planner.Writer{}, false, fmt.Errorf("saving the new epoch: %w", err)
	}
	return next, false, nil
}

const nonceAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// newNonce returns 8 random lower-case letters and digits, as setup draws
// them: a byte at or above the largest multiple of the alphabet's size is
// drawn again, so that every character is as likely.
func newNonce(rand io.Reader) (string, error) {
	const size, limit = len(nonceAlphabet), 256 - 256%len(nonceAlphabet)
	nonce := make([]byte, 0, 8)
	b := make([]byte, 1)
	for len(nonce) < cap(nonce) {
		if _, err := io.ReadFull(rand, b); err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		if int(b[0]) < limit {
			nonce = append(nonce, nonceAlphabet[int(b[0])%size])
		}
	}
	return string(nonce), nil
}
