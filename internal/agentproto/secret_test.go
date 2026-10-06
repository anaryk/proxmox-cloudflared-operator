package agentproto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// hidden holds a token where fmt cannot ask it how to print itself.
type hidden struct{ token Secret }

func requireRedacted(t *testing.T, out string) {
	t.Helper()
	for _, token := range []string{sharedToken, pinnedToken} {
		require.NotContains(t, out, token)
		require.NotContains(t, out, hex.EncodeToString([]byte(token)))
	}
}

func TestASecretPrintsRedactedHoweverItIsAsked(t *testing.T) {
	s := fullSnapshot()
	values := map[string]any{
		"secret":            NewSecret(sharedToken),
		"pointer to secret": new(NewSecret(sharedToken)),
		"tunnel":            s.Tunnels[0],
		"pointer to tunnel": &s.Tunnels[0],
		"snapshot":          s,
		"unexported field":  hidden{NewSecret(sharedToken)},
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3v"} {
			t.Run(name+" "+verb, func(t *testing.T) {
				requireRedacted(t, fmt.Sprintf(verb, v))
			})
		}
	}
	require.Contains(t, fmt.Sprintf("%v", s.Tunnels[0]), "[redacted]")
	require.Contains(t, fmt.Sprintf("%#v", s.Tunnels[0]), "[redacted]")
}

func TestASecretStaysOutOfErrorsAndLogs(t *testing.T) {
	s := fullSnapshot()
	tok := NewSecret(sharedToken)

	err := errors.New(fmt.Sprint("applying ", s.Tunnels[0]))
	requireRedacted(t, err.Error())
	err = fmt.Errorf("tunnel %v: %w", s.Tunnels[0], err)
	requireRedacted(t, err.Error())
	require.Contains(t, err.Error(), "[redacted]")

	var buf bytes.Buffer
	log := zerolog.New(&buf)
	log.Info().
		Interface("tunnel", s.Tunnels[0]).
		Any("snapshot", s).
		Stringer("token", tok).
		Fields(map[string]any{"field": tok}).
		Err(err).
		Msgf("applied %v", s.Tunnels)
	requireRedacted(t, buf.String())
	require.Contains(t, buf.String(), `"token":"[redacted]"`)
	require.Contains(t, buf.String(), `"field":"[redacted]"`)
}

func TestASecretEncodesRedacted(t *testing.T) {
	tok := NewSecret(sharedToken)
	b, err := json.Marshal(tok)
	require.NoError(t, err)
	require.JSONEq(t, `"[redacted]"`, string(b))
	text, err := tok.MarshalText()
	require.NoError(t, err)
	require.Equal(t, "[redacted]", string(text))
	require.Equal(t, "[redacted]", tok.String())
	require.Equal(t, "[redacted]", tok.GoString())
	for _, verb := range []string{"%v", "%#v", "%s", "%q", "%x", "%d", "%10.3v"} {
		require.Equal(t, "[redacted]", fmt.Sprintf(verb, tok), verb)
	}
}

func TestASecretReadsItsText(t *testing.T) {
	var s Secret
	require.NoError(t, json.Unmarshal([]byte(`"`+sharedToken+`"`), &s))
	require.Equal(t, sharedToken, s.Reveal())
	require.True(t, s.Equal(NewSecret(sharedToken)))
	require.False(t, s.Equal(NewSecret(pinnedToken)))

	for _, body := range []string{`""`, `null`} {
		var z Secret
		require.NoError(t, json.Unmarshal([]byte(body), &z))
		require.Equal(t, Secret{}, z)
	}

	require.Equal(t, Secret{}, NewSecret(""))
	require.Empty(t, Secret{}.Reveal())

	var r Secret
	require.ErrorContains(t, json.Unmarshal([]byte(`"[redacted]"`), &r), "redacted")
	require.Error(t, json.Unmarshal([]byte(`42`), &r))
}
