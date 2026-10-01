package cffake

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// Cloudflare answers these two refusals with a 400 and a code. The first is
// also the refusal of a CNAME next to any other record of its name.
const (
	messageNameExists = "An A, AAAA, or CNAME record with that host already exists."
	messageIdentical  = "An identical record already exists."
)

func (f *Fake) zone(id string) (cfapi.Zone, error) {
	i := slices.IndexFunc(f.zones, func(z cfapi.Zone) bool { return z.ID == id })
	if i < 0 {
		return cfapi.Zone{}, notFound("zone", id)
	}
	return f.zones[i], nil
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

// Records lists what the filter asks for, the same way the client checks
// the answer: type, name and comment prefix are matched without regard to case.
func (f *Fake) Records(ctx context.Context, zoneID string, filter cfapi.RecordFilter) ([]cfapi.Record, error) {
	if err := cfapi.CheckID("zone id", zoneID); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSRead, "Records", zoneID); err != nil {
		return nil, err
	}
	if _, err := f.zone(zoneID); err != nil {
		return nil, err
	}
	var out []cfapi.Record
	for _, r := range f.records[zoneID] {
		if filter.Matches(r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// qualify gives a record name the form Cloudflare stores: lower case, with the
// zone name added to a name that is not inside the zone, and "@" meaning the
// zone itself.
func qualify(name, zone string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	zone = strings.ToLower(zone)
	switch {
	case name == "@":
		return zone
	case zone == "" || name == zone || strings.HasSuffix(name, "."+zone):
		return name
	}
	return name + "." + zone
}

// clashes reports whether Cloudflare would refuse to hold both records in the
// zone of name apex. A CNAME stands alone: no other record may share its name.
// At the apex, where Cloudflare flattens a CNAME, only an address record or
// another CNAME clashes with one. Any number of A and AAAA records may share
// a name.
func clashes(a, b cfapi.Record, apex string) bool {
	switch {
	case !strings.EqualFold(a.Name, b.Name) || !isType(a, "CNAME") && !isType(b, "CNAME"):
		return false
	case strings.EqualFold(a.Name, apex):
		return isAddress(a) && isAddress(b)
	}
	return true
}

func isAddress(r cfapi.Record) bool {
	return isType(r, "A") || isType(r, "AAAA") || isType(r, "CNAME")
}

// storedTTL is the TTL Cloudflare keeps for r: a proxied record has no TTL of
// its own and shows 1, automatic, as does a record written without one.
func storedTTL(r cfapi.Record) int {
	if r.Proxied {
		return 1
	}
	return cfapi.SentTTL(r.TTL)
}

// identical reports whether the records have the same type, name and content,
// which Cloudflare refuses whatever the type.
func identical(a, b cfapi.Record) bool {
	return isType(a, b.Type) && strings.EqualFold(a.Name, b.Name) && a.Content == b.Content
}

func isType(r cfapi.Record, typ string) bool { return strings.EqualFold(r.Type, typ) }

// checkRecord says why r cannot be stored next to the records of zone z, not
// counting the record with id except.
func (f *Fake) checkRecord(z cfapi.Zone, r cfapi.Record, except string) error {
	for _, other := range f.records[z.ID] {
		switch {
		case other.ID == except:
		case identical(other, r):
			return &cfapi.Error{Status: http.StatusBadRequest, Codes: []int{codeIdentical}, Message: messageIdentical}
		case clashes(other, r, z.Name):
			return &cfapi.Error{Status: http.StatusBadRequest, Codes: []int{codeNameExists}, Message: messageNameExists}
		}
	}
	return nil
}

// checkRecordFields checks the arguments the client would refuse to send.
func checkRecordFields(r cfapi.Record) error {
	if err := cfapi.CheckName("record type", r.Type); err != nil {
		return err
	}
	return cfapi.CheckName("record name", r.Name)
}

// CreateRecord ignores the ID and ModifiedOn of r. The name is stored in lower
// case and, when it is not inside the zone, with the zone name added, and the
// TTL of a proxied record or an unset one as 1, automatic.
func (f *Fake) CreateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	if err := cfapi.CheckID("zone id", zoneID); err != nil {
		return cfapi.Record{}, err
	}
	if err := checkRecordFields(r); err != nil {
		return cfapi.Record{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSWrite, "CreateRecord", zoneID, r.Name); err != nil {
		return cfapi.Record{}, err
	}
	z, err := f.zone(zoneID)
	if err != nil {
		return cfapi.Record{}, err
	}
	r.Name = qualify(r.Name, z.Name)
	r.TTL = storedTTL(r)
	if err := f.checkRecord(z, r, ""); err != nil {
		return cfapi.Record{}, err
	}
	r.ID = f.newRecordID()
	r.ModifiedOn = f.now()
	f.records[zoneID] = append(f.records[zoneID], r)
	return r, nil
}

// UpdateRecord replaces the record that has the ID of r, which keeps its
// place. The name is treated as CreateRecord does.
func (f *Fake) UpdateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	if err := cfapi.CheckID("zone id", zoneID); err != nil {
		return cfapi.Record{}, err
	}
	if err := cfapi.CheckID("record id", r.ID); err != nil {
		return cfapi.Record{}, err
	}
	if err := checkRecordFields(r); err != nil {
		return cfapi.Record{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSWrite, "UpdateRecord", zoneID, r.ID); err != nil {
		return cfapi.Record{}, err
	}
	z, err := f.zone(zoneID)
	if err != nil {
		return cfapi.Record{}, err
	}
	i := slices.IndexFunc(f.records[zoneID], func(x cfapi.Record) bool { return x.ID == r.ID })
	if i < 0 {
		return cfapi.Record{}, notFound("record", r.ID)
	}
	r.Name = qualify(r.Name, z.Name)
	r.TTL = storedTTL(r)
	if err := f.checkRecord(z, r, r.ID); err != nil {
		return cfapi.Record{}, err
	}
	r.ModifiedOn = f.now()
	f.records[zoneID][i] = r
	return r, nil
}

func (f *Fake) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	if err := cfapi.CheckID("zone id", zoneID); err != nil {
		return err
	}
	if err := cfapi.CheckID("record id", recordID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opDNSWrite, "DeleteRecord", zoneID, recordID); err != nil {
		return err
	}
	if _, err := f.zone(zoneID); err != nil {
		return err
	}
	i := slices.IndexFunc(f.records[zoneID], func(x cfapi.Record) bool { return x.ID == recordID })
	if i < 0 {
		return notFound("record", recordID)
	}
	f.records[zoneID] = slices.Delete(f.records[zoneID], i, i+1)
	return nil
}
