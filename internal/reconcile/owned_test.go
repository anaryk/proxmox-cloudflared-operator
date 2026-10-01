package reconcile

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

func TestOwned(t *testing.T) {
	for comment, want := range map[string]bool{
		"pco:abc":            true,
		"pco:abc note":       true,
		"pco:abc\tnote":      true,
		"pco:abc probe":      true,
		"pco:abcd":           false,
		"pco:abcd note":      false,
		"pco:abc.old":        false,
		"PCO:ABC":            false,
		"notes pco:abc":      false,
		" pco:abc":           false,
		"":                   false,
		"pco:":               false,
		"pco:xyz pco:abc":    false,
		"pco:abc\u00a0note":  true, // a no-break space is a space
		"pco:abc\u200bhello": false,
	} {
		t.Run(comment, func(t *testing.T) {
			require.Equal(t, want, Owned("abc", cfapi.Record{Type: "CNAME", Name: "app.example.com", Comment: comment}))
		})
	}
}

func TestIsProbeRecord(t *testing.T) {
	probe := cfapi.Record{Type: "TXT", Name: "_pco-probe-r4nd.example.com", Content: "pco permission probe", Comment: "pco:abc probe"}
	cases := []struct {
		name   string
		change func(r *cfapi.Record)
		want   bool
	}{
		{"a probe", func(*cfapi.Record) {}, true},
		{"type in lower case", func(r *cfapi.Record) { r.Type = "txt" }, true},
		{"name in upper case", func(r *cfapi.Record) { r.Name = "_PCO-PROBE-R4ND.example.com" }, true},
		{"another type", func(r *cfapi.Record) { r.Type = "CNAME" }, false},
		{"another name", func(r *cfapi.Record) { r.Name = "app.example.com" }, false},
		{"a longer comment", func(r *cfapi.Record) { r.Comment = "pco:abc probe kept by hand" }, false},
		{"the marker alone", func(r *cfapi.Record) { r.Comment = "pco:abc" }, false},
		{"the probe of another install", func(r *cfapi.Record) { r.Comment = "pco:xyz probe" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := probe
			tc.change(&rec)
			require.Equal(t, tc.want, IsProbeRecord("abc", rec))
		})
	}
}
