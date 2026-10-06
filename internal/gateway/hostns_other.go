//go:build !linux

package gateway

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"
)

var errNotLinux = errors.New("the gateway namespace needs Linux")

// NewHost returns the backend of the host profile, which needs Linux.
func NewHost(nsNft, hostNft Nft, log zerolog.Logger) Backend {
	return newHost(unsupported{}, nsNft, hostNft, log, time.Now)
}

// unsupported is the system where there is no network namespace.
type unsupported struct{}

func (unsupported) pinned() (bool, error)                         { return false, errNotLinux }
func (unsupported) mayPin() bool                                  { return false }
func (unsupported) createNamespace() error                        { return errNotLinux }
func (unsupported) openNamespace() error                          { return errNotLinux }
func (unsupported) removeNamespace() error                        { return errNotLinux }
func (unsupported) handle() netns.NsHandle                        { return netns.None() }
func (unsupported) sysctl(side, string) (string, error)           { return "", errNotLinux }
func (unsupported) setSysctl(side, string, string) error          { return errNotLinux }
func (unsupported) link(side, string) (link, bool, error)         { return link{}, false, errNotLinux }
func (unsupported) links(side) ([]link, error)                    { return nil, errNotLinux }
func (unsupported) delLink(side, string) error                    { return errNotLinux }
func (unsupported) setMTU(side, string, int) error                { return errNotLinux }
func (unsupported) setMaster(string, string) error                { return errNotLinux }
func (unsupported) setUp(side, string) error                      { return errNotLinux }
func (unsupported) setIsolated(string, bool) error                { return errNotLinux }
func (unsupported) portVLANs(string) ([]portVLAN, error)          { return nil, errNotLinux }
func (unsupported) addPortVLAN(string, portVLAN) error            { return errNotLinux }
func (unsupported) delPortVLAN(string, int) error                 { return errNotLinux }
func (unsupported) fdb(string) ([]fdbEntry, error)                { return nil, errNotLinux }
func (unsupported) delFDB(string, fdbEntry) error                 { return errNotLinux }
func (unsupported) addrs(side, string) ([]address, error)         { return nil, errNotLinux }
func (unsupported) addAddr(side, string, address) error           { return errNotLinux }
func (unsupported) delAddr(side, string, netip.Prefix) error      { return errNotLinux }
func (unsupported) routes(side, int) ([]route, error)             { return nil, errNotLinux }
func (unsupported) addRoute(side, route) error                    { return errNotLinux }
func (unsupported) delRoute(side, route) error                    { return errNotLinux }
func (unsupported) rules(side) ([]rule, error)                    { return nil, errNotLinux }
func (unsupported) addRule(side, rule) error                      { return errNotLinux }
func (unsupported) delRule(side, rule) error                      { return errNotLinux }
func (unsupported) neighbours(string) ([]neighbour, error)        { return nil, errNotLinux }
func (unsupported) setNeighbour(string, neighbour) error          { return errNotLinux }
func (unsupported) delNeighbour(string, netip.Addr) error         { return errNotLinux }
func (unsupported) flushZone(int) error                           { return errNotLinux }
func (unsupported) addSticky(string, net.HardwareAddr, int) error { return errNotLinux }

func (unsupported) addVeth(string, net.HardwareAddr, string, net.HardwareAddr, int) error {
	return errNotLinux
}

func (unsupported) arpProbe(context.Context, string, netip.Addr, time.Duration) ([]net.HardwareAddr, error) {
	return nil, errNotLinux
}

func (unsupported) dial(context.Context, int, netip.Addr, netip.AddrPort) (net.Conn, error) {
	return nil, errNotLinux
}
