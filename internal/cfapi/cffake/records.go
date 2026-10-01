package cffake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

func (f *Fake) zone(id string) error {
	if !slices.ContainsFunc(f.zones, func(z cfapi.Zone) bool { return z.ID == id }) {
		return notFound("zone", id)
	}
	return nil
}

// newRecordID returns the next "rec-N" that no zone uses yet, so that ids a
// test seeded are never handed out again.
func (f *Fake) newRecordID() string {
	for {
		f.recordSeq++
		id := fmt.Sprintf("rec-%d", f.recordSeq)
		taken := false
		for _, records := range f.records {
			taken = taken || slices.ContainsFunc(records, func(r cfapi.Record) bool { return r.ID == id })
		}
		if !taken {
			return id
		}
	}
}

// Records filters the way Cloudflare does: the name is exact, the comment is
// matched by prefix, and neither cares about case.
func (f *Fake) Records(ctx context.Context, zoneID string, filter cfapi.RecordFilter) ([]cfapi.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSRead, "Records", zoneID); err != nil {
		return nil, err
	}
	if err := f.zone(zoneID); err != nil {
		return nil, err
	}
	var out []cfapi.Record
	for _, r := range f.records[zoneID] {
		if matches(r, filter) {
			out = append(out, r)
		}
	}
	return out, nil
}

func matches(r cfapi.Record, f cfapi.RecordFilter) bool {
	return (f.Type == "" || strings.EqualFold(r.Type, f.Type)) &&
		(f.Name == "" || strings.EqualFold(r.Name, f.Name)) &&
		(f.CommentPrefix == "" || len(r.Comment) >= len(f.CommentPrefix) &&
			strings.EqualFold(r.Comment[:len(f.CommentPrefix)], f.CommentPrefix))
}

// clashes reports whether Cloudflare would refuse to hold both records. A
// CNAME stands alone: no other record may share its name. TXT records are left
// out of the rule, and any number of A and AAAA records may share a name.
func clashes(a, b cfapi.Record) bool {
	if !strings.EqualFold(a.Name, b.Name) || isType(a, "TXT") || isType(b, "TXT") {
		return false
	}
	return isType(a, "CNAME") || isType(b, "CNAME")
}

func isType(r cfapi.Record, typ string) bool { return strings.EqualFold(r.Type, typ) }

// checkRecord says why r cannot be stored next to the records of the zone,
// not counting the record with id except.
func (f *Fake) checkRecord(zoneID string, r cfapi.Record, except string) error {
	if err := blank("record type", r.Type); err != nil {
		return err
	}
	if err := blank("record name", r.Name); err != nil {
		return err
	}
	for _, other := range f.records[zoneID] {
		if other.ID != except && clashes(other, r) {
			return &cfapi.Error{
				Status: http.StatusBadRequest, Codes: []int{codeNameExists},
				Message: "An A, AAAA, or CNAME record with that host already exists.",
			}
		}
	}
	return nil
}

// CreateRecord ignores the ID and ModifiedOn of r.
func (f *Fake) CreateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSWrite, "CreateRecord", zoneID, r.Name); err != nil {
		return cfapi.Record{}, err
	}
	if err := f.zone(zoneID); err != nil {
		return cfapi.Record{}, err
	}
	if err := f.checkRecord(zoneID, r, ""); err != nil {
		return cfapi.Record{}, err
	}
	r.ID = f.newRecordID()
	r.ModifiedOn = f.now()
	f.records[zoneID] = append(f.records[zoneID], r)
	return r, nil
}

// UpdateRecord replaces the record that has the ID of r, which keeps its place.
func (f *Fake) UpdateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSWrite, "UpdateRecord", zoneID, r.ID); err != nil {
		return cfapi.Record{}, err
	}
	if err := f.zone(zoneID); err != nil {
		return cfapi.Record{}, err
	}
	i := slices.IndexFunc(f.records[zoneID], func(x cfapi.Record) bool { return x.ID == r.ID })
	if i < 0 {
		return cfapi.Record{}, notFound("record", r.ID)
	}
	if err := f.checkRecord(zoneID, r, r.ID); err != nil {
		return cfapi.Record{}, err
	}
	r.ModifiedOn = f.now()
	f.records[zoneID][i] = r
	return r, nil
}

func (f *Fake) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSWrite, "DeleteRecord", zoneID, recordID); err != nil {
		return err
	}
	if err := f.zone(zoneID); err != nil {
		return err
	}
	i := slices.IndexFunc(f.records[zoneID], func(x cfapi.Record) bool { return x.ID == recordID })
	if i < 0 {
		return notFound("record", recordID)
	}
	f.records[zoneID] = slices.Delete(f.records[zoneID], i, i+1)
	return nil
}
