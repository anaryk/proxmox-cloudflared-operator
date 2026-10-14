package credentials

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckTokenTakesTheShapeOfACloudflareToken(t *testing.T) {
	for _, tt := range []struct {
		name  string
		token string
		ok    bool
	}{
		{"a real token", "v1_0Km3Np7Qs9Tu1Vw3Xy5Za7Bc9De1Fg3Hi5Jk7L", true},
		{"the shortest", strings.Repeat("a", MinTokenLength), true},
		{"the longest", strings.Repeat("a", MaxTokenLength), true},
		{"underscore and dash", "abc_DEF-ghi_JKL-mno_PQR-0123", true},
		{"one short of the shortest", strings.Repeat("a", MinTokenLength-1), false},
		{"one over the longest", strings.Repeat("a", MaxTokenLength+1), false},
		{"a short token", "short-token", false},
		{"empty", "", false},
		{"a space inside", "abcdefghij klmnopqrstuvwxyz", false},
		{"a dot", "abcdefghij.klmnopqrstuvwxyz", false},
		{"a newline at the end", "abcdefghijklmnopqrstuvwxyz\n", false},
		{"a letter outside ASCII", "abcdefghijklmnopqrstuvwxyzé", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.ok, ValidToken(tt.token))
			err := CheckToken(tt.token)
			if tt.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrBadToken)
			require.EqualError(t, err, "the token is not a Cloudflare API token: it has 20 to 256 characters, all of A-Z a-z 0-9 _ -")
		})
	}
}
