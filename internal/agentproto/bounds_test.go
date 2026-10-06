package agentproto

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func statusBody(t *testing.T, st Status) []byte {
	t.Helper()
	b, err := json.Marshal(st)
	require.NoError(t, err)
	return b
}

// padded is body with spaces after it up to size bytes.
func padded(t *testing.T, body []byte, size int) io.Reader {
	t.Helper()
	require.LessOrEqual(t, len(body), size)
	return io.MultiReader(bytes.NewReader(body), strings.NewReader(strings.Repeat(" ", size-len(body))))
}

func events(n int) []json.RawMessage {
	out := make([]json.RawMessage, n)
	for i := range out {
		out[i] = json.RawMessage(`{"kind":"route"}`)
	}
	return out
}

func TestDecodeStatusBounds(t *testing.T) {
	body := statusBody(t, fullStatus())
	cases := []struct {
		name     string
		r        io.Reader
		tooLarge bool
	}{
		{"a body of 1 MiB", padded(t, body, MaxStatusBytes), false},
		{"a body of 1 MiB and one byte", padded(t, body, MaxStatusBytes+1), true},
		{"a body that never ends", infinite{}, true},
		{"500 events", bytes.NewReader(statusBody(t, Status{Node: "node2", Events: events(MaxEventBatch)})), false},
		{"501 events", bytes.NewReader(statusBody(t, Status{Node: "node2", Events: events(MaxEventBatch + 1)})), true},
		{"a resend of 10 000 events", bytes.NewReader(statusBody(t, Status{Node: "node2", Resend: MaxResend})), false},
		{"a resend of 10 001 events", bytes.NewReader(statusBody(t, Status{Node: "node2", Resend: MaxResend + 1})), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DecodeStatus(c.r)
			if !c.tooLarge {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrTooLarge)
		})
	}
}

func TestDecodeStatusRefusesWhatIsNoStatus(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":                `status`,
		"two values":              `{"node":"node2"} {"node":"node3"}`,
		"a negative resend":       `{"node":"node2","resend":-1}`,
		"nothing":                 ``,
		"a list":                  `[]`,
		"a field of a wrong type": `{"seqApplied":"many"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeStatus(strings.NewReader(body))
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrTooLarge)
		})
	}
}

// infinite is a body that sends spaces for ever.
type infinite struct{}

func (infinite) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}
