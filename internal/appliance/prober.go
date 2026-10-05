package appliance

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// Gateways are the gateways of the default routes of the container, from
// /proc/net/route, sorted and each once: they join the soft deny.
func (s System) Gateways() ([]netip.Addr, error) {
	f, err := os.Open(s.proc("net", "route"))
	if err != nil {
		return nil, fmt.Errorf("reading the routes: %w", err)
	}
	defer func() { _ = f.Close() }()
	const rtfGateway = 0x2
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
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out), nil
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
