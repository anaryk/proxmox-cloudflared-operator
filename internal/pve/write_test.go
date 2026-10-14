package pve

import (
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// written is a request that changes something, as the test server read it.
type written struct {
	method, path string
	form         url.Values
	contentType  string
	auth         string
}

// newWriteServer answers every request with the reply and remembers what was
// sent.
func newWriteServer(t *testing.T, rep reply) (*Client, func() []written) {
	t.Helper()
	var mu sync.Mutex
	var seen []written
	c := startServer(t, testConfig(""), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		form, err := url.ParseQuery(string(body))
		require.NoError(t, err)
		mu.Lock()
		seen = append(seen, written{
			method: r.Method, path: r.URL.Path, form: form,
			contentType: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization"),
		})
		mu.Unlock()
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	return c, func() []written {
		mu.Lock()
		defer mu.Unlock()
		return append([]written(nil), seen...)
	}
}

func TestSetGuestConfigPutsTheValuesUnderTheDigest(t *testing.T) {
	c, seen := newWriteServer(t, reply{http.StatusOK, `{"data":null}`})
	err := c.SetGuestConfig(t.Context(), "pve1", appliance, map[string]string{
		"net1": "name=leg1,bridge=vmbr1,hwaddr=02:50:43:4f:00:01,firewall=0",
	}, "256cbbe2f7cd3a686dc9ab8854c9a7f61057830b")
	require.NoError(t, err)

	got := seen()
	require.Len(t, got, 1)
	require.Equal(t, http.MethodPut, got[0].method)
	require.Equal(t, "/api2/json/nodes/pve1/lxc/9201/config", got[0].path)
	require.Equal(t, "application/x-www-form-urlencoded", got[0].contentType)
	require.Equal(t, "PVEAPIToken="+testTokenID+"="+testSecret, got[0].auth)
	require.Equal(t, url.Values{
		"net1":   {"name=leg1,bridge=vmbr1,hwaddr=02:50:43:4f:00:01,firewall=0"},
		"digest": {"256cbbe2f7cd3a686dc9ab8854c9a7f61057830b"},
	}, got[0].form)
}

func TestDeleteGuestConfigKeysPutsTheDelete(t *testing.T) {
	c, seen := newWriteServer(t, reply{http.StatusOK, `{"data":null}`})
	vm := model.GuestRef{Kind: model.KindQEMU, VMID: 9220}
	require.NoError(t, c.DeleteGuestConfigKeys(t.Context(), "pve1", vm, []string{"net3", "net4"}, "d1"))

	got := seen()
	require.Len(t, got, 1)
	require.Equal(t, http.MethodPut, got[0].method)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/9220/config", got[0].path)
	require.Equal(t, url.Values{"delete": {"net3,net4"}, "digest": {"d1"}}, got[0].form)
}

// Proxmox says that the configuration changed since its digest was read with
// a 500 and these words; that is ErrStaleDigest, which a caller answers by
// reading the configuration again.
func TestAStaleDigestIsErrStaleDigest(t *testing.T) {
	c, _ := newWriteServer(t, reply{http.StatusInternalServerError,
		`{"data":null,"message":"detected modified configuration - file changed by other user? Try again.\n"}`})
	err := c.SetGuestConfig(t.Context(), "pve1", appliance, map[string]string{"net1": "name=leg1,bridge=vmbr1"}, "old")
	require.ErrorIs(t, err, ErrStaleDigest)
	err = c.DeleteGuestConfigKeys(t.Context(), "pve1", appliance, []string{"net1"}, "old")
	require.ErrorIs(t, err, ErrStaleDigest)
}

// A file that changed between Proxmox's read of it and its write is said in
// other words.
func TestAChecksumMismatchIsErrStaleDigest(t *testing.T) {
	c, _ := newWriteServer(t, reply{http.StatusInternalServerError,
		`{"data":null,"message":"checksum mismatch (file change by other user?)\n"}`})
	err := c.SetGuestConfig(t.Context(), "pve1", appliance, map[string]string{"net1": "name=leg1,bridge=vmbr1"}, "old")
	require.ErrorIs(t, err, ErrStaleDigest)
}

func TestAWriteThatIsRefusedSaysSo(t *testing.T) {
	c, _ := newWriteServer(t, reply{http.StatusForbidden, `{"data":null,"message":"Permission check failed (/vms/9201, VM.Config.Network)"}`})
	err := c.SetGuestConfig(t.Context(), "pve1", appliance, map[string]string{"net1": "name=leg1,bridge=vmbr1"}, "d")
	require.True(t, IsForbidden(err), "%v", err)
	require.NotErrorIs(t, err, ErrStaleDigest)
	require.ErrorContains(t, err, "VM.Config.Network")
}

func TestAWriteNamesNoKeyThatIsNoKey(t *testing.T) {
	c, seen := newWriteServer(t, reply{http.StatusOK, `{"data":null}`})
	for _, values := range []map[string]string{nil, {"": "x"}, {"net1&delete=net0": "x"}, {"Net1": "x"}} {
		require.Error(t, c.SetGuestConfig(t.Context(), "pve1", appliance, values, "d"), "%v", values)
	}
	for _, keys := range [][]string{nil, {""}, {"net1,net0"}} {
		require.Error(t, c.DeleteGuestConfigKeys(t.Context(), "pve1", appliance, keys, "d"), "%v", keys)
	}
	require.Error(t, c.SetGuestConfig(t.Context(), "pve1/../x", appliance, map[string]string{"net1": "x"}, "d"))
	require.Empty(t, seen(), "nothing is sent")
}

func TestPendingChangesListsTheKeysThatWaitForARestart(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/nodes/pve1/qemu/9220/pending": {http.StatusOK, `{"data":[
		{"key":"name","value":"web"},
		{"key":"net2","value":"virtio=06:50:43:4F:76:05,bridge=pconet","pending":"virtio=06:50:43:4F:76:05,bridge=pconet,link_down=1"},
		{"key":"net1","value":"virtio=BC:24:11:92:02:20,bridge=vmbr1","delete":1},
		{"key":"net3","pending":"virtio=06:50:43:4F:76:06,bridge=pconet"},
		{"key":"digest","value":"abc"}
	]}`}})
	got, err := c.PendingChanges(t.Context(), "pve1", 9220)
	require.NoError(t, err)
	require.Equal(t, "/api2/json/nodes/pve1/qemu/9220/pending", rec.requests()[0].uri)
	require.Equal(t, []string{"net1", "net2", "net3"}, got)

	c, _ = newTestClient(t, map[string]reply{"/nodes/pve1/qemu/9220/pending": {http.StatusOK, `{"data":[{"key":"name","value":"web"}]}`}})
	got, err = c.PendingChanges(t.Context(), "pve1", 9220)
	require.NoError(t, err)
	require.Empty(t, got)
}
