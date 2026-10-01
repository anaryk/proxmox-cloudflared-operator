package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

const (
	adoptedFile = "adopted.jsonl"

	// maxAdoptedSize caps the log of adopted records; the oldest lines go first.
	maxAdoptedSize = 256 << 10
)

// adoptedEntry is one line of the log.
type adoptedEntry struct {
	At     time.Time     `json:"at"`
	Zone   string        `json:"zone"`
	Record adoptedRecord `json:"record"`
}

// adoptedRecord is a DNS record as the log keeps it.
type adoptedRecord struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Content    string    `json:"content"`
	Proxied    bool      `json:"proxied"`
	Comment    string    `json:"comment,omitempty"`
	ModifiedOn time.Time `json:"modifiedOn,omitzero"`
}

// AppendAdopted records a DNS record as it was before an adoption replaced it,
// as one JSON line of <cluster>/adopted.jsonl. The file is capped at 256 KiB by
// dropping the oldest lines, and the newest line always stays.
//
// pmxcfs has no append worth relying on, so the file is read, extended and
// written again through a temporary file.
func (s *Store) AppendAdopted(at time.Time, zone string, rec cfapi.Record) error {
	line, err := json.Marshal(adoptedEntry{
		At:   at.UTC(),
		Zone: zone,
		Record: adoptedRecord{
			ID: rec.ID, Type: rec.Type, Name: rec.Name, Content: rec.Content,
			Proxied: rec.Proxied, Comment: rec.Comment, ModifiedOn: rec.ModifiedOn.UTC(),
		},
	})
	if err != nil {
		return fmt.Errorf("encoding the adopted record %s: %w", rec.Name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.paths.Cluster, adoptedFile)
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the adopted records: %w", err)
	}
	if err := ensureDir(s.paths.Cluster); err != nil {
		return fmt.Errorf("storing the adopted record %s: %w", rec.Name, err)
	}
	if err := writeFileAtomic(path, path+tempExt, appendLine(old, line)); err != nil {
		return fmt.Errorf("storing the adopted record %s: %w", rec.Name, err)
	}
	return nil
}

// appendLine returns log with line added and the oldest lines dropped until
// it fits the cap, never dropping the line just added.
func appendLine(log, line []byte) []byte {
	out := make([]byte, 0, len(log)+len(line)+2)
	out = append(out, log...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, line...)
	out = append(out, '\n')
	for len(out) > maxAdoptedSize {
		i := bytes.IndexByte(out, '\n')
		if i < 0 || i == len(out)-1 {
			break
		}
		out = out[i+1:]
	}
	return out
}
