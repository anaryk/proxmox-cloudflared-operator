package credentials

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	probeContent = "pco permission probe"

	// cleanupTimeout bounds the removal of a probe object.
	cleanupTimeout = 30 * time.Second
)

func (r *run) probeComment() string { return planner.ProbeRecordComment(r.checker.installID) }

// writeProbe is one write probe, which creates an object and deletes it again.
// Run only ever deletes an object it knows to be its own: the one a create
// answered with, or the one a lookup by the probe's exact name finds after a
// create that may have gone through.
type writeProbe struct {
	r          *run
	capability Capability
	scope      scope
	kind       string // "record" or "tunnel"
	name       string // of the probe object
	hint       string // what to grant when Cloudflare refuses
}

func (r *run) probeDNSWrite(ctx context.Context, z cfapi.Zone) {
	name := planner.ProbeRecordPrefix + r.suffix + "." + z.Name
	p := writeProbe{
		r: r, capability: CapDNSWrite, scope: zoneScope(z), kind: "record", name: name,
		hint: grant(permDNSEdit, z.Name),
	}
	created, err := r.api.CreateRecord(ctx, z.ID, cfapi.Record{
		Type:    "TXT",
		Name:    name,
		Content: probeContent,
		Comment: r.probeComment(),
	})
	switch {
	case err != nil:
		p.createFailed(ctx, err, func(ctx context.Context) bool { return r.removeStrayRecord(ctx, z, name) })
	case created.ID == "" || !strings.EqualFold(created.Name, name) || created.Comment != r.probeComment():
		p.unexpectedAnswer()
	default:
		p.remove(ctx, func(ctx context.Context) error { return r.api.DeleteRecord(ctx, z.ID, created.ID) })
	}
}

func (r *run) probeTunnelWrite(ctx context.Context, a cfapi.Account) {
	name := planner.ProbeTunnelName(r.checker.installID, r.suffix)
	p := writeProbe{
		r: r, capability: CapTunnelWrite, scope: accountScope(a), kind: "tunnel", name: name,
		hint: grant(permTunnelEdit, a.Name),
	}
	created, err := r.api.CreateTunnel(ctx, a.ID, name)
	switch {
	case err != nil:
		p.createFailed(ctx, err, func(ctx context.Context) bool { return r.removeStrayTunnel(ctx, a, name) })
	case created.ID == "" || created.Name != name:
		p.unexpectedAnswer()
	default:
		p.remove(ctx, func(ctx context.Context) error { return r.api.DeleteTunnel(ctx, a.ID, created.ID) })
	}
}

// createFailed records a create that did not succeed. When the error does not
// rule out that the object was made, removeStray looks for it; the check then
// says so if that failed.
func (p writeProbe) createFailed(ctx context.Context, err error, removeStray func(context.Context) bool) {
	detail := err.Error()
	if cfapi.IsAuth(err) {
		detail = p.hint
	}
	if !refusal(err) && !removeStray(ctx) {
		detail += "; probe may exist: " + p.name
	}
	p.r.fail(p.capability, p.scope, detail)
}

// unexpectedAnswer records a create whose answer is not the probe object, so
// that its id cannot be trusted for a delete.
func (p writeProbe) unexpectedAnswer() {
	p.r.fail(p.capability, p.scope, "unexpected answer to the probe create; nothing deleted; probe may exist: "+p.name)
}

// remove deletes the probe. An object that is already gone counts as deleted.
func (p writeProbe) remove(ctx context.Context, del func(context.Context) error) {
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if err := del(cleanup); err != nil && !cfapi.IsNotFound(err) {
		p.r.fail(p.capability, p.scope, leftBehind(p.kind, p.name, err, p.hint))
		return
	}
	p.r.pass(p.capability, p.scope)
}

// refusal reports whether err says Cloudflare did not act on the request: it
// was refused before it was sent, or answered with a client error. Anything
// else, such as a timeout, a cancellation, a server error or an answer that
// cannot be read, leaves open whether the object was made.
func refusal(err error) bool {
	if errors.Is(err, cfapi.ErrInvalidArgument) {
		return true
	}
	var apiErr *cfapi.Error
	return errors.As(err, &apiErr) &&
		apiErr.Status >= http.StatusBadRequest && apiErr.Status < http.StatusInternalServerError &&
		apiErr.Status != http.StatusRequestTimeout
}

// removeStrayRecord removes the probe record that a create of unknown outcome
// may have made. It looks the record up by its exact name and deletes only a
// record with that name and the comment of a probe. It reports whether the
// lookup and every delete worked.
func (r *run) removeStrayRecord(ctx context.Context, z cfapi.Zone, name string) bool {
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	found, err := r.api.Records(cleanup, z.ID, cfapi.RecordFilter{
		Type:          "TXT",
		Name:          name,
		CommentPrefix: planner.DNSMarker(r.checker.installID),
	})
	if err != nil {
		return false
	}
	clean := true
	for _, rec := range found {
		if !strings.EqualFold(rec.Name, name) || rec.Comment != r.probeComment() {
			continue
		}
		if err := r.api.DeleteRecord(cleanup, z.ID, rec.ID); err != nil && !cfapi.IsNotFound(err) {
			clean = false
		}
	}
	return clean
}

// removeStrayTunnel is removeStrayRecord for the probe tunnel, which is found
// by its exact name.
func (r *run) removeStrayTunnel(ctx context.Context, a cfapi.Account, name string) bool {
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	t, found, err := r.api.FindTunnel(cleanup, a.ID, name)
	if err != nil {
		return false
	}
	if !found || t.Name != name {
		return true
	}
	err = r.api.DeleteTunnel(cleanup, a.ID, t.ID)
	return err == nil || cfapi.IsNotFound(err)
}

// cleanupContext returns the context a probe object is looked up and deleted
// with. A run that was cancelled after it created the object must still remove
// it.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// leftBehind is the detail of a probe that was created and could not be
// deleted; the admin has to remove it. Only an authorisation error is a matter
// of permissions.
func leftBehind(kind, name string, err error, hint string) string {
	detail := "probe " + kind + " left behind: " + name
	if cfapi.IsAuth(err) {
		return detail + "; " + hint
	}
	return detail + " (" + err.Error() + ")"
}
