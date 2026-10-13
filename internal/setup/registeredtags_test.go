package setup

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// pvesh prints registered-tags as one string before Proxmox VE 9.2 and as a
// JSON list from it on.
func TestRegisteredTagsReadsBothShapes(t *testing.T) {
	for _, tt := range []struct {
		name, out string
		want      []string
	}{
		{"string", `{"registered-tags":"cf-tunnel;cf-tunnel-managed"}`, []string{"cf-tunnel", "cf-tunnel-managed"}},
		{"list", `{"registered-tags":["cf-tunnel","cf-tunnel-managed"]}`, []string{"cf-tunnel", "cf-tunnel-managed"}},
		{"empty string", `{"registered-tags":""}`, nil},
		{"empty list", `{"registered-tags":[]}`, nil},
		{"list with an empty entry", `{"registered-tags":["cf-tunnel",""]}`, []string{"cf-tunnel"}},
		{"absent", `{"keyboard":"en-us"}`, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{t: t, events: new([]string), script: []call{{line: "pvesh get /cluster/options --output-format json", out: tt.out}}}

			got, err := RegisteredTags(context.Background(), r)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
