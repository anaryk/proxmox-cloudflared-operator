package clusterapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

func TestTheLongPollReturnsAtOnceOnANewSeq(t *testing.T) {
	h := newHarness(t, time.Hour)
	c := h.client(h.ca, hostID(testNode))
	h.ctrl.publish(snapshotOf(3))

	resp := h.get(c, "/v1/agent/desired?epoch=1.n1&after=2")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	s, err := agentproto.DecodeSnapshot(resp.Body)
	require.NoError(t, err)
	require.Equal(t, uint64(3), s.Seq)
	require.Equal(t, "run-token-of-the-shared-tunnel", s.Tunnels[0].Token.Reveal(), "the wire form carries the token")

	t.Run("published while the poll waits", func(t *testing.T) {
		got := make(chan *answer, 1)
		go func() { got <- h.get(c, "/v1/agent/desired?epoch=1.n1&after=3") }()
		<-h.ctrl.polled
		h.ctrl.publish(snapshotOf(4))
		select {
		case resp := <-got:
			require.Equal(t, http.StatusOK, resp.StatusCode)
			s, err := agentproto.DecodeSnapshot(resp.Body)
			require.NoError(t, err)
			require.Equal(t, uint64(4), s.Seq)
		case <-time.After(10 * time.Second):
			t.Fatal("the poll did not return on the new seq")
		}
	})
	t.Run("an epoch the controller does not serve is answered at once", func(t *testing.T) {
		resp := h.get(c, "/v1/agent/desired?epoch=1.old&after=99")
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
	t.Run("no epoch yet", func(t *testing.T) {
		resp := h.get(c, "/v1/agent/desired")
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestTheLongPollEndsEmptyAfterItsTime(t *testing.T) {
	h := newHarness(t)
	h.ctrl.publish(snapshotOf(3))
	resp := h.get(h.client(h.ca, hostID(testNode)), "/v1/agent/desired?epoch=1.n1&after=3")
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestABadQueryOfTheLongPollIsRefused(t *testing.T) {
	h := newHarness(t)
	c := h.client(h.ca, hostID(testNode))
	for _, q := range []string{"?after=x", "?after=-1", "?epoch=1", "?epoch=0.n1&after=1"} {
		t.Run(q, func(t *testing.T) {
			resp := h.get(c, "/v1/agent/desired"+q)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestASnapshotOfAnotherNodeIsNeverServed(t *testing.T) {
	h := newHarness(t)
	h.ctrl.renderFor("pve9")
	h.ctrl.publish(snapshotOf(1))
	resp := h.get(h.client(h.ca, hostID(testNode)), "/v1/agent/desired")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}
