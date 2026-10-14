package agentproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
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

// addrs are n addresses of the pool of service addresses.
func addrs(n int) []netip.Addr {
	out := make([]netip.Addr, 0, n)
	for a := netip.MustParseAddr("198.18.1.0"); len(out) < n; a = a.Next() {
		out = append(out, a)
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
		{"4096 service addresses", bytes.NewReader(statusBody(t, Status{Node: "node2", ServiceAddrs: addrs(MaxServiceAddrs)})), false},
		{"4097 service addresses", bytes.NewReader(statusBody(t, Status{Node: "node2", ServiceAddrs: addrs(MaxServiceAddrs + 1)})), true},
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

func TestDecodeSnapshotBound(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, EncodeSnapshot(&buf, fullSnapshot()))
	body := buf.Bytes()

	s, err := DecodeSnapshot(padded(t, body, MaxSnapshotBytes))
	require.NoError(t, err, "a body of 8 MiB")
	require.Equal(t, fullSnapshot(), s)

	_, err = DecodeSnapshot(padded(t, body, MaxSnapshotBytes+1))
	require.ErrorIs(t, err, ErrTooLarge, "a body of 8 MiB and one byte")
	_, err = DecodeSnapshot(infinite{})
	require.ErrorIs(t, err, ErrTooLarge, "a body that never ends")
}

// The bound leaves room: the snapshot of a node that serves 2000 routes, each
// of its own guest with the whole proof, is far below it.
func TestALargeInstallFitsTheSnapshotBound(t *testing.T) {
	s := fullSnapshot()
	one := s.Targets[0]
	s.Targets = nil
	for i := range 2000 {
		target := one
		target.Key = fmt.Sprintf("app-%04d.example.com", i)
		target.Route.Hostname = target.Key
		s.Targets = append(s.Targets, target)
		s.Deny.Denied = append(s.Deny.Denied, netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}))
	}
	var buf bytes.Buffer
	require.NoError(t, EncodeSnapshot(&buf, s))
	require.Less(t, buf.Len(), MaxSnapshotBytes/2)
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
