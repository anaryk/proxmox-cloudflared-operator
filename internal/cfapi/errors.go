package cfapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Cloudflare DNS error codes that mean the record to create is already there:
// a CNAME or address record for that name, the same record again, and an
// identical record.
const (
	codeNameExists      = 81053
	codeRecordExists    = 81057
	codeIdenticalRecord = 81058
)

// errUnexpected marks an answer that cannot be trusted as an answer from the
// API: not an envelope, or one without the fields every envelope has.
var errUnexpected = errors.New("unexpected response")

// Error is a call the API refused: a non-2xx status, or an envelope whose
// success is false. A call the client held back because an earlier answer
// was a 429 is one too, with status 429. It never contains the API token.
type Error struct {
	Status     int
	Codes      []int         // every code in the envelope, in order
	Message    string        // the first message of the envelope without control characters, with the token blanked out and cut to 512 bytes, or the status text
	RetryAfter time.Duration // set for 429
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cloudflare api: HTTP %d: %s", e.Status, e.Message)
	if len(e.Codes) > 0 {
		codes := make([]string, len(e.Codes))
		for i, code := range e.Codes {
			codes[i] = strconv.Itoa(code)
		}
		b.WriteString(" (codes " + strings.Join(codes, ", ") + ")")
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, " (retry after %s)", e.RetryAfter)
	}
	return b.String()
}

// IsAuth reports whether err says the token is rejected or lacks permission.
func IsAuth(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

// IsNotFound reports whether err says the thing asked for does not exist.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// IsRateLimited reports whether err is a 429.
func IsRateLimited(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusTooManyRequests
}

// IsConflict reports whether err says what was to be created already exists.
func IsConflict(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusConflict ||
		slices.ContainsFunc(apiErr.Codes, func(code int) bool {
			return code == codeNameExists || code == codeRecordExists || code == codeIdenticalRecord
		})
}
