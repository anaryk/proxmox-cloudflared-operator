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

// whyNotReady reads what a connector logged last, oldest first, for the last
// word on why it is not connected: that Cloudflare refused its token, a
// registration refused as unauthorized, or that another process holds its
// metrics address, which it fails to listen on at its start. A connection
// registered since says neither.
func whyNotReady(lines []string, metricsAddr string) (refused, portHeld bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		switch line := lines[i]; {
		case strings.Contains(line, "Registered tunnel connection"):
			return false, false
		case strings.Contains(line, "Unauthorized"):
			return true, false
		case metricsAddr != "" && strings.Contains(line, "address already in use") && strings.Contains(line, metricsAddr):
			return false, true
		}
	}
	return false, false
}

// readJournal reads what the unit of a connector that is not ready logged
// last, and notes on st what it says. A metrics port that another process
// holds is not given out again. A journal that cannot be read says nothing.
func (m *Manager) readJournal(ctx context.Context, st *Status, port int) {
	if m.journal == nil {
		return
	}
	lines, err := m.journal(ctx, UnitName(st.TunnelID), journalLines)
	if err != nil {
		m.log.Debug().Err(err).Str("tunnel", st.TunnelID).Msg("reading the journal of a connector failed")
		return
	}
	st.TokenRefused, st.MetricsPortHeld = whyNotReady(lines, st.MetricsAddr)
	if st.MetricsPortHeld {
		m.mu.Lock()
		m.held[port] = true
		m.mu.Unlock()
	}
}
