package connector

import (
	"context"
	"strings"
)

// journalLines is how much of what a connector logged last is read. It spans
// a few of its restarts, which come every five seconds while it fails.
const journalLines = 50

// journalReader is a Systemd that can also read what a unit logged last.
type journalReader interface {
	Journal(ctx context.Context, unit string, lines int) ([]string, error)
}

// tokenRefused reads what a connector logged last, oldest first, and reports
// whether the last word on its connections is that Cloudflare refused its
// token: a registration refused as unauthorized, and none that worked since.
func tokenRefused(lines []string) bool {
	for i := len(lines) - 1; i >= 0; i-- {
		switch line := lines[i]; {
		case strings.Contains(line, "Registered tunnel connection"):
			return false
		case strings.Contains(line, "Unauthorized"):
			return true
		}
	}
	return false
}

// readJournal reads what the unit of a connector that is not ready logged
// last, and notes on st what it says. A journal that cannot be read says
// nothing.
func (m *Manager) readJournal(ctx context.Context, st *Status) {
	if m.journal == nil {
		return
	}
	lines, err := m.journal(ctx, UnitName(st.TunnelID), journalLines)
	if err != nil {
		m.log.Debug().Err(err).Str("tunnel", st.TunnelID).Msg("reading the journal of a connector failed")
		return
	}
	st.TokenRefused = tokenRefused(lines)
}
