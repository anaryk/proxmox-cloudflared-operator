package applianceinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// The web interface of the appliance listens on net0's address only: the
// other cards of the container are legs into the networks of guests, who
// would reach its sign-in page through them. The installer knows net0's
// address, from --ip or from the lease DHCP gave eth0, and writes it where
// pco web and the daemon read it, before the daemon's first start makes the
// key and the certificate for it. Once that certificate is there, its
// fingerprint is printed, so that the first visit can be checked against it.

// webTries is how often the installer looks for net0's lease and for the
// certificate, a second apart.
const webTries = 30

// net0Addr is the IPv4 address of eth0, the card of net0, as the container
// has it.
func (r *run) net0Addr(ctx context.Context, vmid int) (netip.Addr, error) {
	for try := 1; ; try++ {
		out, err := r.exec(ctx, vmid, "ip", "-j", "-4", "addr", "show", "dev", "eth0")
		if err != nil {
			return netip.Addr{}, fmt.Errorf("reading the address of eth0 in lxc/%d: %w", vmid, err)
		}
		if a, ok := globalIPv4(out); ok {
			return a, nil
		}
		if try == webTries {
			return netip.Addr{}, fmt.Errorf("eth0 of lxc/%d has no IPv4 address after %d s: give it one with --ip, "+
				"or check the DHCP server on bridge %s", vmid, webTries, r.o.Bridge)
		}
		if err := r.h.sleep(ctx, time.Second); err != nil {
			return netip.Addr{}, err
		}
	}
}

// globalIPv4 is the first global IPv4 address of what ip -j addr prints.
func globalIPv4(out string) (netip.Addr, bool) {
	var links []struct {
		AddrInfo []struct {
			Family string `json:"family"`
			Local  string `json:"local"`
			Scope  string `json:"scope"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return netip.Addr{}, false
	}
	for _, l := range links {
		for _, a := range l.AddrInfo {
			if addr, err := netip.ParseAddr(a.Local); err == nil && a.Family == "inet" && a.Scope == "global" {
				return addr, true
			}
		}
	}
	return netip.Addr{}, false
}

// writeListen writes net0's address and the environment of pco-web into the
// container.
func (r *run) writeListen(ctx context.Context) error {
	vmid := r.j.VMID
	addr, err := r.net0Addr(ctx, vmid)
	if err != nil {
		return err
	}
	if err := r.makeTemp(); err != nil {
		return err
	}
	defer r.removeTemp()
	listen := net.JoinHostPort(addr.String(), webcert.Port)
	for _, f := range []struct{ name, dst, content string }{
		{"net0", webcert.Net0File, addr.String() + "\n"},
		{"pco-web", webcert.EnvFile, "# Written by pco appliance install. pco-web listens on net0's address only:\n" +
			"# the other cards of the appliance are legs into the networks of guests.\n" +
			"PCO_WEB_LISTEN=" + listen + "\n"},
	} {
		path := filepath.Join(r.tmp, f.name)
		if err := writeNew(path, []byte(f.content)); err != nil {
			return err
		}
		_, err := r.r.Run(ctx, "pct", "push", strconv.Itoa(vmid), path, f.dst, "--perms", "0644")
		if rerr := os.Remove(path); rerr != nil {
			r.ask.Warn("removing %s: %v", path, rerr)
		}
		if err != nil {
			return fmt.Errorf("pushing %s into lxc/%d: %w", f.dst, vmid, err)
		}
	}
	r.web.url = "https://" + listen + "/"
	r.ask.Info("web interface: listens on %s, net0's address", listen)
	return nil
}

// startWeb waits for the certificate the daemon makes as it starts, enables
// and starts pco-web, and says the fingerprint to check the first visit with.
func (r *run) startWeb(ctx context.Context) error {
	vmid := r.j.VMID
	var fp string
	for try := 1; ; try++ {
		out, err := r.exec(ctx, vmid, "pco", "web", "cert")
		if err == nil {
			if fp = fingerprintOf(out); fp != "" {
				break
			}
		}
		if try == webTries {
			if err == nil {
				err = errors.New("it printed no fingerprint")
			}
			return fmt.Errorf("the daemon of lxc/%d made no certificate of the web interface within %d s: %w", vmid, webTries, err)
		}
		if err := r.h.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	if _, err := r.exec(ctx, vmid, "systemctl", "enable", "--now", "pco-web.service"); err != nil {
		return fmt.Errorf("starting the web interface of lxc/%d: %w", vmid, err)
	}
	r.web.fingerprint = fp
	r.ask.Info("web interface: started, certificate SHA-256 %s", fp)
	return nil
}

// fingerprintOf reads the fingerprint pco web cert prints.
func fingerprintOf(out string) string {
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "SHA-256" {
			return f[1]
		}
	}
	return ""
}
