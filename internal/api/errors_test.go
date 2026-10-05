package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every error answer is an ErrorBody, which clients take their type from.
func TestAnErrorAnswerIsAnErrorBody(t *testing.T) {
	rec := do(newServer(&fakeEngine{}), http.MethodGet, "/v1/nowhere", "")

	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	var body ErrorBody
	require.NoError(t, dec.Decode(&body))
	require.Equal(t, ErrorBody{Error: "no such route", Code: codeNoRoute}, body)
}

func TestRedact(t *testing.T) {
	const secret = "abcdef0123456789ABCD"
	for _, tt := range []struct {
		name    string
		msg     string
		secrets []string
		want    string
	}{
		{"plain", "bad token " + secret + " given", []string{secret}, "bad token [redacted] given"},
		{"every occurrence", secret + " and " + secret, []string{secret}, "[redacted] and [redacted]"},
		{"the trimmed form of a padded secret", "bad token " + secret, []string{"  " + secret + "\n"}, "bad token [redacted]"},
		{"the padded form", "bad token \t" + secret + "\n!", []string{"\t" + secret + "\n"}, "bad token [redacted]!"},
		{"the quoted form", fmt.Sprintf("cannot use %q", "  "+secret+"\n"), []string{"  " + secret + "\n"}, `cannot use "[redacted]"`},
		{"the quoted form of an odd secret", fmt.Sprintf("cannot use %q", `a"b\c`+secret), []string{`a"b\c` + secret}, `cannot use "[redacted]"`},
		{"the longer secret first", secret + "xyz", []string{secret, secret + "xyz"}, "[redacted]"},
		{"no secrets", "nothing to see", nil, "nothing to see"},
		{"an empty secret is no secret", "nothing to see", []string{"", "  "}, "nothing to see"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, redact(tt.msg, tt.secrets))
		})
	}
}
