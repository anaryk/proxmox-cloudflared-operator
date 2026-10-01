package store

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const secretToken = "tok-Sup3rS3cret-value"

// requireNoSecretOnDisk fails when any file below dir holds secret.
func requireNoSecretOnDisk(t *testing.T, dir, secret string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NotContains(t, string(b), secret, path)
		return nil
	})
	require.NoError(t, err)
}

func credentialOf(id string) Credential {
	return Credential{ID: id, Label: "Shop account", Kind: "scoped", Token: NewSecret(secretToken), AddedAt: t0}
}

func TestCredentialsLiveOnThePrivateRootOnly(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Credentials()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	b, a := credentialOf("cred-b"), credentialOf("cred-a")
	b.Label = "Second"
	require.NoError(t, s.SaveCredential(b))
	require.NoError(t, s.SaveCredential(a))

	got, err = s.Credentials()
	require.NoError(t, err)
	require.Equal(t, []Credential{a, b}, got)
	require.Equal(t, secretToken, got[0].Token.Reveal())
	require.Equal(t, []string{"credentials/cred-a.json", "credentials/cred-b.json"}, stored(t, p.Private))
	require.Empty(t, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Local))
	requireNoSecretOnDisk(t, p.Cluster, secretToken)
	requireNoSecretOnDisk(t, p.Local, secretToken)

	require.NoError(t, s.SaveCredential(a))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Private, "credentials", "cred-a.json")))

	require.NoError(t, s.DeleteCredential("cred-a"))
	require.NoError(t, s.DeleteCredential("cred-a"))
	got, err = s.Credentials()
	require.NoError(t, err)
	require.Equal(t, []Credential{b}, got)
}

func TestSaveCredentialRefusesIncompleteCredentials(t *testing.T) {
	s, p := openStore(t)
	noID := credentialOf("")
	noToken := credentialOf("cred-a")
	noToken.Token = Secret{}
	badID := credentialOf("../x")
	for name, c := range map[string]Credential{"no id": noID, "no token": noToken, "unsafe id": badID} {
		err := s.SaveCredential(c)
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), secretToken, name)
	}
	require.Empty(t, stored(t, p.Private))
	require.Error(t, s.DeleteCredential(""))
}

func TestCredentialErrorsNeverCarryTheToken(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveCredential(credentialOf("cred-a")))
	path := filepath.Join(p.Private, "credentials", "cred-b.json")

	for name, body := range map[string]string{
		"syntax error after the token": envelopeJSON("cred-b", `{"id":"cred-b","token":"`+secretToken+`" x}`),
		"token of the wrong type":      envelopeJSON("cred-b", `{"id":"cred-b","token":["`+secretToken+`"]}`),
		"label of the wrong type":      envelopeJSON("cred-b", `{"id":"cred-b","label":123,"token":"`+secretToken+`"}`),
		"cut off":                      `{"schemaVersion":1,"rev":1,"id":"cred-b","data":{"id":"cred-b","token":"` + secretToken,
	} {
		writeFile(t, path, body)
		_, err := s.Credentials()
		require.Error(t, err, name)
		require.NotContains(t, err.Error(), secretToken, name)
		require.Contains(t, err.Error(), "cred-b.json", name)
	}
}

func TestSecret(t *testing.T) {
	require.Equal(t, secretToken, NewSecret(secretToken).Reveal())
	require.Empty(t, Secret{}.Reveal())
	require.Equal(t, Secret{}, NewSecret(""), "an empty secret is the zero secret")
	require.Equal(t, NewSecret("a"), NewSecret("a"))
	require.NotEqual(t, NewSecret("a"), NewSecret("b"))
	require.Equal(t, "[redacted]", NewSecret(secretToken).String())
	require.Equal(t, "[redacted]", Secret{}.String())
}

// hold keeps secrets in unexported fields, which fmt reads through reflection
// without asking them how to print themselves.
type hold struct {
	cred  Credential
	token PVEToken
	bare  Secret
}

type holdExported struct {
	Cred  Credential
	Token PVEToken
	Bare  Secret
}

func TestSecretsAreRedactedWhateverPrintsThem(t *testing.T) {
	c := credentialOf("cred-a")
	tok := PVEToken{TokenID: "pco@pve!pco", Secret: NewSecret(secretToken)}
	values := map[string]any{
		"secret":              c.Token,
		"credential":          c,
		"credential pointer":  &c,
		"credential slice":    []Credential{c},
		"credential array":    [1]Credential{c},
		"credential map":      map[string]Credential{"a": c},
		"pve token":           tok,
		"pve token pointer":   &tok,
		"unexported fields":   hold{cred: c, token: tok, bare: c.Token},
		"unexported pointer":  &hold{cred: c, token: tok, bare: c.Token},
		"exported fields":     holdExported{Cred: c, Token: tok, Bare: c.Token},
		"exported pointer":    &holdExported{Cred: c, Token: tok, Bare: c.Token},
		"nested unexported":   []hold{{cred: c, token: tok, bare: c.Token}},
		"interface in a map":  map[string]any{"c": c, "h": hold{cred: c}},
		"anonymous struct":    struct{ s Secret }{c.Token},
		"pointer to a secret": &c.Token,
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
			out := fmt.Sprintf(verb, v)
			require.NotContains(t, out, secretToken, "%s %s", name, verb)
			require.NotContains(t, out, hex.EncodeToString([]byte(secretToken)), "%s %s", name, verb)
		}
		require.NotContains(t, fmt.Sprint(v), secretToken, name)
		require.NotContains(t, fmt.Sprintln(v), secretToken, name)
	}
	require.Contains(t, fmt.Sprintf("%v", c), "cred-a")
	require.Contains(t, fmt.Sprintf("%v", tok), "pco@pve!pco")
}

func TestSecretsAreRedactedInJSON(t *testing.T) {
	c := credentialOf("cred-a")
	tok := PVEToken{TokenID: "pco@pve!pco", Secret: NewSecret(secretToken)}
	for name, v := range map[string]any{
		"secret": c.Token, "credential": c, "credential pointer": &c, "credentials": []Credential{c},
		"pve token": tok, "holder": holdExported{Cred: c, Token: tok, Bare: c.Token},
	} {
		b, err := json.Marshal(v)
		require.NoError(t, err, name)
		require.NotContains(t, string(b), secretToken, name)
		require.Contains(t, string(b), "[redacted]", name)
		b, err = json.MarshalIndent(v, "", "  ")
		require.NoError(t, err, name)
		require.NotContains(t, string(b), secretToken, name)
	}
}

func TestSecretsAreRedactedInLogs(t *testing.T) {
	c := credentialOf("cred-a")
	tok := PVEToken{TokenID: "pco@pve!pco", Secret: NewSecret(secretToken)}
	var buf bytes.Buffer
	log := zerolog.New(&buf)

	log.Info().Interface("credential", c).Interface("token", tok).Msg("as interfaces")
	log.Info().Any("credential", &c).Any("secret", c.Token).Msg("as any")
	log.Info().Stringer("credential", c).Stringer("token", tok).Msg("as stringers")
	log.Info().Str("credential", fmt.Sprint(c)).Str("secret", fmt.Sprintf("%v", c.Token)).Msg("as strings")
	log.Info().Interface("holder", hold{cred: c, token: tok, bare: c.Token}).Msg("unexported")
	log.Info().Err(fmt.Errorf("saving %v: %w", c, os.ErrPermission)).Msg("in an error")

	require.NotContains(t, buf.String(), secretToken)
	require.Contains(t, buf.String(), "cred-a")
}

func TestTheSecretIsOnDiskAndComesBack(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveCredential(credentialOf("cred-a")))
	require.NoError(t, s.SavePVEToken(PVEToken{TokenID: "pco@pve!pco", Secret: NewSecret(secretToken)}))

	for _, f := range []string{
		filepath.Join(p.Private, "credentials", "cred-a.json"),
		filepath.Join(p.Private, "meta", "pve-token.json"),
	} {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		require.Contains(t, string(b), secretToken, f)
		require.NotContains(t, string(b), "redacted", f)
	}
	creds, err := s.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1)
	require.Equal(t, secretToken, creds[0].Token.Reveal())
	tok, found, err := s.PVEToken()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, secretToken, tok.Secret.Reveal())
}

func TestPVETokenRoundTrip(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.PVEToken()
	require.NoError(t, err)
	require.False(t, found)

	want := PVEToken{TokenID: "pco@pve!pco", Secret: NewSecret("3f1c0a9e-1111-2222-3333-444455556666")}
	require.NoError(t, s.SavePVEToken(want))
	got, found, err := s.PVEToken()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)

	require.Equal(t, []string{"meta/pve-token.json"}, stored(t, p.Private))
	require.Empty(t, stored(t, p.Cluster))
	require.Empty(t, stored(t, p.Local))
	require.NoError(t, s.SavePVEToken(want))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Private, "meta", "pve-token.json")))
}

func TestSavePVETokenRefusesIncompleteTokens(t *testing.T) {
	s, p := openStore(t)
	require.Error(t, s.SavePVEToken(PVEToken{Secret: NewSecret("s")}))
	require.Error(t, s.SavePVEToken(PVEToken{TokenID: "pco@pve!pco"}))
	require.Empty(t, stored(t, p.Private))
}
