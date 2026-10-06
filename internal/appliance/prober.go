package appliance

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// rtfGateway is the flag of a route through a gateway.
const rtfGateway = 0x2

// Gateways are the gateways of the default routes of the container, from
// /proc/net/route and /proc/net/ipv6_route, sorted and each once: they join
// the soft deny. A kernel without IPv6 has no IPv6 routes to read.
func (s System) Gateways() ([]netip.Addr, error) {
	v4, err := s.gateways4()
	if err != nil {
		return nil, err
	}
	v6, err := s.gateways6()
	if err != nil {
		return nil, err
	}
	out := slices.Concat(v4, v6)
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out), nil
}

func (s System) gateways4() ([]netip.Addr, error) {
	f, err := os.Open(s.proc("net", "route"))
	if err != nil {
		return nil, fmt.Errorf("reading the routes: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []netip.Addr
	sc := bufio.NewScanner(f)
	for first := true; sc.Scan(); first = false {
		fields := strings.Fields(sc.Text())
		if first || len(fields) < 8 || fields[1] != "00000000" || fields[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 16)
		if err != nil || flags&rtfGateway == 0 {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		// The kernel writes the address as a number in the byte order of the
		// host, which is little-endian wherever pco runs.
		var a [4]byte
		binary.BigEndian.PutUint32(a[:], binary.LittleEndian.Uint32(b))
		out = append(out, netip.AddrFrom4(a))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the routes: %w", err)
	}
	return out, nil
}

// gateways6 reads the next hops of the IPv6 default routes. A line of
// /proc/net/ipv6_route is the destination and its prefix length, the source
// and its prefix length, the next hop, the metric, the references, the use,
// the flags and the device, the addresses in 32 hex digits as written.
func (s System) gateways6() ([]netip.Addr, error) {
	f, err := os.Open(s.proc("net", "ipv6_route"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("reading the IPv6 routes: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []netip.Addr
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[1] != "00" || strings.Trim(fields[0], "0") != "" {
			continue
		}
		flags, err := strconv.ParseUint(fields[8], 16, 32)
		if err != nil || flags&rtfGateway == 0 {
			continue
		}
		b, err := hex.DecodeString(fields[4])
		if err != nil || len(b) != 16 {
			continue
		}
		if a := netip.AddrFrom16([16]byte(b)); !a.IsUnspecified() {
			out = append(out, a)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the IPv6 routes: %w", err)
	}
	return out, nil
}

// prober is the host prober of the container, whose interfaces are on the
// segments the daemon maps them to: inside the container no bridge says so.
type prober struct {
	resolve.Prober
	segments func() map[string]resolve.Segment
}

// NewProber wraps the host prober: Interfaces gets its Segment from
// segments(), everything else passes through.
func NewProber(host resolve.Prober, segments func() map[string]resolve.Segment) resolve.Prober {
	return prober{Prober: host, segments: segments}
}

// Interfaces lists the interfaces of the container, each on the segment the
// map gives its name, or on none.
func (p prober) Interfaces(ctx context.Context) ([]resolve.HostIface, error) {
	ifaces, err := p.Prober.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	segments := p.segments()
	out := slices.Clone(ifaces)
	for i := range out {
		out[i].Segment = segments[out[i].Name]
	}
	return out, nil
}
