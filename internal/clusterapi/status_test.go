package clusterapi

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
)

func statusOf(events int) agentproto.Status {
	st := agentproto.Status{
		Protocol: agentproto.Range{Min: 1, Max: 1}, Build: "dev", Node: testNode, Kind: pki.KindHost,
		EpochSeen: epoch1, SeqApplied: 3, Targets: []agentproto.TargetReport{},
	}
	for i := range events {
		st.Events = append(st.Events, json.RawMessage(`{"n":`+string(rune('0'+i%10))+`}`))
	}
	st.EventCursor = uint64(events)
	return st
}

func encodeStatus(t *testing.T, st agentproto.Status) []byte {
	t.Helper()
	body, err := json.Marshal(st)
	require.NoError(t, err)
	return body
}

func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(body)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

type eventSink struct {
	mu   sync.Mutex
	list []string
}

func (s *eventSink) add(node, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, node+": "+msg)
}

func (s *eventSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.list...)
}

func TestAStatusIsAnsweredWithTheHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.ctrl.setHeartbeat(agentproto.Heartbeat{Epoch: epoch1, LeaseValid: true, Seq: 3, ReadyReplicas: map[string]int{"t": 2}})
	c := h.client(h.ca, hostID(testNode))

	resp := h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(2)))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var hb agentproto.Heartbeat
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&hb))
	require.Equal(t, epoch1, hb.Epoch)
	require.True(t, hb.LeaseValid)
	require.Equal(t, uint64(2), hb.EventAck)
	require.Len(t, h.ctrl.reported(), 1)
	require.Equal(t, testNode, h.ctrl.reported()[0].Node)

	t.Run("compressed", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/status", gzipped(t, encodeStatus(t, statusOf(1))), "Content-Encoding", "gzip")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Len(t, h.ctrl.reported(), 2)
	})
	t.Run("another content encoding", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(1)), "Content-Encoding", "br")
		require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
	})
}

func TestAStatusOfAnotherNodeOrKindIsRefused(t *testing.T) {
	h := newHarness(t)
	c := h.client(h.ca, hostID(testNode))
	st := statusOf(0)
	st.Node = "pve3"
	resp := h.post(c, "/v1/agent/status", encodeStatus(t, st))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	st = statusOf(0)
	st.Kind = pki.KindAppliance
	resp = h.post(c, "/v1/agent/status", encodeStatus(t, st))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Empty(t, h.ctrl.reported())
}

func TestAStatusOverOneMiBIsRefusedWithAnEvent(t *testing.T) {
	h := newHarness(t)
	sink := h.events
	c := h.client(h.ca, hostID(testNode))

	st := statusOf(0)
	body := encodeStatus(t, st)
	pad := func(n int) []byte {
		// The problems field is filled up so that the body is exactly n bytes.
		st := st
		filler := n - len(body) - len(`,"problems":[""]`)
		st.Problems = []string{strings.Repeat("x", filler)}
		b := encodeStatus(t, st)
		require.Len(t, b, n)
		return b
	}
	resp := h.post(c, "/v1/agent/status", pad(agentproto.MaxStatusBytes))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = h.post(c, "/v1/agent/status", pad(agentproto.MaxStatusBytes+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	require.Len(t, sink.all(), 1)
	require.Contains(t, sink.all()[0], testNode+": ")
	require.Contains(t, sink.all()[0], "too large")

	t.Run("the bound holds after decompression", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/status", gzipped(t, pad(agentproto.MaxStatusBytes+1)), "Content-Encoding", "gzip")
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	})
	t.Run("too many events in one batch", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/status", encodeStatus(t, statusOf(agentproto.MaxEventBatch+1)))
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	})
	t.Run("a malformed body", func(t *testing.T) {
		resp := h.post(c, "/v1/agent/status", []byte("{"))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
	require.Len(t, h.ctrl.reported(), 1, "nothing refused reached the controller")
}

func TestAResendPastTenThousandEventsIsRefused(t *testing.T) {
	h := newHarness(t)
	sink := h.events
	c := h.client(h.ca, hostID(testNode))

	send := func(st agentproto.Status) int {
		t.Helper()
		resp := h.post(c, "/v1/agent/status", encodeStatus(t, st))
		return resp.StatusCode
	}
	// Twenty batches of 500 are the bound; the agent says the resend is
	// smaller than it is.
	for i := range 20 {
		st := statusOf(agentproto.MaxEventBatch)
		st.Resend = 100
		require.Equal(t, http.StatusOK, send(st), "batch %d", i)
	}
	st := statusOf(1)
	st.Resend = 100
	require.Equal(t, http.StatusRequestEntityTooLarge, send(st))
	require.Len(t, sink.all(), 1)
	require.Contains(t, sink.all()[0], "10001 events")

	t.Run("outside a resend the count stands", func(t *testing.T) {
		st := statusOf(1)
		require.Equal(t, http.StatusOK, send(st), "ordinary batches are not counted")
		st.Resend = 1
		require.Equal(t, http.StatusRequestEntityTooLarge, send(st), "the resend of this epoch is spent")
	})
	t.Run("a new epoch starts over", func(t *testing.T) {
		st := statusOf(1)
		st.EpochSeen = agentproto.Epoch{InstallID: testInstall, Generation: 2, Nonce: "n2"}
		st.Resend = 1
		require.Equal(t, http.StatusOK, send(st))
	})
}
