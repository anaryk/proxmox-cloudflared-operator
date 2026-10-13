package applianceinstall

import (
	"encoding/hex"
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

// sdnNotePath is where the node keeps the note of the SDN objects made for
// the appliance vmid: under the private part of the cluster filesystem,
// which only root reads and writes, on whichever node the appliance moves to.
func (r *run) sdnNotePath(vmid int) string {
	return filepath.Join(r.h.pveDir, "priv", "pco-appliance", "sdn-vm"+strconv.Itoa(vmid))
}

// readSDNNote reads the note the node keeps for the appliance vmid; empty
// when there is none.
func (r *run) readSDNNote(vmid int) (string, error) {
	b, err := r.h.readFile(r.sdnNotePath(vmid))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("reading the note of the SDN objects of lxc/%d: %w", vmid, err)
	}
	note := strings.TrimSpace(string(b))
	if !madeNote.MatchString(note) {
		return "", nil
	}
	return note, nil
}

// ensureSDNNote returns the note the node keeps for the appliance vmid, and
// draws and keeps one where there is none yet.
func (r *run) ensureSDNNote(vmid int) (string, error) {
	note, err := r.readSDNNote(vmid)
	if err != nil || note != "" {
		return note, err
	}
	b := make([]byte, 16)
	if _, err := io.ReadFull(r.rand, b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	note = hex.EncodeToString(b)
	path := r.sdnNotePath(vmid)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("keeping the note of the SDN objects of lxc/%d: %w", vmid, err)
	}
	if err := atomicfile.Write(path, []byte(note+"\n"), atomicfile.Options{Mode: 0o600}); err != nil {
		return "", fmt.Errorf("keeping the note of the SDN objects of lxc/%d: %w", vmid, err)
	}
	return note, nil
}

// removeSDNNote removes the note the node keeps for the appliance vmid,
// once its container is gone.
func (r *run) removeSDNNote(vmid int) error {
	if err := os.Remove(r.sdnNotePath(vmid)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the note of the SDN objects of lxc/%d: %w", vmid, err)
	}
	return nil
}
