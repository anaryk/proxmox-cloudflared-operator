package credentials

import "errors"

// The length of a Cloudflare API token. Real ones are 40 characters; the
// bounds keep junk away from Cloudflare and keep the scrubbing of messages
// from ever working on a one-letter secret.
const (
	MinTokenLength = 20
	MaxTokenLength = 256
)

// ErrBadToken is the one answer to a token of the wrong shape, wherever a
// token enters: setup, credential add, the appliance's bootstrap.
var ErrBadToken = errors.New("the token is not a Cloudflare API token: it has 20 to 256 characters, all of A-Z a-z 0-9 _ -")

// ValidToken tells whether token has the shape of a Cloudflare API token:
// MinTokenLength to MaxTokenLength characters of A-Z, a-z, 0-9, _ and -.
func ValidToken(token string) bool {
	if len(token) < MinTokenLength || len(token) > MaxTokenLength {
		return false
	}
	for i := range len(token) {
		switch b := token[i]; {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_', b == '-':
		default:
			return false
		}
	}
	return true
}

// CheckToken is ValidToken as an error: ErrBadToken for the wrong shape.
func CheckToken(token string) error {
	if !ValidToken(token) {
		return ErrBadToken
	}
	return nil
}
