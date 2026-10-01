package cfapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJoinPath(t *testing.T) {
	tests := []struct {
		name     string
		segments []string
		want     string
		wantErr  string
	}{
		{"plain ids", []string{"zones", "023e105f4ecef8ad9ca31a8372d0c353", "dns_records"}, "/zones/023e105f4ecef8ad9ca31a8372d0c353/dns_records", ""},
		{"one segment", []string{"zones"}, "/zones", ""},
		{"every segment is escaped", []string{"zones", "a b", "100%", "q?x#y"}, "/zones/a%20b/100%25/q%3Fx%23y", ""},
		{"a percent sign is data, not an escape", []string{"zones", "%zz"}, "/zones/%25zz", ""},
		{"dots inside a name are fine", []string{"zones", "a..b", "...", ".hidden"}, "/zones/a..b/.../.hidden", ""},
		{"dot dot", []string{"zones", "z1", "dns_records", ".."}, "", `"." or ".."`},
		{"dot", []string{"zones", "."}, "", `"." or ".."`},
		{"empty segment", []string{"zones", "", "dns_records"}, "", "empty segment"},
		{"no segments", nil, "", "no segments"},
		{"slash inside a segment", []string{"zones", "a/b"}, "", "slash"},
		{"slash only", []string{"zones", "/"}, "", "slash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := joinPath(tt.segments...)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestJoinPathOutputIsAccepted(t *testing.T) {
	ids := []string{"a b", "100%", "%zz", "q?x#y", "ünï", `a\b`, "x;y", "a+b", "...", "a..b", "%2e%2e", "%2F"}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			path, err := joinPath("zones", id, "dns_records")
			require.NoError(t, err)
			require.NoError(t, checkPath(path))

			// The path goes out as built, and the server reads the id back.
			env := setup(t, reply(http.StatusOK, okBody(`{}`)))
			require.NoError(t, env.c.do(context.Background(), http.MethodGet, path, nil, nil, nil))
			require.Equal(t, "/client/v4"+path, env.requests()[0].uri)
		})
	}
}

func TestCheckPath(t *testing.T) {
	require.NoError(t, checkPath("/zones/z1/dns_records"))
	require.NoError(t, checkPath("zones/z1"))
	require.NoError(t, checkPath("/accounts/a1/cfd_tunnel/t1/configurations"))
	for _, path := range []string{"", "/", "/zones/../x", "/zones//x", "/zones/%zz", "/zones/%2e%2e", "/zones/a%2Fb"} {
		require.Error(t, checkPath(path), path)
	}
}
