package doctor

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// WebCert is the certificate of the web interface as the node has it, and in
// the appliance the address it listens on and net0's.
type WebCert struct {
	Mode      string // webcert.ModeCA, ModeOwn, ModePVEProxy or ModeSelfSigned
	Cert, Key []byte // tls.crt and tls.key
	Err       error  // why they could not be read

	Appliance bool
	VMID      int        // of the appliance
	Listen    string     // PCO_WEB_LISTEN, else net0's address and port 8643
	ListenErr error      // why the environment of the unit could not be read
	Net0      netip.Addr // as /etc/pco/net0 has it
	Net0Err   error
	Live      []netip.Addr // the global IPv4 addresses of net0's card now
	LiveErr   error
}

// checkWebCert says how the certificate of the web interface stands: its
// mode, until when it is valid and its fingerprint, which a browser shows.
func checkWebCert(w WebCert, now time.Time) Finding {
	const check = "web certificate"
	fix := webCertFix(w.Mode)
	if w.Err != nil {
		return fail(check, fmt.Sprintf("mode %s: the certificate cannot be read: %v", w.Mode, w.Err), fix)
	}
	leaf, err := webcert.ParseCert(w.Cert)
	if err != nil {
		return fail(check, fmt.Sprintf("mode %s: tls.crt: %v", w.Mode, err), fix)
	}
	key, err := webcert.ParseKey(w.Key)
	switch {
	case err != nil:
		return fail(check, fmt.Sprintf("mode %s: tls.key: %v", w.Mode, err), fix)
	case !webcert.Matches(leaf, key):
		return fail(check, fmt.Sprintf("mode %s: tls.key does not match tls.crt", w.Mode), fix)
	}
	until := leaf.NotAfter.UTC().Format(time.RFC3339)
	detail := fmt.Sprintf("mode %s, valid until %s, SHA-256 fingerprint %s", w.Mode, until, webcert.Fingerprint(leaf))
	switch left := leaf.NotAfter.Sub(now); {
	case left <= 0:
		return fail(check, fmt.Sprintf("mode %s: the certificate expired at %s", w.Mode, until), fix)
	case left >= webcert.RenewBefore:
	case w.Mode == webcert.ModeOwn:
		return warn(check, detail+"; it expires in "+days(left), fix)
	case w.Mode == webcert.ModeCA || w.Mode == webcert.ModeSelfSigned:
		return warn(check, detail+"; it expires in "+days(left)+", and pco renews it 30 days before",
			"journalctl -u pco says why it was not renewed; pco web cert renew")
	}
	return ok(check, detail)
}

// checkWebListen says whether the web interface of the appliance listens on
// net0's address only. Its other cards are legs into the networks of guests,
// which would reach the sign-in page, and through it try the passwords of
// the cluster, where Proxmox VE's own page is out of their reach.
func checkWebListen(w WebCert) Finding {
	const check = "web listen"
	const fixListen = "take PCO_WEB_LISTEN out of /etc/default/pco-web, or give it net0's address, then systemctl restart pco-web"
	vmid := "<vmid>"
	if w.VMID > 0 {
		vmid = strconv.Itoa(w.VMID)
	}
	switch {
	case w.ListenErr != nil:
		return warn(check, fmt.Sprintf("the address pco-web listens on cannot be read: %v", w.ListenErr), fixListen)
	case w.Net0Err != nil:
		return warn(check, fmt.Sprintf("net0's address is not known (%v), so pco-web refuses to start", w.Net0Err),
			RepairFix(vmid)+", which writes it")
	case w.LiveErr != nil:
		return warn(check, fmt.Sprintf("the addresses of net0's card cannot be read: %v", w.LiveErr), "journalctl -u pco says more")
	case len(w.Live) == 0:
		return fail(check, fmt.Sprintf("net0's card has no IPv4 address; pco-web cannot listen on %s", w.Net0),
			"check the DHCP server of net0's bridge, or give net0 a static address with pct set "+vmid+" --net0 ...,ip=<cidr>")
	case !slices.Contains(w.Live, w.Net0):
		return fail(check, fmt.Sprintf("%s says %s, but net0's card has %s now: pco-web cannot listen there", webcert.Net0File, w.Net0, joinAddrs(w.Live)),
			"the daemon writes the card's address there and restarts pco-web within a minute; journalctl -u pco says why it did not")
	case w.Listen == "":
		return warn(check, "pco-web has no address to listen on", fixListen)
	}
	if err := webcert.CheckListen(w.Listen, w.Net0); err != nil {
		return warn(check, fmt.Sprintf("pco-web listens on %s, which is not net0's (%s): guests on a leg may reach the sign-in page", w.Listen, w.Net0), fixListen)
	}
	return ok(check, "pco-web listens on "+w.Listen+", net0's address")
}

func joinAddrs(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// webCertFix is what puts a certificate of a mode right.
func webCertFix(mode string) string {
	switch mode {
	case webcert.ModeOwn:
		return "pco web cert import <crt> <key>"
	case webcert.ModePVEProxy:
		return "pco setup --repair"
	}
	return "pco web cert renew"
}

// WebCert reads the certificate of the web interface, within the timeout;
// false when setup did not set the web interface up.
func (h *HostEnv) WebCert(ctx context.Context) (WebCert, bool) {
	if h.Web == nil {
		return WebCert{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout())
	defer cancel()
	type read struct {
		w       WebCert
		enabled bool
	}
	done := make(chan read, 1)
	go func() {
		w, enabled := h.Web(ctx)
		done <- read{w, enabled}
	}()
	select {
	case r := <-done:
		return r.w, r.enabled
	case <-ctx.Done():
		return WebCert{Err: fmt.Errorf("no answer within %s: %w", h.timeout(), ctx.Err())}, true
	}
}
