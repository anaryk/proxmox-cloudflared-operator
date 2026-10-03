package cfapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsUnanswered(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"a transport failure", transportError{errors.New("connection reset")}, true},
		{"a server error", &Error{Status: http.StatusServiceUnavailable}, true},
		{"a rate limit", &Error{Status: http.StatusTooManyRequests}, true},
		{"a cancelled call", context.Canceled, true},
		{"a call out of time", context.DeadlineExceeded, true},
		{"wrapped", fmt.Errorf("listing zones: %w", &Error{Status: http.StatusInternalServerError}), true},
		{"a refusal", &Error{Status: http.StatusForbidden}, false},
		{"a missing thing", &Error{Status: http.StatusNotFound}, false},
		{"an error of the caller", errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsUnanswered(tt.err))
		})
	}
}
