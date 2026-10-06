package applianceinstall

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"slices"
	"strings"
)

const (
	// resolvConfPath is the resolver file of the node, resolvedConfPath the
	// one systemd-resolved writes with the servers it forwards to.
	resolvConfPath   = "/etc/resolv.conf"
	resolvedConfPath = "/run/systemd/resolve/resolv.conf"

	// maxNameservers is how many name servers the C library asks, and so
	// how many the container is given.
	maxNameservers = 3

	nameThem = "name the appliance's name servers with --nameserver"
)

// resolvedStubs are the addresses systemd-resolved answers on, on the
// loopback of the node.
var resolvedStubs = []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 53}), netip.AddrFrom4([4]byte{127, 0, 0, 54})}

// checkNameservers refuses the name servers of --nameserver that cannot work
// in the container: what is no address, and an address of the loopback, which
// in the container is the container's own.
func checkNameservers(servers []string) error {
	if len(servers) > maxNameservers {
		return fmt.Errorf("--nameserver: at most %d name servers, as the C library of the container asks no more", maxNameservers)
	}
	for _, s := range servers {
		a, err := netip.ParseAddr(s)
		switch {
		case err != nil || a.IsUnspecified():
			return fmt.Errorf("--nameserver %q: want the IP address of a name server", s)
		case a.Unmap().IsLoopback():
			return fmt.Errorf("--nameserver %s: an address of the loopback is the container's own, where no name server listens: "+
				"name one the container reaches", s)
		}
	}
	return nil
}

// chooseNameservers decides the name servers of the container: those of
// --nameserver, or those the node asks in the end. Without them Proxmox copies
// the node's resolv.conf into the container, where a server on the loopback,
// as the stub of systemd-resolved at 127.0.0.53 is, is the container's own,
// and nothing answers.
func (r *run) chooseNameservers() error {
	if len(r.o.Nameservers) > 0 {
		r.dns = nil
		for _, s := range r.o.Nameservers {
			r.dns = append(r.dns, netip.MustParseAddr(s).Unmap().String())
		}
		r.ask.Info("preflight: the appliance resolves names through %s, as --nameserver says", strings.Join(r.dns, ", "))
		return nil
	}
	servers, err := readNameservers(r.h.readFile, r.h.resolv)
	if err != nil {
		return err
	}
	how := "as the node does"
	if i := slices.IndexFunc(servers, func(a netip.Addr) bool { return slices.Contains(resolvedStubs, a) }); i >= 0 {
		stub := servers[i]
		if servers, err = readNameservers(r.h.readFile, r.h.resolved); err != nil {
			return err
		}
		if len(servers) == 0 {
			return fmt.Errorf("the node resolves names through systemd-resolved at %s, and %s names no server it forwards to: %s",
				stub, resolvedConfPath, nameThem)
		}
		how = "the servers systemd-resolved on the node forwards to"
	}
	if len(servers) == 0 {
		return fmt.Errorf("the node's %s names no name server: %s", resolvConfPath, nameThem)
	}
	var local []string
	r.dns = nil
	for _, a := range servers {
		switch {
		case a.IsLoopback():
			local = append(local, a.String())
			r.ask.Warn("the node's name server %s is on the node itself, which the container does not reach: the appliance goes without it", a)
		case a.Zone() != "":
			r.ask.Warn("the node's name server %s is reached through a link of the node, which the container does not have: "+
				"the appliance goes without it", a)
		case len(r.dns) < maxNameservers:
			r.dns = append(r.dns, a.String())
		}
	}
	if len(r.dns) == 0 {
		what := "an address of the node itself"
		if len(local) > 1 {
			what = "addresses of the node itself"
		}
		return fmt.Errorf("the node resolves names through %s, %s that the container does not reach: %s",
			strings.Join(local, ", "), what, nameThem)
	}
	r.ask.Info("preflight: the appliance resolves names through %s, %s", strings.Join(r.dns, ", "), how)
	return nil
}

// readNameservers reads the name servers of a resolver file, in its order;
// none when there is no file.
func readNameservers(readFile func(string) ([]byte, error), path string) ([]netip.Addr, error) {
	b, err := readFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the name servers of the node: %w", err)
	}
	var out []netip.Addr
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil {
			out = append(out, a.Unmap())
		}
	}
	return out, nil
}
