package store

import (
	"fmt"
	"io"
)

const redacted = "[redacted]"

// Secret is a value that must not end up in a log or a reply: an API token. It
// prints and encodes as "[redacted]" whichever way it is asked, and Reveal
// returns the text for the one place that needs it.
//
// It holds the text behind a pointer, not as a string, on purpose. fmt reads a
// field that is not exported through reflection and never asks it how to print
// itself, so a string type, whatever its String method says, would show its
// text in the %v of a struct that holds it in such a field; a pointer shows an
// address. Make one with NewSecret; the zero Secret is the empty one.
type Secret struct{ p *string }

// NewSecret returns a Secret that holds s.
func NewSecret(s string) Secret {
	if s == "" {
		return Secret{}
	}
	return Secret{p: &s}
}

// Reveal returns the text of the secret.
func (s Secret) Reveal() string {
	if s.p == nil {
		return ""
	}
	return *s.p
}

// String returns "[redacted]".
func (Secret) String() string { return redacted }

// GoString returns "[redacted]".
func (Secret) GoString() string { return redacted }

// Format prints "[redacted]" for every verb.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON encodes "[redacted]". The store writes what it keeps through its
// own types, so that a secret in a file is never one a caller encoded.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
