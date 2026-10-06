package agentproto

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const redacted = "[redacted]"

// Secret is a run token on its way to an agent. It prints and encodes as
// "[redacted]" whichever way it is asked, so that a snapshot can go into a log
// or an error; EncodeSnapshot writes its text for the wire, and Reveal returns
// it to the one place that needs it.
//
// It holds the text behind a pointer, not as a string, on purpose: fmt prints
// a field that is not exported through reflection, without asking it how to
// print itself, and a string would show its text there. So == compares the
// pointers; compare two secrets with Equal. The zero Secret is the empty one.
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

// Equal reports whether two secrets hold the same text, in a time that does
// not depend on where they differ.
func (s Secret) Equal(o Secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.Reveal()), []byte(o.Reveal())) == 1
}

// String returns "[redacted]".
func (Secret) String() string { return redacted }

// GoString returns "[redacted]".
func (Secret) GoString() string { return redacted }

// Format prints "[redacted]" for every verb, so that no verb falls back to the
// text.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalText returns "[redacted]".
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// MarshalJSON encodes "[redacted]", which is what a logger that encodes its
// fields as JSON writes too.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// UnmarshalJSON reads the text of the secret. It refuses "[redacted]": what
// was encoded for a log is no token to run a connector with.
func (s *Secret) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("reading a secret: %w", err)
	}
	if text == redacted {
		return errors.New("the secret is redacted: it was encoded for a log, not with EncodeSnapshot")
	}
	*s = NewSecret(text)
	return nil
}
