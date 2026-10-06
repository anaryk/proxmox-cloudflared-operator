package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// ErrStaleDigest says that a guest's configuration changed after the digest a
// write was made under was read: the write changed nothing, and is made again
// on a configuration read anew.
var ErrStaleDigest = errors.New("the configuration changed since it was read")

// SetGuestConfig PUTs the given keys of a guest's configuration under the
// digest; a stale digest is ErrStaleDigest. Proxmox applies a change to a
// running guest at once where it can, and keeps it pending otherwise.
func (c *Client) SetGuestConfig(ctx context.Context, node string, ref model.GuestRef, values map[string]string, digest string) error {
	if len(values) == 0 {
		return errors.New("setting no value of a guest's configuration")
	}
	form := url.Values{}
	for key, value := range values {
		if !validKey(key) {
			return fmt.Errorf("%q is no key of a guest's configuration", key)
		}
		form.Set(key, value)
	}
	return c.writeConfig(ctx, node, ref, form, digest)
}

// DeleteGuestConfigKeys deletes keys of a guest's configuration under the
// digest; a stale digest is ErrStaleDigest.
func (c *Client) DeleteGuestConfigKeys(ctx context.Context, node string, ref model.GuestRef, keys []string, digest string) error {
	if len(keys) == 0 {
		return errors.New("deleting no key of a guest's configuration")
	}
	for _, key := range keys {
		if !validKey(key) {
			return fmt.Errorf("%q is no key of a guest's configuration", key)
		}
	}
	return c.writeConfig(ctx, node, ref, url.Values{"delete": {strings.Join(keys, ",")}}, digest)
}

func (c *Client) writeConfig(ctx context.Context, node string, ref model.GuestRef, form url.Values, digest string) error {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "config")
	if err != nil {
		return err
	}
	if digest != "" {
		form.Set("digest", digest)
	}
	if err := c.put(ctx, endpoint, form); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusInternalServerError &&
			containsFold(apiErr.Message, "detected modified configuration") {
			return fmt.Errorf("changing the config of %s: %w: %w", ref, ErrStaleDigest, err)
		}
		return fmt.Errorf("changing the config of %s: %w", ref, err)
	}
	return nil
}

// PendingChanges lists the keys of a QEMU guest whose change waits for a
// restart: a value that is pending, or a key whose deletion is, sorted.
func (c *Client) PendingChanges(ctx context.Context, node string, vmid int) ([]string, error) {
	endpoint, err := guestEndpoint(node, model.KindQEMU, vmid, "pending")
	if err != nil {
		return nil, err
	}
	var rows []pendingWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching the pending config of qemu/%d: %w", vmid, err)
	}
	var keys []string
	for i, row := range rows {
		if row.Key == "" {
			return nil, fmt.Errorf("fetching the pending config of qemu/%d: entry %d has no key", vmid, i)
		}
		if _, pending := stringify(row.Pending); pending || deleted(row.Delete) {
			keys = append(keys, row.Key)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// validKey accepts the keys of a guest's configuration, as net1 or
// ipconfig0: lower-case letters and digits, so that a key never adds a
// parameter of its own to the form.
func validKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// put calls PUT <base>/api2/json/<endpoint> with the form as its body. The
// answer's data, null for the calls pco makes, is not read.
func (c *Client) put(ctx context.Context, endpoint string, form url.Values) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u := c.base.JoinPath(apiPath, endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.hc.Do(req)
	if err != nil {
		if notVerified(err) {
			c.noteVerify(err)
		}
		return err
	}
	c.noteVerify(nil)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if len(body) > maxBodyBytes {
			body = nil
		}
		return newAPIError(resp.StatusCode, body)
	}
	if len(body) > maxBodyBytes {
		return fmt.Errorf("response exceeds %d bytes", maxBodyBytes)
	}
	if !json.Valid(body) {
		return errors.New("decoding response: not JSON")
	}
	return nil
}
