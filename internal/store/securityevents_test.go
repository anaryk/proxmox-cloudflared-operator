package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func securityEvent(at time.Time, msg string) SecurityEvent {
	return SecurityEvent{At: at, Kind: "enrol", Subject: "pve-b", Message: msg, Actor: "root@pam"}
}

func TestSecurityEventsAreKeptPerNode(t *testing.T) {
	s, p := openStore(t)
	got, err := s.SecurityEvents()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	require.NoError(t, s.AppendSecurityEvent("pve-a", securityEvent(t0, "join token issued")))
	require.NoError(t, s.AppendSecurityEvent("pve-b-2", securityEvent(t0.Add(time.Second), "join token used")))
	require.NoError(t, s.AppendSecurityEvent("pve-a", securityEvent(t0.Add(2*time.Second), "node admitted")))

	got, err = s.SecurityEvents()
	require.NoError(t, err)
	a0, b1, a2 := securityEvent(t0, "join token issued"), securityEvent(t0.Add(time.Second), "join token used"),
		securityEvent(t0.Add(2*time.Second), "node admitted")
	a0.Node, b1.Node, a2.Node = "pve-a", "pve-b-2", "pve-a"
	require.Equal(t, []SecurityEvent{a0, b1, a2}, got, "every node's, merged by time")
	require.Equal(t, []string{"security-events/pve-a.json", "security-events/pve-b-2.json"}, stored(t, p.Cluster),
		"one file per node, which only that node writes")
}

func TestASecurityEventOfAnotherNodeIsRefused(t *testing.T) {
	s, p := openStore(t)
	e := securityEvent(t0, "x")
	e.Node = "pve-b"

	require.ErrorContains(t, s.AppendSecurityEvent("pve-a", e), "pve-b")
	require.ErrorContains(t, s.AppendSecurityEvent("pve-a", SecurityEvent{At: t0, Message: "no kind"}), "kind")
	require.ErrorContains(t, s.AppendSecurityEvent("pve-a", SecurityEvent{Kind: "enrol", Message: "no time"}), "time")
	require.ErrorContains(t, s.AppendSecurityEvent("", securityEvent(t0, "no node")), "node")
	require.Empty(t, stored(t, p.Cluster))
}

func TestAFileOfSecurityEventsKeepsTheLast500(t *testing.T) {
	s, _ := openStore(t)
	full := securityLog{Node: "pve1"}
	for i := range MaxSecurityEvents {
		e := securityEvent(t0.Add(time.Duration(i)*time.Second), fmt.Sprintf("event %d", i))
		e.Node = "pve1"
		full.Events = append(full.Events, e)
	}
	require.NoError(t, s.cluster.put(kindSecurityEvents, "pve1", full, true))
	for i := MaxSecurityEvents; i < MaxSecurityEvents+20; i++ {
		require.NoError(t, s.AppendSecurityEvent("pve1", securityEvent(t0.Add(time.Duration(i)*time.Second), fmt.Sprintf("event %d", i))))
	}

	got, err := s.SecurityEvents()

	require.NoError(t, err)
	require.Len(t, got, MaxSecurityEvents)
	require.Equal(t, "event 20", got[0].Message, "the oldest go first")
	require.Equal(t, fmt.Sprintf("event %d", MaxSecurityEvents+19), got[len(got)-1].Message)
}

// Long messages would take the file over the limit of the cluster
// filesystem before 500 of them: the oldest give way.
func TestAFileOfSecurityEventsStaysUnderTheLimitOfAFile(t *testing.T) {
	s, _ := openStore(t)
	long := strings.Repeat("x", 100_000)
	for i := range 20 {
		require.NoError(t, s.AppendSecurityEvent("pve1", securityEvent(t0.Add(time.Duration(i)*time.Second), fmt.Sprintf("%d %s", i, long))))
	}

	got, err := s.SecurityEvents()

	require.NoError(t, err)
	require.Less(t, len(got), 20)
	require.Greater(t, len(got), 5)
	require.True(t, strings.HasPrefix(got[len(got)-1].Message, "19 "), "the newest is kept")

	require.ErrorContains(t, s.AppendSecurityEvent("pve1", securityEvent(t0, strings.Repeat("y", maxFileSize))), "limit")
}
