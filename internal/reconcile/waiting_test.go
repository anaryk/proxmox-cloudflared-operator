package reconcile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWhatWaitsForTheRateLimitIsOneLine(t *testing.T) {
	tests := []struct {
		name string
		w    Waiting
		line string
	}{
		{"nothing", Waiting{}, ""},
		{"a change", Waiting{Changes: 1}, "1 change waits for Cloudflare's rate limit"},
		{"changes", Waiting{Changes: 23}, "23 changes wait for Cloudflare's rate limit"},
		{"a read", Waiting{Reads: []string{"the listing of zone example.com"}}, "the listing of zone example.com waits for Cloudflare's rate limit"},
		{"two reads", Waiting{Reads: []string{"the tunnel of account acc1", "the listing of zone example.com"}},
			"the tunnel of account acc1 and the listing of zone example.com wait for Cloudflare's rate limit"},
		{"reads and changes", Waiting{Changes: 2, Reads: []string{"the tunnel of account acc1", "the listing of zone example.com"}},
			"the tunnel of account acc1, the listing of zone example.com and 2 changes wait for Cloudflare's rate limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.line, tt.w.Line())
		})
	}
}

func TestWhatWaitsAddsUp(t *testing.T) {
	w := Waiting{Reads: []string{"the tunnel of account acc1"}}

	w.Add(Waiting{Changes: 2, Reads: []string{"the listing of zone example.com"}})
	w.Add(Waiting{Changes: 1})

	require.Equal(t, Waiting{Changes: 3, Reads: []string{"the tunnel of account acc1", "the listing of zone example.com"}}, w)
}
