package doctor

import (
	"context"
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/webcert"
)

// WebCert is the certificate of the web interface as the node has it.
type WebCert struct {
	Mode      string // webcert.ModeCA, ModeOwn or ModePVEProxy
	Cert, Key []byte // tls.crt and tls.key
	Err       error  // why they could not be read
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
	case w.Mode == webcert.ModeCA:
		return warn(check, detail+"; it expires in "+days(left)+", and pco renews it 30 days before",
			"journalctl -u pco says why it was not renewed; pco web cert renew")
	}
	return ok(check, detail)
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
