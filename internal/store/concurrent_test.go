package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	s, _ := openStore(t)
	done := make(chan error, 8)
	for i := range 4 {
		go func() {
			done <- s.SaveNode(NodeEntry{Name: "pve1", Version: fmt.Sprint(i), Since: t0})
		}()
		go func() {
			done <- s.AppendAdopted(t0.Add(time.Duration(i)*time.Second), "zone1", adoptedSample(i))
		}()
	}
	for range 8 {
		require.NoError(t, <-done)
	}
	nodes, err := s.Nodes()
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Len(t, adoptedLines(t, s), 4)
}
