package pki

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

var testSecret = []byte("s3cret-of-node-2")

func newJoinToken(t *testing.T, ca CA, now time.Time) JoinToken {
	t.Helper()
	tok, err := NewJoinToken(testInstall, "node2", []string{"192.0.2.10:8644", "[2001:db8::10]:8644"}, ca, now, bytes.NewReader(testSecret))
	require.NoError(t, err)
	return tok
}

func TestNewJoinToken(t *testing.T) {
	ca := newCA(t, t0)
	tok := newJoinToken(t, ca, t0)
	require.Equal(t, JoinToken{
		Version:    1,
		InstallID:  testInstall,
		Node:       "node2",
		Endpoints:  []string{"192.0.2.10:8644", "[2001:db8::10]:8644"},
		ServerName: "controller.pco-0123456789ab.invalid",
		CASHA256:   ca.SHA256(),
		Expires:    t0.Add(15 * time.Minute),
		secret:     new(string(testSecret)),
	}, tok)
}

func TestNewJoinTokenRefuses(t *testing.T) {
	ca := newCA(t, t0)
	endpoints := []string{"192.0.2.10:8644"}
	cases := []struct {
		name          string
		install, node string
		endpoints     []string
		ca            CA
		random        []byte
		wantSubstring string
	}{
		{"a bad install id", "x", "node2", endpoints, ca, testSecret, "install id"},
		{"a bad node name", testInstall, "node_2", endpoints, ca, testSecret, "node name"},
		{"no endpoint", testInstall, "node2", nil, ca, testSecret, "no controller endpoint"},
		{"an endpoint without a port", testInstall, "node2", []string{"192.0.2.10"}, ca, testSecret, "endpoint"},
		{"an endpoint with port 0", testInstall, "node2", []string{"192.0.2.10:0"}, ca, testSecret, "endpoint"},
		{"no CA", testInstall, "node2", endpoints, CA{}, testSecret, "no CA"},
		{"too few random bytes", testInstall, "node2", endpoints, ca, testSecret[:3], "random"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewJoinToken(c.install, c.node, c.endpoints, c.ca, t0, bytes.NewReader(c.random))
			require.ErrorContains(t, err, c.wantSubstring)
		})
	}
}

func TestJoinTokenRoundTrip(t *testing.T) {
	tok := newJoinToken(t, newCA(t, t0), t0)
	s := tok.Encode()
	require.True(t, strings.HasPrefix(s, "pco1."))
	require.NotContains(t, s, "=", "base64url without padding")

	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, "pco1."))
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(payload, &fields))
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	require.ElementsMatch(t, []string{"v", "install", "node", "endpoints", "serverName", "ca", "secret", "exp"}, keys)

	got, err := ParseJoinToken(s)
	require.NoError(t, err)
	require.Equal(t, tok, got)

	got, err = ParseJoinToken(s + "\n")
	require.NoError(t, err, "a token read from a file keeps its line end")
	require.Equal(t, tok, got)
}

func TestParseJoinTokenTakesAnExpiredOne(t *testing.T) {
	tok := newJoinToken(t, newCA(t, t0), t0.Add(-time.Hour))
	got, err := ParseJoinToken(tok.Encode())
	require.NoError(t, err)
	require.True(t, got.Expires.Before(t0), "the caller decides what an expired token means")
}

// tokenWith is the encoded test token with the fields of change set over it.
func tokenWith(t *testing.T, change map[string]any) string {
	t.Helper()
	var fields map[string]any
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(newJoinToken(t, newCA(t, t0), t0).Encode(), JoinPrefix))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(payload, &fields))
	for k, v := range change {
		if v == nil {
			delete(fields, k)
			continue
		}
		fields[k] = v
	}
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	return JoinPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func TestParseJoinTokenRefuses(t *testing.T) {
	good := newJoinToken(t, newCA(t, t0), t0).Encode()
	payload := strings.TrimPrefix(good, JoinPrefix)
	short := base64.StdEncoding.EncodeToString(testSecret[:15])

	cases := []struct {
		name          string
		token         string
		wantSubstring string
	}{
		{"another prefix", "pco2." + payload, "not a pco join token"},
		{"no prefix", payload, "not a pco join token"},
		{"empty", "", "not a pco join token"},
		{"bad base64", JoinPrefix + "!!!" + payload, "reading the join token"},
		{"padded base64", JoinPrefix + base64.URLEncoding.EncodeToString([]byte(`{"v":1}`)), "reading the join token"},
		{"not JSON", JoinPrefix + base64.RawURLEncoding.EncodeToString([]byte("nope")), "reading the join token"},
		{"version 2", tokenWith(t, map[string]any{"v": 2}), "version 2"},
		{"no version", tokenWith(t, map[string]any{"v": nil}), "version 0"},
		{"a bad install id", tokenWith(t, map[string]any{"install": "x"}), "install id"},
		{"a bad node name", tokenWith(t, map[string]any{"node": ""}), "node name"},
		{"no endpoint", tokenWith(t, map[string]any{"endpoints": []string{}}), "no controller endpoint"},
		{"a bad endpoint", tokenWith(t, map[string]any{"endpoints": []string{"controller"}}), "endpoint"},
		{"another server name", tokenWith(t, map[string]any{"serverName": "example.org"}), "server name"},
		{"a CA hash that is not one", tokenWith(t, map[string]any{"ca": "abc"}), "CA"},
		{"a CA hash in upper case", tokenWith(t, map[string]any{"ca": strings.Repeat("AB", 32)}), "CA"},
		{"a short secret", tokenWith(t, map[string]any{"secret": short}), "secret"},
		{"no secret", tokenWith(t, map[string]any{"secret": nil}), "secret"},
		{"no expiry", tokenWith(t, map[string]any{"exp": nil}), "expiry"},
		{"too long", JoinPrefix + strings.Repeat("A", 8<<10), "longer than"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseJoinToken(c.token)
			require.ErrorContains(t, err, c.wantSubstring)
			require.NotContains(t, err.Error(), base64.StdEncoding.EncodeToString(testSecret))
		})
	}
}

// heldToken holds a token where fmt cannot ask it how to print itself.
type heldToken struct{ token JoinToken }

func TestJoinTokenNeverPrintsItsSecret(t *testing.T) {
	tok := newJoinToken(t, newCA(t, t0), t0)
	held := heldToken{tok}
	exported := struct{ Token JoinToken }{tok}
	forms := []string{
		string(testSecret),
		hex.EncodeToString(testSecret),
		strings.ToUpper(hex.EncodeToString(testSecret)),
		base64.StdEncoding.EncodeToString(testSecret),
		base64.RawURLEncoding.EncodeToString(testSecret),
		fmt.Sprintf("%d", testSecret),
		fmt.Sprintf("%#v", testSecret),
		fmt.Sprintf("%v", [16]byte(testSecret)),
	}

	printed := map[string]string{
		"String":   tok.String(),
		"an error": fmt.Errorf("joining with %v", tok).Error(),
	}
	values := map[string]any{
		"the token":                    tok,
		"a pointer to it":              &tok,
		"an exported field":            exported,
		"an unexported field":          held,
		"a pointer to the field owner": &held,
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
			printed[name+" "+verb] = fmt.Sprintf(verb, v)
		}
		b, err := json.Marshal(v)
		require.NoError(t, err)
		printed[name+" JSON"] = string(b)
	}
	var buf bytes.Buffer
	log := zerolog.New(&buf)
	log.Info().
		Interface("token", tok).
		Any("held", held).
		Any("exported", exported).
		Msgf("joined with %v and %+v", tok, held)
	printed["zerolog"] = buf.String()

	for how, out := range printed {
		t.Run(how, func(t *testing.T) {
			for _, f := range forms {
				require.NotContains(t, out, f)
			}
		})
	}
	require.Contains(t, tok.String(), "node2")
	require.Contains(t, tok.String(), testInstall)
	require.Contains(t, printed["the token JSON"], `"node":"node2"`)
	require.NotContains(t, printed["the token JSON"], "secret")
}

func TestJoinTokenSecretHash(t *testing.T) {
	tok := newJoinToken(t, newCA(t, t0), t0)
	sum := sha256.Sum256(testSecret)
	require.Equal(t, hex.EncodeToString(sum[:]), tok.SecretHash())

	other := tok
	other.secret = new("another secret!!")
	require.NotEqual(t, tok.SecretHash(), other.SecretHash())

	require.Empty(t, JoinToken{}.SecretHash())
}

func TestJoinTokenMatches(t *testing.T) {
	tok := newJoinToken(t, newCA(t, t0), t0)
	stored := tok.SecretHash()
	parsed, err := ParseJoinToken(tok.Encode())
	require.NoError(t, err)

	require.True(t, tok.Matches(stored))
	require.True(t, parsed.Matches(stored), "the token the agent sends matches what the controller stored")
	require.False(t, tok.Matches(strings.ToUpper(stored)))
	require.False(t, tok.Matches(stored[:63]))
	require.False(t, tok.Matches(""))
	another := sha256.Sum256([]byte("another secret"))
	require.False(t, tok.Matches(hex.EncodeToString(another[:])))

	var none JoinToken
	require.False(t, none.Matches(""), "a token without a secret matches nothing")
	require.False(t, none.Matches(stored))
	require.Empty(t, none.Encode())
}
