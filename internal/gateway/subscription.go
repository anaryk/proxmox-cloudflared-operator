package gateway

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// SubscriptionErrors takes what the netlink library hands the error callback
// of a subscription. The library calls it for a message it skips and goes
// on receiving: one from a sender other than the kernel ("Wrong sender
// portid"), an interrupted dump, an error message, a link it cannot read.
// Only a receive that fails ends the subscription, and the library closes
// its channel right after. So no error ends a watch by itself; the last one
// says why the channel closed.
type SubscriptionErrors struct {
	// skipped, when set, is told of the first message skipped.
	skipped func(err error)

	mu    sync.Mutex
	last  error
	count int
}

// NewSubscriptionErrors returns the errors of one subscription; skipped may
// be nil.
func NewSubscriptionErrors(skipped func(err error)) *SubscriptionErrors {
	return &SubscriptionErrors{skipped: skipped}
}

// receiveFailed starts the error the library reports before it ends a
// subscription.
const receiveFailed = "Receive failed"

// Report is the subscription's error callback.
func (s *SubscriptionErrors) Report(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.last = err
	first := false
	if !strings.HasPrefix(err.Error(), receiveFailed) {
		s.count++
		first = s.count == 1
	}
	s.mu.Unlock()
	if first && s.skipped != nil {
		s.skipped(err)
	}
}

// Skipped returns how many messages the subscription skipped.
func (s *SubscriptionErrors) Skipped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

// Ended is the error of a watch whose channel closed.
func (s *SubscriptionErrors) Ended(what string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return errors.New(what + ": the subscription ended")
	}
	return fmt.Errorf("%s: the subscription ended: %w", what, s.last)
}
