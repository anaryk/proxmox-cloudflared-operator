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
// key and the certificate for it; the daemon keeps it current from then on.
// Once that certificate is there, its fingerprint is printed, so that the
// first visit can be checked against it. It serves IPv4 only.

// webTries is how often the installer looks for net0's lease and for the
// certificate, a second apart.
const webTries = 30

// webEnv is the environment file of pco-web the installer writes: nothing
// set, so that pco-web listens on net0's address as the daemon keeps it.
const webEnv = "# Written by pco appliance install. pco-web listens on net0's address, port 8643, which\n" +
	"# the daemon keeps in " + webcert.Net0File + ": the other cards of the appliance are legs into the\n" +
	"# networks of guests. PCO_WEB_LISTEN may name another port of that address, no other address.\n"

// net0Addr is the IPv4 address of eth0, the card of net0, as the container
// has it.
func (r *run) net0Addr(ctx context.Context, vmid int) (netip.Addr, error) {
	for try := 1; ; try++ {
		out, err := r.exec(ctx, vmid, "ip", "-j", "addr", "show", "dev", "eth0")
		if err != nil {
			return netip.Addr{}, fmt.Errorf("reading the address of eth0 in lxc/%d: %w", vmid, err)
		}
		v4, v6 := globalAddrs(out)
		if v4.IsValid() {
			return v4, nil
		}
		if try == webTries {
			if v6 {
				return netip.Addr{}, fmt.Errorf("eth0 of lxc/%d has an IPv6 address only, and the web interface of the appliance "+
					"serves IPv4: give net0 an IPv4 address with --ip", vmid)
			}
			return netip.Addr{}, fmt.Errorf("eth0 of lxc/%d has no IPv4 address after %d s: give it one with --ip, "+
				"or check the DHCP server on bridge %s", vmid, webTries, r.o.Bridge)
		}
		if err := r.h.sleep(ctx, time.Second); err != nil {
			return netip.Addr{}, err
		}
	}
}

// globalAddrs reads what ip -j addr prints: the first global IPv4 address,
// and whether there is a global IPv6 address.
func globalAddrs(out string) (v4 netip.Addr, v6 bool) {
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
			addr, err := netip.ParseAddr(a.Local)
			switch {
			case err != nil || a.Scope != "global":
			case a.Family == "inet" && !v4.IsValid():
				v4 = addr
			case a.Family == "inet6":
				v6 = true
			}
		}
	}
	return v4, v6
}

// writeListen writes net0's address and the environment of pco-web into the
// container.
func (r *run) writeListen(ctx context.Context) error {
	return r.writeWebFiles(ctx, true)
}

// writeWebFiles writes net0's address into the container, and the
// environment of pco-web when env says so.
func (r *run) writeWebFiles(ctx context.Context, env bool) error {
	vmid := r.j.VMID
	addr, err := r.net0Addr(ctx, vmid)
	if err != nil {
		return err
	}
	if err := r.makeTemp(); err != nil {
		return err
	}
	defer r.removeTemp()
	files := []struct{ name, dst, content string }{{"net0", webcert.Net0File, addr.String() + "\n"}}
	if env {
		files = append(files, struct{ name, dst, content string }{"pco-web", webcert.EnvFile, webEnv})
	}
	for _, f := range files {
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
	r.web.url = webURL(addr)
	r.ask.Info("web interface: listens on %s, net0's address", net.JoinHostPort(addr.String(), webcert.Port))
	return nil
}

func webURL(addr netip.Addr) string {
	return "https://" + net.JoinHostPort(addr.String(), webcert.Port) + "/"
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
	if r.web.url == "" {
		// A run resumed after the address was written reads it back.
		if out, err := r.exec(ctx, vmid, "cat", webcert.Net0File); err == nil {
			if addr, err := netip.ParseAddr(strings.TrimSpace(out)); err == nil {
				r.web.url = webURL(addr)
			}
		}
	}
	r.web.fingerprint = fp
	r.ask.Info("web interface: started, certificate SHA-256 %s", fp)
	return nil
}

// repairWeb gives an appliance installed before its web interface what the
// installer gives one now: net0's address and the environment of pco-web
// before the daemon starts again, and pco-web started once it has its
// certificate. What is there is left as it is; before tells which half.
func (r *run) repairWeb(ctx context.Context, before bool) error {
	vmid := r.j.VMID
	if before {
		missing, err := r.missing(ctx, vmid, "test", "-f", webcert.Net0File)
		if err != nil || !missing {
			return err
		}
		noEnv, err := r.missing(ctx, vmid, "test", "-f", webcert.EnvFile)
		if err != nil {
			return err
		}
		return r.writeWebFiles(ctx, noEnv)
	}
	disabled, err := r.missing(ctx, vmid, "systemctl", "is-enabled", "--quiet", "pco-web.service")
	if err != nil || !disabled {
		return err
	}
	return r.startWeb(ctx)
}

// missing runs a test in the container: true when it exits with 1, false
// when it passes.
func (r *run) missing(ctx context.Context, vmid int, test ...string) (bool, error) {
	_, err := r.exec(ctx, vmid, test...)
	if err == nil {
		return false, nil
	}
	var code interface{ ExitCode() int }
	if errors.As(err, &code) && code.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("looking at lxc/%d (%s): %w", vmid, strings.Join(test, " "), err)
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
