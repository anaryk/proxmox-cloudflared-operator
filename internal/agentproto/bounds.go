package agentproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTooLarge is a status over one of the bounds an agent is held to. The
// controller answers it with 413 and records an event.
var ErrTooLarge = errors.New("too large")

// DecodeStatus reads a status body. r is the body once any content encoding
// is undone, so a compressed body is held to what it expands to; DecodeStatus
// reads at most one byte beyond MaxStatusBytes of it. It refuses a body over
// MaxStatusBytes, more than MaxEventBatch events, and a resend of more than
// MaxResend events with ErrTooLarge. Fields it does not know are ignored.
func DecodeStatus(r io.Reader) (Status, error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxStatusBytes+1))
	if err != nil {
		return Status{}, fmt.Errorf("reading the status: %w", err)
	}
	if len(body) > MaxStatusBytes {
		return Status{}, fmt.Errorf("the status is %w: a body is at most %d bytes", ErrTooLarge, MaxStatusBytes)
	}
	var st Status
	if err := json.Unmarshal(body, &st); err != nil {
		return Status{}, fmt.Errorf("decoding the status: %w", err)
	}
	switch {
	case len(st.Events) > MaxEventBatch:
		return Status{}, fmt.Errorf("the status is %w: %d events, at most %d go in one", ErrTooLarge, len(st.Events), MaxEventBatch)
	case st.Resend > MaxResend:
		return Status{}, fmt.Errorf("the status is %w: a resend of %d events, at most %d", ErrTooLarge, st.Resend, MaxResend)
	case st.Resend < 0:
		return Status{}, fmt.Errorf("decoding the status: a resend of %d events", st.Resend)
	}
	return st, nil
}
