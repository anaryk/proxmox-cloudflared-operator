package cffake

import (
	"net/http"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// What the API gives of a listing of records when no page size is asked for,
// and from least to most what it accepts.
const (
	defaultRecordPage, minRecordPage, maxRecordPage = 100, 1, 5_000_000
)

type wireRecord struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Content    string    `json:"content"`
	Proxied    bool      `json:"proxied"`
	TTL        int       `json:"ttl"`
	Comment    *string   `json:"comment"` // null for none, as the API sends it
	ModifiedOn time.Time `json:"modified_on"`
}

func newWireRecord(r cfapi.Record) wireRecord {
	w := wireRecord{ID: r.ID, Type: r.Type, Name: r.Name, Content: r.Content, Proxied: r.Proxied, TTL: r.TTL, ModifiedOn: r.ModifiedOn.UTC()}
	if r.Comment != "" {
		w.Comment = &r.Comment
	}
	return w
}

func (h *handler) listRecords(r *http.Request, p params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page", "type", "name", "comment.startswith"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultRecordPage, minRecordPage, maxRecordPage)
	if err != nil {
		return reply{}, err
	}
	filter := cfapi.RecordFilter{Type: q.Get("type"), Name: q.Get("name"), CommentPrefix: q.Get("comment.startswith")}
	all, err := h.f.Records(r.Context(), p["zone"], filter)
	if err != nil {
		return reply{}, err
	}
	page := pageOf(all, pg)
	items := make([]wireRecord, len(page))
	for i, rec := range page {
		items[i] = newWireRecord(rec)
	}
	return reply{items, pg.info(len(items), len(all), true)}, nil
}

// readRecord reads the body of a write of a record. A new record has to name
// its type, name and content. A change has to carry every field: Cloudflare
// leaves out of a change what the body does not carry, the fake replaces the
// record, and the client says everything it means.
func readRecord(r *http.Request, change bool) (cfapi.Record, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return cfapi.Record{}, err
	}
	data, err := readBody(r)
	if err != nil {
		return cfapi.Record{}, err
	}
	var body struct {
		Type    *string `json:"type"`
		Name    *string `json:"name"`
		Content *string `json:"content"`
		Proxied *bool   `json:"proxied"`
		Comment *string `json:"comment"`
		TTL     *int    `json:"ttl"`
	}
	if err := decodeObject(data, &body, "type", "name", "content", "proxied", "comment", "ttl"); err != nil {
		return cfapi.Record{}, err
	}
	for _, f := range []struct {
		name            string
		present, needed bool
	}{
		{"type", body.Type != nil, true},
		{"name", body.Name != nil, true},
		{"content", body.Content != nil, true},
		{"proxied", body.Proxied != nil, change},
		{"comment", body.Comment != nil, change},
		{"ttl", body.TTL != nil, change},
	} {
		if f.needed && !f.present {
			return cfapi.Record{}, badRequest("%s is required", f.name)
		}
	}
	if body.TTL != nil && !validTTL(*body.TTL) {
		// The fake would take 0 for no TTL, as a Go caller means it; on the wire
		// it is a TTL that Cloudflare refuses.
		return cfapi.Record{}, errBadTTL()
	}
	rec := cfapi.Record{Type: deref(body.Type), Name: deref(body.Name), Content: deref(body.Content), Comment: deref(body.Comment)}
	if body.Proxied != nil {
		rec.Proxied = *body.Proxied
	}
	if body.TTL != nil {
		rec.TTL = *body.TTL
	}
	return rec, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *handler) createRecord(r *http.Request, p params) (reply, error) {
	rec, err := readRecord(r, false)
	if err != nil {
		return reply{}, err
	}
	created, err := h.f.CreateRecord(r.Context(), p["zone"], rec)
	if err != nil {
		return reply{}, err
	}
	return reply{result: newWireRecord(created)}, nil
}

func (h *handler) updateRecord(r *http.Request, p params) (reply, error) {
	rec, err := readRecord(r, true)
	if err != nil {
		return reply{}, err
	}
	rec.ID = p["record"]
	updated, err := h.f.UpdateRecord(r.Context(), p["zone"], rec)
	if err != nil {
		return reply{}, err
	}
	return reply{result: newWireRecord(updated)}, nil
}

func (h *handler) deleteRecord(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	if err := h.f.DeleteRecord(r.Context(), p["zone"], p["record"]); err != nil {
		return reply{}, err
	}
	return reply{result: map[string]string{"id": p["record"]}}, nil
}
