package cfapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// wireRecord has the fields of Record, so that one converts to the other.
type wireRecord struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Content    string    `json:"content"`
	Proxied    bool      `json:"proxied"`
	Comment    string    `json:"comment"`
	ModifiedOn time.Time `json:"modified_on"`
}

// record maps an answer to a Record. A record without an id cannot be updated
// or deleted, so it is not a record.
func (w wireRecord) record() (Record, error) {
	if w.ID == "" {
		return Record{}, fmt.Errorf("%w: dns record without an id", errUnexpected)
	}
	return Record(w), nil
}

// recordBody is what creating or changing a record sends. Every field is
// always present: a missing comment would leave the old one in place.
type recordBody struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment"`
	TTL     int    `json:"ttl"`
}

func newRecordBody(r Record) (recordBody, error) {
	if err := ValidName("record type", r.Type); err != nil {
		return recordBody{}, err
	}
	if err := ValidName("record name", r.Name); err != nil {
		return recordBody{}, err
	}
	// A TTL of 1 means automatic, the only one a proxied record has.
	return recordBody{Type: r.Type, Name: r.Name, Content: r.Content, Proxied: r.Proxied, Comment: r.Comment, TTL: 1}, nil
}

func recordsPath(zoneID string) (string, error) {
	if err := ValidID("zone id", zoneID); err != nil {
		return "", err
	}
	return joinPath("zones", zoneID, "dns_records")
}

func recordPath(zoneID, recordID string) (string, error) {
	if err := ValidID("zone id", zoneID); err != nil {
		return "", err
	}
	if err := ValidID("record id", recordID); err != nil {
		return "", err
	}
	return joinPath("zones", zoneID, "dns_records", recordID)
}

// Records lists the DNS records of a zone that match f. The filter is also
// applied to what comes back, because callers decide what to delete by it: a
// record outside the filter fails the whole call.
func (c *Client) Records(ctx context.Context, zoneID string, f RecordFilter) ([]Record, error) {
	path, err := recordsPath(zoneID)
	if err != nil {
		return nil, fmt.Errorf("listing dns records: %w", err)
	}
	query := url.Values{"per_page": {"100"}}
	if f.Type != "" {
		query.Set("type", f.Type)
	}
	if f.Name != "" {
		query.Set("name", f.Name)
	}
	if f.CommentPrefix != "" {
		query.Set("comment.startswith", f.CommentPrefix)
	}

	var out []Record
	err = listEach(ctx, c, path, query, func(w wireRecord) error {
		r, err := w.record()
		if err != nil {
			return err
		}
		if !f.Matches(r) {
			return fmt.Errorf("%w: cloudflare returned a record outside the requested filter", errUnexpected)
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing dns records of zone %s: %w", zoneID, err)
	}
	return out, nil
}

// CreateRecord creates r, whose ID and ModifiedOn are ignored, and returns the
// record as Cloudflare stored it. A name that already has a conflicting record
// is an error for which IsConflict is true.
func (c *Client) CreateRecord(ctx context.Context, zoneID string, r Record) (Record, error) {
	path, err := recordsPath(zoneID)
	if err != nil {
		return Record{}, fmt.Errorf("creating dns record: %w", err)
	}
	body, err := newRecordBody(r)
	if err != nil {
		return Record{}, fmt.Errorf("creating dns record: %w", err)
	}
	return c.writeRecord(ctx, http.MethodPost, path, body, "creating", r.Name)
}

// UpdateRecord changes the record with the id of r to r and returns the record
// as Cloudflare stored it.
func (c *Client) UpdateRecord(ctx context.Context, zoneID string, r Record) (Record, error) {
	path, err := recordPath(zoneID, r.ID)
	if err != nil {
		return Record{}, fmt.Errorf("updating dns record: %w", err)
	}
	body, err := newRecordBody(r)
	if err != nil {
		return Record{}, fmt.Errorf("updating dns record: %w", err)
	}
	return c.writeRecord(ctx, http.MethodPatch, path, body, "updating", r.Name)
}

func (c *Client) writeRecord(ctx context.Context, method, path string, body recordBody, verb, name string) (Record, error) {
	var got wireRecord
	if err := c.do(ctx, method, path, nil, body, &got); err != nil {
		return Record{}, fmt.Errorf("%s dns record %s: %w", verb, name, err)
	}
	r, err := got.record()
	if err != nil {
		return Record{}, fmt.Errorf("%s dns record %s: %w", verb, name, err)
	}
	return r, nil
}

// DeleteRecord deletes a DNS record.
func (c *Client) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	path, err := recordPath(zoneID, recordID)
	if err != nil {
		return fmt.Errorf("deleting dns record: %w", err)
	}
	if err := c.do(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
		return fmt.Errorf("deleting dns record %s: %w", recordID, err)
	}
	return nil
}
