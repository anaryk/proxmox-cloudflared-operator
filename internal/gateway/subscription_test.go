package gateway

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// A message from a sender other than the kernel is skipped by the library,
// which goes on receiving; the watch goes on too, and says so once.
func TestASkippedMessageEndsNoSubscription(t *testing.T) {
	var told []error
	s := NewSubscriptionErrors(func(err error) { told = append(told, err) })
	for range 3 {
		s.Report(errors.New("Wrong sender portid 4242, expected 0"))
	}
	s.Report(nil)
	require.Len(t, told, 1, "told once")
	require.Equal(t, 3, s.Skipped())
}

func TestTheLastErrorSaysWhyASubscriptionEnded(t *testing.T) {
	s := NewSubscriptionErrors(nil)
	require.EqualError(t, s.Ended("watching the links"), "watching the links: the subscription ended")

	s.Report(errors.New("Wrong sender portid 4242, expected 0"))
	s.Report(errors.New("Receive failed: no buffer space available"))
	require.Equal(t, 1, s.Skipped(), "a failed receive is no message skipped")
	require.EqualError(t, s.Ended("watching the links"), "watching the links: the subscription ended: Receive failed: no buffer space available")
}
