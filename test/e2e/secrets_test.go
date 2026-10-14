//go:build e2e

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// recorder is a testing.TB that keeps what is logged and whether the test
// failed, so that the scrubbing of the suite's own logs is checked without a
// node.
type recorder struct {
	testing.TB
	logged []string
	failed bool
}

func (r *recorder) Helper() {}

func (r *recorder) Log(args ...any) {
	for _, a := range args {
		r.logged = append(r.logged, a.(string))
	}
}

func (r *recorder) Errorf(string, ...any) { r.failed = true }

func TestSecretsStayOutOfLogs(t *testing.T) {
	const key = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIBxJ7m0aVYk9qP3sZQ1x2Lw4T9n8u5yRcT0lQfG2hJpKoAoGCCqGSM49\n-----END EC PRIVATE KEY-----\n"
	var s secrets
	s.add(fakeToken, "pco1.eyJ2IjoxLCJzIjoiYWJjZGVmZ2hpamtsbW5vcCJ9", key, "short")

	cases := []struct {
		name, line string
		leaks      bool
	}{
		{"a Cloudflare token", "pco credential add read " + fakeToken + " from the file", true},
		{"a join token", "the token: pco1.eyJ2IjoxLCJzIjoiYWJjZGVmZ2hpamtsbW5vcCJ9", true},
		{"a line of a key", "MHcCAQEEIBxJ7m0aVYk9qP3sZQ1x2Lw4T9n8u5yRcT0lQfG2hJpKoAoGCCqGSM49", true},
		{"nothing secret", "pco setup: store: created install 3f2a9c1b0d4e", false},
		{"a value too short to be one", "a short line", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{TB: t}
			s.logf(r, "%s", tc.line)
			require.Len(t, r.logged, 1)
			require.Equal(t, tc.leaks, r.failed, "the line %q", r.logged[0])
			require.NotContains(t, r.logged[0], fakeToken)
			require.NotContains(t, r.logged[0], "MHcCAQEEIBxJ7m0aVYk9qP3sZQ1x2Lw4T9n8u5yRcT0lQfG2hJpKoAoGCCqGSM49")
			require.NotContains(t, r.logged[0], "pco1.eyJ2")
		})
	}
}
