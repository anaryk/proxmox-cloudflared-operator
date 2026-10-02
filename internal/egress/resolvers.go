package egress

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"strings"
)

const (
	resolvConf = "/etc/resolv.conf"
	// maxResolvConf is how much of resolv.conf is read.
	maxResolvConf = 64 << 10
)

// SystemResolvers returns the name servers that /etc/resolv.conf names, the
// ones cloudflared resolves with: the function a daemon passes to New.
func SystemResolvers() ([]netip.Addr, error) { return readResolvers(resolvConf) }

// readResolvers returns every valid name server the file names, in its order,
// and none when there is no file: not the default of the C library, which
// asks the host itself, and which the filter then does not open.
func readResolvers(path string) ([]netip.Addr, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxResolvConf+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(b) > maxResolvConf {
		// A line cut by the limit could name another address than the file.
		b = b[:bytes.LastIndexByte(b[:maxResolvConf], '\n')+1]
	}
	return parseResolvers(string(b)), nil
}

func parseResolvers(text string) []netip.Addr {
	var out []netip.Addr
	for line := range strings.Lines(text) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(fields[1]); err == nil {
			out = append(out, normalizeAddr(a))
		}
	}
	return out
}
