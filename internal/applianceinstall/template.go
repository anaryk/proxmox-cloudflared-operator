package applianceinstall

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	taskPoll     = 2 * time.Second
	taskPollsMax = 900 // half an hour
)

// prepareTemplate takes the template given as a file, or the one on the
// template storage when its checksum is the release's, or downloads it
// through Proxmox, which checks the checksum.
func (r *run) prepareTemplate(ctx context.Context) error {
	if r.o.Template != "" {
		return r.localTemplate()
	}
	name := templateName(r.h.version, r.arch)
	sum, err := r.checksum(name)
	switch {
	case err != nil:
		return err
	case sum == "":
		return fmt.Errorf("%s is not in --checksums %s, so it cannot be downloaded: give it with --template", name, r.o.ChecksumsFile)
	}
	volid := r.o.TemplateStorage + ":vztmpl/" + name
	have, err := templates(ctx, r.r, r.node, r.o.TemplateStorage)
	if err != nil {
		return err
	}
	if slices.Contains(have, volid) {
		if err := r.checkVolume(ctx, volid, sum); err != nil {
			return err
		}
		r.j.Template = volid
		r.ask.Info("template: %s is there, its checksum is the release's", volid)
		return nil
	}
	if err := r.record(func(j *journal) { r.appliance().Template = volid }); err != nil {
		return err
	}
	if err := r.download(ctx, name, sum); err != nil {
		if have, lerr := templates(ctx, r.r, r.node, r.o.TemplateStorage); lerr == nil && !slices.Contains(have, volid) {
			_ = r.record(func(j *journal) { r.appliance().Template = "" })
		}
		return err
	}
	r.j.Template = volid
	r.ask.Info("template: downloaded %s, checksum verified", volid)
	return nil
}

func (r *run) localTemplate() error {
	info, err := os.Stat(r.o.Template)
	if err != nil {
		return fmt.Errorf("--template: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("--template %s is not a file", r.o.Template)
	}
	sum, err := r.checksum(filepath.Base(r.o.Template))
	if err != nil {
		return err
	}
	if sum != "" {
		got, err := fileSHA256(r.o.Template)
		if err != nil {
			return err
		}
		if got != sum {
			return fmt.Errorf("--template %s has sha256 %s, not %s as --checksums says", r.o.Template, got, sum)
		}
	}
	r.j.Template = r.o.Template
	r.ask.Info("template: %s", r.o.Template)
	return nil
}

// checksum returns the sha256 --checksums gives for the file, empty when it
// names none or there is no --checksums.
func (r *run) checksum(name string) (string, error) {
	if r.o.ChecksumsFile == "" {
		if r.o.Template != "" {
			return "", nil
		}
		return "", errors.New("the template is downloaded with the checksum the release lists: give its checksums.txt with --checksums, or the template with --template")
	}
	b, err := r.h.readFile(r.o.ChecksumsFile)
	if err != nil {
		return "", fmt.Errorf("reading --checksums: %w", err)
	}
	return checksumOf(b, name), nil
}

// checksumOf finds the sha256 of a file in a checksums.txt as sha256sum
// writes it.
func checksumOf(list []byte, name string) string {
	sc := bufio.NewScanner(bytes.NewReader(list))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			if _, err := hex.DecodeString(f[0]); err == nil {
				return strings.ToLower(f[0])
			}
		}
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkVolume compares the checksum of a template on a storage with the
// release's.
func (r *run) checkVolume(ctx context.Context, volid, sum string) error {
	out, err := r.r.Run(ctx, "pvesm", "path", volid)
	if err != nil {
		return fmt.Errorf("finding the file of %s: %w", volid, err)
	}
	got, err := fileSHA256(strings.TrimSpace(out))
	if err != nil {
		return err
	}
	if got != sum {
		return fmt.Errorf("%s is there with sha256 %s, not the release's %s: remove it with pvesm free %s", volid, got, sum, volid)
	}
	return nil
}

// download has Proxmox fetch the template and check its checksum. pvesh
// exits 0 also when the download fails: the task says how it ended.
func (r *run) download(ctx context.Context, name, sum string) error {
	base := strings.TrimRight(r.o.ReleaseBase, "/")
	if base == "" {
		base = ReleaseBase(r.h.version)
	}
	out, err := r.r.Run(ctx, "pvesh", "create", fmt.Sprintf("/nodes/%s/storage/%s/download-url", r.node, r.o.TemplateStorage),
		"--content", "vztmpl", "--filename", name, "--url", base+"/"+name,
		"--checksum", sum, "--checksum-algorithm", "sha256")
	if err != nil {
		return fmt.Errorf("downloading %s: %w", name, err)
	}
	upid := ""
	for line := range strings.Lines(out) {
		if l := strings.Trim(strings.TrimSpace(line), `"`); strings.HasPrefix(l, "UPID:") {
			upid = l
		}
	}
	if upid == "" {
		return fmt.Errorf("downloading %s: pvesh named no task", name)
	}
	return r.waitTask(ctx, upid, "downloading "+name)
}

func (r *run) waitTask(ctx context.Context, upid, what string) error {
	for range taskPollsMax {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := pvesh(ctx, r.r, &st, fmt.Sprintf("/nodes/%s/tasks/%s/status", r.node, upid)); err != nil {
			return fmt.Errorf("%s: reading task %s: %w", what, upid, err)
		}
		if st.Status == "stopped" {
			if st.ExitStatus != "OK" {
				return fmt.Errorf("%s: the task ended with %q", what, st.ExitStatus)
			}
			return nil
		}
		if err := r.h.sleep(ctx, taskPoll); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return fmt.Errorf("%s: task %s did not end within half an hour", what, upid)
}
