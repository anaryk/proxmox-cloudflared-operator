package applianceinstall

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
)

// sdnRecord is what the node keeps of the SDN objects pco appliance managed
// enable made for an appliance: for which container, told by its VMID and
// the mark of its install, and a random note of each object made, which
// the line of the description that names it carries too. The description
// can be read by whoever audits the container; a note of an object the
// node did not record is in no file.
type sdnRecord struct {
	VMID int    `json:"vmid"`
	Mark string `json:"mark"`
	Zone string `json:"zone,omitempty"`
	Vnet string `json:"vnet,omitempty"`
}

// sdnNotePath is where the node keeps the record of the SDN objects made for
// the appliance vmid: under the private part of the cluster filesystem,
// which only root reads and writes, on whichever node the appliance moves to.
func (r *run) sdnNotePath(vmid int) string {
	return filepath.Join(r.h.pveDir, "priv", "pco-appliance", "sdn-vm"+strconv.Itoa(vmid))
}

// readSDNRecord reads the record the node keeps for the appliance vmid
// whose description starts with mark. A record of another container, of
// another install of the same VMID, or one that cannot be parsed is none.
func (r *run) readSDNRecord(vmid int, mark string) (sdnRecord, error) {
	b, err := r.h.readFile(r.sdnNotePath(vmid))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return sdnRecord{}, nil
	case err != nil:
		return sdnRecord{}, fmt.Errorf("reading the record of the SDN objects of lxc/%d: %w", vmid, err)
	}
	var rec sdnRecord
	if json.Unmarshal(b, &rec) != nil || rec.VMID != vmid || rec.Mark == "" || rec.Mark != mark {
		return sdnRecord{}, nil
	}
	if !madeNote.MatchString(rec.Zone) {
		rec.Zone = ""
	}
	if !madeNote.MatchString(rec.Vnet) {
		rec.Vnet = ""
	}
	return rec, nil
}

// recordSDN keeps in the record of the appliance vmid, whose description
// starts with mark, a note for each object of zone and vnet, drawing one
// for an object that has none yet, and drops the note of an object that is
// no longer made for it.
func (r *run) recordSDN(vmid int, mark string, zone, vnet bool) (sdnRecord, error) {
	rec, err := r.readSDNRecord(vmid, mark)
	if err != nil {
		return sdnRecord{}, err
	}
	rec.VMID, rec.Mark = vmid, mark
	for _, o := range []struct {
		made bool
		note *string
	}{{zone, &rec.Zone}, {vnet, &rec.Vnet}} {
		switch {
		case !o.made:
			*o.note = ""
		case *o.note == "":
			b := make([]byte, 16)
			if _, err := io.ReadFull(r.rand, b); err != nil {
				return sdnRecord{}, fmt.Errorf("reading random bytes: %w", err)
			}
			*o.note = hex.EncodeToString(b)
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return sdnRecord{}, err
	}
	path := r.sdnNotePath(vmid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return sdnRecord{}, fmt.Errorf("keeping the record of the SDN objects of lxc/%d: %w", vmid, err)
	}
	if err := atomicfile.Write(path, append(b, '\n'), atomicfile.Options{Mode: 0o600}); err != nil {
		return sdnRecord{}, fmt.Errorf("keeping the record of the SDN objects of lxc/%d: %w", vmid, err)
	}
	return rec, nil
}

// removeSDNNote removes the record the node keeps for the appliance vmid:
// once its container is gone, and before a container of that VMID is made,
// so that a record of an earlier install never counts for a later one.
func (r *run) removeSDNNote(vmid int) error {
	if err := os.Remove(r.sdnNotePath(vmid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the record of the SDN objects of lxc/%d: %w", vmid, err)
	}
	return nil
}

// firstLine is the first line of a description, the mark of its install.
func firstLine(desc string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(desc), "\n")
	return strings.TrimSpace(first)
}
