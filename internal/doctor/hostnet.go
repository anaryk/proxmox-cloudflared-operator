package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// netnsPin is where the gateway namespace is pinned.
var netnsPin = filepath.Join("/run/netns", gateway.Namespace)

var _ NetworkEnv = (*HostEnv)(nil)

// errNotAtHand is what the host cannot tell where the daemon did not give it
// the means.
var errNotAtHand = errors.New("not known to this process")

// sdnReader is the part of the Proxmox client that lists the SDN with what
// waits to be applied.
type sdnReader interface {
	SDNPending(ctx context.Context) (pve.SDN, error)
}

// The daemon hands the doctor its *pve.Client, on a host and in the
// appliance alike.
var _ sdnReader = (*pve.Client)(nil)

// NetnsPinned checks that the gateway namespace is pinned, within the
// timeout.
func (h *HostEnv) NetnsPinned(ctx context.Context) error {
	check := h.Pinned
	if check == nil {
		check = func() error { return netnsPinned(netnsPin) }
	}
	return h.within(ctx, check)
}

// UnitProperty asks systemd for a property of a unit as it is loaded.
func (h *HostEnv) UnitProperty(ctx context.Context, unit, property string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	if h.Show != nil {
		return h.Show(ctx, unit, property)
	}
	var stdout, stderr capped
	stdout.max, stderr.max = maxVersionOutput, maxVersionOutput
	cmd := exec.CommandContext(ctx, systemctlPath, "show", "--property="+property, "--value", "--", unit)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("%w: %s", err, detail)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

// SDNPending asks Proxmox for the SDN with the changes that wait to be
// applied.
func (h *HostEnv) SDNPending(ctx context.Context) (pve.SDN, error) {
	r, ok := h.Proxmox.(sdnReader)
	if !ok {
		return pve.SDN{}, errNotAtHand
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	return r.SDNPending(ctx)
}

// PconetIPv6 reads whether IPv6 is on on the bridge of the managed network.
func (h *HostEnv) PconetIPv6(context.Context) (bool, error) {
	proc := h.Proc
	if proc == "" {
		proc = "/proc"
	}
	return ipv6On(filepath.Join(proc, "sys/net/ipv6/conf", gateway.ManagedBridge, "disable_ipv6"))
}

// ipv6On reads a disable_ipv6 sysctl: IPv6 is on unless it says 1. A bridge
// that does not exist has none.
func ipv6On(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	return strings.TrimSpace(string(raw)) != "1", nil
}

// HostTable checks that the table inet pco_host is loaded.
func (h *HostEnv) HostTable(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	nft := h.HostNft
	if nft == nil {
		nft = gateway.NewHostNft()
	}
	_, err := nft.List(ctx, hostTable)
	return err
}

// ServicePool reads the pool of service addresses, within the timeout.
func (h *HostEnv) ServicePool(ctx context.Context) (used, size int, err error) {
	if h.Pool == nil {
		return 0, 0, errNotAtHand
	}
	pool, err := withinValue(ctx, h, func() ([2]int, error) {
		used, size, err := h.Pool()
		return [2]int{used, size}, err
	})
	return pool[0], pool[1], err
}

// pinsAt are the pins and when they were given.
type pinsAt struct {
	pins []store.Pin
	at   time.Time
}

// Pins reads the pins the gateway was last given, within the timeout.
func (h *HostEnv) Pins(ctx context.Context) ([]store.Pin, time.Time, error) {
	if h.PinsOf == nil {
		return nil, time.Time{}, errNotAtHand
	}
	p, err := withinValue(ctx, h, func() (pinsAt, error) {
		pins, at, err := h.PinsOf()
		return pinsAt{pins, at}, err
	})
	return p.pins, p.at, err
}

// withinValue is within for a read that returns a value: one given up on
// is left to finish on its own, and what it returns then is dropped.
func withinValue[T any](ctx context.Context, h *HostEnv, read func() (T, error)) (T, error) {
	type result struct {
		val T
		err error
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		v, err := read()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.val, r.err
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("no answer within %s: %w", h.timeout(), ctx.Err())
	}
}
