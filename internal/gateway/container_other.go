//go:build !linux

package gateway

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// NewContainer returns the backend of the appliance, which needs Linux.
func NewContainer(nft Nft, api NICWriter, self model.GuestRef, node string, expect func(mac net.HardwareAddr), log zerolog.Logger,
	opts ...ContainerOption,
) Backend {
	return newContainer(noContainer{}, nft, api, self, node, expect, log, time.Now, opts...)
}

// noContainer is the network of an appliance where there is none.
type noContainer struct{}

func (noContainer) handle() netns.NsHandle                 { return netns.None() }
func (noContainer) sysctl(string) (string, error)          { return "", errNotLinux }
func (noContainer) setSysctl(string, string) error         { return errNotLinux }
func (noContainer) link(string) (link, bool, error)        { return link{}, false, errNotLinux }
func (noContainer) links() ([]link, error)                 { return nil, errNotLinux }
func (noContainer) addDummy(string) error                  { return errNotLinux }
func (noContainer) rename(string, string) error            { return errNotLinux }
func (noContainer) setUp(string) error                     { return errNotLinux }
func (noContainer) setDown(string) error                   { return errNotLinux }
func (noContainer) addrs(string) ([]address, error)        { return nil, errNotLinux }
func (noContainer) addAddr(string, address) error          { return errNotLinux }
func (noContainer) delAddr(string, netip.Prefix) error     { return errNotLinux }
func (noContainer) routes(int) ([]route, error)            { return nil, errNotLinux }
func (noContainer) addRoute(route) error                   { return errNotLinux }
func (noContainer) delRoute(route) error                   { return errNotLinux }
func (noContainer) rules() ([]rule, error)                 { return nil, errNotLinux }
func (noContainer) addRule(rule) error                     { return errNotLinux }
func (noContainer) delRule(rule) error                     { return errNotLinux }
func (noContainer) neighbours(string) ([]neighbour, error) { return nil, errNotLinux }
func (noContainer) setNeighbour(string, neighbour) error   { return errNotLinux }
func (noContainer) delNeighbour(string, netip.Addr) error  { return errNotLinux }

func (noContainer) arpProbe(context.Context, string, netip.Addr, time.Duration) ([]net.HardwareAddr, error) {
	return nil, errNotLinux
}

func (noContainer) announce(string, netip.Addr) error { return errNotLinux }

func (noContainer) dial(context.Context, int, netip.Addr, netip.AddrPort) (net.Conn, error) {
	return nil, errNotLinux
}
