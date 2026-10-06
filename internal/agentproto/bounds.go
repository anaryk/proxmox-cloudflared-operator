package agentproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrTooLarge is a body over one of the bounds of the protocol. The
// controller answers a status that is with 413 and records an event.
var ErrTooLarge = errors.New("too large")

// readAtMost reads r to its end, or fails with ErrTooLarge once it passes
// limit bytes. It reads at most one byte beyond limit.
func readAtMost(r io.Reader, limit int, what string) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("reading the %s: %w", what, err)
	}
	if len(body) > limit {
		return nil, fmt.Errorf("the %s is %w: a body is at most %d bytes", what, ErrTooLarge, limit)
	}
	return body, nil
}

// DecodeSnapshot reads what EncodeSnapshot writes, tokens included. It
// refuses a body over MaxSnapshotBytes with ErrTooLarge, and anything after
// the snapshot. Fields it does not know are ignored.
func DecodeSnapshot(r io.Reader) (Snapshot, error) {
	body, err := readAtMost(r, MaxSnapshotBytes, "snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	var s Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return Snapshot{}, fmt.Errorf("decoding the snapshot: %w", err)
	}
	return s, nil
}

// DecodeStatus reads a status body. r is the body once any content encoding
// is undone, so a compressed body is held to what it expands to. It refuses a
// body over MaxStatusBytes, more than MaxEventBatch events, and a resend of
// more than MaxResend events with ErrTooLarge. Fields it does not know are
// ignored.
func DecodeStatus(r io.Reader) (Status, error) {
	body, err := readAtMost(r, MaxStatusBytes, "status")
	if err != nil {
		return Status{}, err
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
