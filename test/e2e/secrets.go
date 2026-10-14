//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// redactedValue stands for a secret in what the suite logs.
const redactedValue = "[redacted]"

// minSecret is the shortest value taken as a secret: a shorter one would
// match by chance in any output.
const minSecret = 12

// secrets are the tokens and keys a run handles: the Cloudflare tokens, the
// run tokens of the tunnels, the join tokens and the agents' keys. Whatever
// the run logs is scrubbed of them first, and a line that carried one fails
// the test, as the suite must not have printed it.
type secrets struct {
	mu     sync.Mutex
	values []string
}

// add records secret values, and for a value of several lines, as a key in
// PEM, each of its lines as well.
func (s *secrets) add(values ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		for _, part := range append([]string{v}, strings.Split(v, "\n")...) {
			part = strings.TrimSpace(part)
			if len(part) >= minSecret && !strings.HasPrefix(part, "-----") {
				s.values = append(s.values, part)
			}
		}
	}
}

// scrub returns text with every secret replaced.
func (s *secrets) scrub(text string) string {
	clean, _ := s.scrubbed(text)
	return clean
}

// scrubbed returns text with every secret replaced, and whether it had one.
func (s *secrets) scrubbed(text string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, v := range s.values {
		if strings.Contains(text, v) {
			text = strings.ReplaceAll(text, v, redactedValue)
			found = true
		}
	}
	return text, found
}

// logf logs a line of the run without any secret it may carry, and fails
// the test when it carried one.
func (s *secrets) logf(t testing.TB, format string, args ...any) {
	t.Helper()
	line, found := s.scrubbed(fmt.Sprintf(format, args...))
	t.Log(line)
	if found {
		t.Errorf("a line the suite was about to log carried a secret; it was logged with %s in its place", redactedValue)
	}
}

// runToken is the token a connector of the tunnel runs with now, recorded as
// a secret of the run.
func (c *clusterRun) runToken(t testing.TB, tunnelID string) string {
	t.Helper()
	token := c.cloud.RunToken(t, tunnelID)
	c.secret.add(token)
	return token
}

// logf logs a line of a run on a cluster, without its secrets.
func (c *clusterRun) logf(t testing.TB, format string, args ...any) {
	t.Helper()
	c.secret.logf(t, format, args...)
}
