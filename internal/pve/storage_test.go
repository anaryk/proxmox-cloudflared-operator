package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorage(t *testing.T) {
	tests := []struct {
		id   string
		body string
		want StorageInfo
	}{
		{"nfs1", fixture(t, "storage_nfs.json"), StorageInfo{ID: "nfs1", Type: "nfs", Shared: true}},
		{"local", fixture(t, "storage_local.json"), StorageInfo{ID: "local", Type: "dir"}},
		{"ceph-pool", `{"data":{"storage":"ceph-pool","type":"rbd","shared":"1"}}`, StorageInfo{ID: "ceph-pool", Type: "rbd", Shared: true}},
		{"zfs.local", `{"data":{"type":"zfspool","shared":0}}`, StorageInfo{ID: "zfs.local", Type: "zfspool"}},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			c, rec := newTestClient(t, map[string]reply{"/storage/" + tt.id: {http.StatusOK, tt.body}})
			got, err := c.Storage(context.Background(), tt.id)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, "/api2/json/storage/"+tt.id, rec.requests()[0].uri)
		})
	}
}

func TestStorageRejectsAnAnswerWithoutAType(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/storage/local": {http.StatusOK, `{"data":{"storage":"local"}}`}})
	_, err := c.Storage(context.Background(), "local")
	require.ErrorContains(t, err, "unexpected response")
}

func TestStorageRefusesAnIDThatIsNoPathSegment(t *testing.T) {
	c, rec := newTestClient(t, nil)
	for _, id := range []string{"", "..", "a/b", "nfs 1", "nfs?x=1"} {
		_, err := c.Storage(context.Background(), id)
		require.ErrorContains(t, err, "invalid storage")
	}
	require.Empty(t, rec.requests())
}

func TestStorageNotThereOrNotAllowed(t *testing.T) {
	c := newErrorClient(t, http.StatusInternalServerError, `{"data":null,"message":"storage 'gone' does not exist\n"}`)
	_, err := c.Storage(context.Background(), "gone")
	require.True(t, IsNotFound(err))

	c = newErrorClient(t, http.StatusForbidden, `{"data":null,"message":"Permission check failed (/storage/nfs1, Datastore.Audit)"}`)
	_, err = c.Storage(context.Background(), "nfs1")
	require.True(t, IsForbidden(err))
}
