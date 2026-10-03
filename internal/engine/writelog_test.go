package engine

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A change at Cloudflare is one event of the engine, which is one line of the
// log at info level; the reconcilers that made it keep their own line for a
// debugging run.
func TestEveryWriteIsOneInfoLineThatIsItsEvent(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	e.log = zerolog.New(&buf)
	e.eng = e.newEngine()
	e.enforce()

	e.cycle()

	require.NotEmpty(t, e.tunnels())
	require.NotEmpty(t, e.records())
	info := map[string]int{}
	for line := range strings.Lines(buf.String()) {
		var entry struct{ Level, Message string }
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		if entry.Level == "info" {
			info[entry.Message]++
		}
	}
	var actions []string
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindAction {
			actions = append(actions, ev.Message)
		}
	}
	require.GreaterOrEqual(t, len(actions), 3, "the tunnel, its configuration and the record")
	for _, msg := range actions {
		require.Equal(t, 1, info[msg], msg)
	}
	for _, own := range []string{"created tunnel", "wrote tunnel configuration", "changed dns record"} {
		require.Zero(t, info[own], own)
	}
}
