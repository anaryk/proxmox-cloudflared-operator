package bundle

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const passphrase = "correct horse battery"

// full is a bundle of every part.
func full() Contents {
	return Contents{
		InstallID:    "0123456789ab",
		InstalledAt:  t0.Add(-72 * time.Hour),
		Settings:     json.RawMessage(`{"gateTag":"cf-tunnel","observeOnly":false}`),
		ManualRoutes: []json.RawMessage{json.RawMessage(`{"hostname":"a.example.com","manualId":"a"}`)},
		Approvals:    []json.RawMessage{json.RawMessage(`{"owner":"qemu/101","identity":"x"}`)},
		Segments:     []json.RawMessage{json.RawMessage(`{"bridge":"vmbr0"}`)},
		NodeSegments: []json.RawMessage{json.RawMessage(`{"node":"pve2","bridge":"vmbr0"}`)},
		Nodes:        []json.RawMessage{json.RawMessage(`{"name":"pve1"}`), json.RawMessage(`{"name":"pve2"}`)},
		CA: &CAFiles{
			CertPEM: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
			KeyPEM:  store.NewSecret("-----BEGIN PRIVATE KEY-----\nBBBB\n-----END PRIVATE KEY-----\n"),
		},
		PreviousCA: &CAFiles{
			CertPEM: "-----BEGIN CERTIFICATE-----\nCCCC\n-----END CERTIFICATE-----\n",
			KeyPEM:  store.NewSecret("-----BEGIN PRIVATE KEY-----\nDDDD\n-----END PRIVATE KEY-----\n"),
		},
		CARotatedAt:      t0.Add(-time.Hour),
		CARotation:       json.RawMessage(`{"node":"pve3","startedAt":"2026-10-06T10:00:00Z","done":["credentials"]}`),
		Observe:          json.RawMessage(`{"since":"2026-10-06T09:00:00Z"}`),
		Cluster:          json.RawMessage(`{"caSHA256":"aaaa","name":"lab"}`),
		Credentials:      []json.RawMessage{json.RawMessage(`{"id":"main","token":"cf-secret-token"}`)},
		Leader:           json.RawMessage(`{"installId":"0123456789ab","generation":4,"nonce":"abcd"}`),
		Networks:         []json.RawMessage{json.RawMessage(`{"id":"vmbr0","mark":1}`)},
		ServiceAddrs:     []json.RawMessage{json.RawMessage(`{"network":"vmbr0","addr":"192.0.2.20","service":"198.18.1.0"}`)},
		NodeServiceAddrs: []json.RawMessage{json.RawMessage(`{"network":"vmbr9","node":"pve2","addr":"10.30.0.5","service":"198.18.1.1"}`)},
		ServicePoolNext:  netip.MustParseAddr("198.18.1.2"),
		Leases:           []json.RawMessage{json.RawMessage(`{"mac":"06:50:43:4f:76:0a","addr":"10.203.118.10"}`)},
		Consents:         []json.RawMessage{json.RawMessage(`{"owner":"qemu/101"}`)},
		Attachments:      []json.RawMessage{json.RawMessage(`{"owner":"qemu/101","nic":1}`)},
		Claims:           []json.RawMessage{json.RawMessage(`{"hostname":"a.example.com","owner":"qemu/101"}`)},
		Tombstones:       json.RawMessage(`{"zone/old.example.com":{"generation":3,"nonce":"abcd"}}`),
		Placements:       []json.RawMessage{json.RawMessage(`{"hostname":"a.example.com","tunnel":"t1","kind":"shared"}`)},
		Moves:            []json.RawMessage{json.RawMessage(`{"owner":"qemu/101","from":"t1","to":"t2","step":"rules-added"}`)},
		MadeAt:           t0,
		Build:            "0.2.0",
	}
}

func requireSameContents(t *testing.T, want, got Contents) {
	t.Helper()
	require.Equal(t, want.InstallID, got.InstallID)
	require.True(t, want.InstalledAt.Equal(got.InstalledAt))
	require.True(t, want.MadeAt.Equal(got.MadeAt))
	require.Equal(t, want.Build, got.Build)
	require.Equal(t, want.CA, got.CA)
	require.Equal(t, want.PreviousCA, got.PreviousCA)
	require.True(t, want.CARotatedAt.Equal(got.CARotatedAt))
	require.Equal(t, want.ServicePoolNext, got.ServicePoolNext)
	for name, pair := range map[string][2]json.RawMessage{
		"settings":    {want.Settings, got.Settings},
		"leader":      {want.Leader, got.Leader},
		"cluster":     {want.Cluster, got.Cluster},
		"CA rotation": {want.CARotation, got.CARotation},
		"observe":     {want.Observe, got.Observe},
		"tombstones":  {want.Tombstones, got.Tombstones},
	} {
		if len(pair[0]) == 0 {
			require.Empty(t, pair[1], name)
			continue
		}
		require.JSONEq(t, string(pair[0]), string(pair[1]), name)
	}
	for name, pair := range map[string][2][]json.RawMessage{
		"manual routes":          {want.ManualRoutes, got.ManualRoutes},
		"approvals":              {want.Approvals, got.Approvals},
		"segments":               {want.Segments, got.Segments},
		"node segments":          {want.NodeSegments, got.NodeSegments},
		"nodes":                  {want.Nodes, got.Nodes},
		"credentials":            {want.Credentials, got.Credentials},
		"networks":               {want.Networks, got.Networks},
		"service addresses":      {want.ServiceAddrs, got.ServiceAddrs},
		"node service addresses": {want.NodeServiceAddrs, got.NodeServiceAddrs},
		"leases":                 {want.Leases, got.Leases},
		"consents":               {want.Consents, got.Consents},
		"attachments":            {want.Attachments, got.Attachments},
		"claims":                 {want.Claims, got.Claims},
		"placements":             {want.Placements, got.Placements},
		"moves":                  {want.Moves, got.Moves},
	} {
		require.Len(t, pair[1], len(pair[0]), name)
		for i := range pair[0] {
			require.JSONEq(t, string(pair[0][i]), string(pair[1][i]), name)
		}
	}
}

func seal(t *testing.T, c Contents) []byte {
	t.Helper()
	b, err := Seal(c, []byte(passphrase), rand.Reader)
	require.NoError(t, err)
	return b
}

func TestABundleOpensWithItsPassphrase(t *testing.T) {
	want := full()

	got, err := Open(seal(t, want), []byte(passphrase))

	require.NoError(t, err)
	requireSameContents(t, want, got)
}

func TestABundleWithOnlyTheInstallOpens(t *testing.T) {
	want := Contents{InstallID: "0123456789ab", MadeAt: t0, Build: "dev"}

	got, err := Open(seal(t, want), []byte(passphrase))

	require.NoError(t, err)
	require.Equal(t, "0123456789ab", got.InstallID)
	require.Nil(t, got.CA)
	require.Empty(t, got.Credentials)
	require.Empty(t, got.Leader)
	require.Empty(t, got.Nodes)
}

func TestAWrongPassphraseOpensNothing(t *testing.T) {
	b := seal(t, full())

	for _, wrong := range []string{"correct horse batterz", "", "short", strings.ToUpper(passphrase)} {
		got, err := Open(b, []byte(wrong))

		require.ErrorIs(t, err, ErrWrongPassphrase)
		require.Zero(t, got)
		require.NotContains(t, err.Error(), passphrase)
	}
}

func TestATruncatedBundleIsRefused(t *testing.T) {
	b := seal(t, full())

	for _, n := range []int{0, 1, len(magic), headerLen - 1, headerLen, headerLen + tagLen - 1} {
		_, err := Open(b[:n], []byte(passphrase))
		require.ErrorIs(t, err, ErrTruncated, "%d bytes", n)
	}
	for _, n := range []int{headerLen + tagLen, headerLen + 40, len(b) - 1} {
		_, err := Open(b[:n], []byte(passphrase))
		require.ErrorIs(t, err, ErrWrongPassphrase, "cut to %d of %d bytes", n, len(b))
	}
}

func TestABundleThatWasChangedIsRefused(t *testing.T) {
	b := seal(t, full())

	// Any byte of the header (the key derives from some, the rest is
	// authenticated with the text) or of the text.
	for _, at := range []int{headerLen - 1, headerLen - nonceLen, headerLen - nonceLen - 1, headerLen - nonceLen - saltLen, headerLen, len(b) - 1, len(b) - tagLen} {
		changed := bytes.Clone(b)
		changed[at] ^= 0x01
		_, err := Open(changed, []byte(passphrase))
		require.ErrorIs(t, err, ErrWrongPassphrase, "byte %d", at)
	}
	longer := append(bytes.Clone(b), 0)
	_, err := Open(longer, []byte(passphrase))
	require.ErrorIs(t, err, ErrWrongPassphrase)
}

func TestNothingThatIsNoBundleIsOpened(t *testing.T) {
	b := seal(t, full())
	for name, text := range map[string][]byte{
		"a text":        bytes.Repeat([]byte("not a bundle "), 20),
		"another magic": append([]byte("PCOBUNDX"), b[len(magic):]...),
	} {
		_, err := Open(text, []byte(passphrase))
		require.ErrorIs(t, err, ErrNotBundle, name)
	}
}

func TestAPassphraseUnderSixteenCharactersIsRefused(t *testing.T) {
	for _, short := range []string{"", "a", "fifteen chars!!", strings.Repeat("ě", 15)} {
		b, err := Seal(full(), []byte(short), rand.Reader)

		require.ErrorIs(t, err, ErrWeakPassphrase, "%q", short)
		require.Nil(t, b)
	}
	for _, enough := range []string{"sixteen chars!!!", strings.Repeat("ě", 16)} {
		_, err := Seal(full(), []byte(enough), rand.Reader)
		require.NoError(t, err, "%q", enough)
	}
	require.Equal(t, 16, MinPassphrase)
}

func TestTheErrorOfAWeakPassphraseDoesNotRepeatIt(t *testing.T) {
	err := CheckPassphrase([]byte("tiny-secret-pw"))

	require.ErrorIs(t, err, ErrWeakPassphrase)
	require.NotContains(t, err.Error(), "tiny-secret-pw")
}

func TestTheErrorOfAWeakPassphraseSaysHowManyCharactersAreNeeded(t *testing.T) {
	err := CheckPassphrase([]byte("tiny"))

	require.EqualError(t, err, "the passphrase is too short: it has 4 characters, and at least 16 are needed")
}

// The header is the format: its layout, and the parameters of the key
// derivation, are what a later build reads the bundle by.
func TestTheHeaderCarriesTheParametersOfTheKeyDerivation(t *testing.T) {
	salt := bytes.Repeat([]byte{0xA1}, 16)
	nonce := bytes.Repeat([]byte{0xB2}, 12)

	b, err := Seal(full(), []byte(passphrase), bytes.NewReader(append(bytes.Clone(salt), nonce...)))

	require.NoError(t, err)
	want := []byte("PCOBUNDL")
	want = append(want, 1, 1)                           // format 1, Argon2id
	want = binary.BigEndian.AppendUint32(want, 3)       // passes
	want = binary.BigEndian.AppendUint32(want, 64*1024) // 64 MiB, in KiB
	want = append(want, 4)                              // lanes
	want = append(want, salt...)
	want = append(want, nonce...)
	require.Equal(t, want, b[:headerLen])
	require.Equal(t, 8+2+4+4+1+16+12, headerLen)
}

func TestTheTextIsAES256GCMUnderAnArgon2idKey(t *testing.T) {
	b := seal(t, full())
	header := b[:headerLen]
	salt := header[headerLen-nonceLen-saltLen : headerLen-nonceLen]
	nonce := header[headerLen-nonceLen:]

	key := argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, 32)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	text, err := gcm.Open(nil, nonce, b[headerLen:], header)

	require.NoError(t, err, "a reader that knows only the format opens it")
	_, err = gcm.Open(nil, nonce, b[headerLen:], nil)
	require.Error(t, err, "the header is authenticated with the text")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(text, &doc))
	require.Equal(t, "0123456789ab", doc["installId"])
	require.Len(t, key, 32, "AES-256")
}

func TestAHeaderThatAsksForOtherParametersIsRefusedBeforeAnyKeyIsDerived(t *testing.T) {
	b := seal(t, full())
	timeAt := len(magic) + 2
	memoryAt := timeAt + 4
	threadsAt := memoryAt + 4

	for name, change := range map[string]func([]byte){
		"more memory than a node can spare": func(h []byte) { binary.BigEndian.PutUint32(h[memoryAt:], 1<<22) },
		"less memory":                       func(h []byte) { binary.BigEndian.PutUint32(h[memoryAt:], 8) },
		"more passes":                       func(h []byte) { binary.BigEndian.PutUint32(h[timeAt:], 1000) },
		"fewer passes":                      func(h []byte) { binary.BigEndian.PutUint32(h[timeAt:], 1) },
		"more lanes":                        func(h []byte) { h[threadsAt] = 64 },
		"another derivation":                func(h []byte) { h[len(magic)+1] = 2 },
	} {
		changed := bytes.Clone(b)
		change(changed[:headerLen])

		start := time.Now()
		_, err := Open(changed, []byte(passphrase))

		require.ErrorIs(t, err, ErrUnsupported, name)
		require.Less(t, time.Since(start), time.Second, "%s: refused before the key is derived", name)
	}
}

func TestABundleOfAnotherFormatSaysWhoCanReadIt(t *testing.T) {
	b := seal(t, full())
	b[len(magic)] = 2

	_, err := Open(b, []byte(passphrase))

	require.ErrorIs(t, err, ErrUnsupported)
	require.ErrorContains(t, err, "format 2")
	require.ErrorContains(t, err, "newer")
}

func TestABundleOfAFormatNoPcoWroteIsOfNoKnownFormat(t *testing.T) {
	b := seal(t, full())
	b[len(magic)] = 0

	_, err := Open(b, []byte(passphrase))

	require.ErrorIs(t, err, ErrUnsupported)
	require.ErrorContains(t, err, "not a bundle of a known format")
	require.NotContains(t, err.Error(), "newer")
}

func TestTwoBundlesOfOneInstallShareNeitherSaltNorText(t *testing.T) {
	a, b := seal(t, full()), seal(t, full())

	require.NotEqual(t, a[headerLen-nonceLen-saltLen:headerLen], b[headerLen-nonceLen-saltLen:headerLen])
	require.NotEqual(t, a[headerLen:], b[headerLen:])
}

func TestTheBundleHoldsNoPlainText(t *testing.T) {
	b := seal(t, full())

	for _, secret := range []string{"cf-secret-token", "BBBB", "0123456789ab", "pve1", passphrase} {
		require.NotContains(t, string(b), secret)
	}
}

func TestSealNeedsAnInstallAndRandomness(t *testing.T) {
	_, err := Seal(Contents{MadeAt: t0}, []byte(passphrase), rand.Reader)
	require.ErrorContains(t, err, "no install")

	_, err = Seal(full(), []byte(passphrase), iotest.ErrReader(errors.New("no entropy")))
	require.ErrorContains(t, err, "no entropy")

	_, err = Seal(full(), []byte(passphrase), bytes.NewReader(make([]byte, 20)))
	require.Error(t, err, "a reader that gives less than the salt and the nonce")
}

func TestABundleWithAFieldThisBuildDoesNotKnowIsRefused(t *testing.T) {
	// Authentic, and made by something that knows more: restoring it would
	// leave out what it carries.
	b := sealText(t, []byte(`{"installId":"0123456789ab","madeAt":"2026-10-06T12:00:00Z","somethingNew":[1]}`))

	_, err := Open(b, []byte(passphrase))

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrWrongPassphrase)
	require.ErrorContains(t, err, "somethingNew")
}

func TestABundleWithTextAfterItsObjectIsRefused(t *testing.T) {
	b := sealText(t, []byte(`{"installId":"0123456789ab","madeAt":"2026-10-06T12:00:00Z"}{}`))

	_, err := Open(b, []byte(passphrase))

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrWrongPassphrase)
}

func TestABundleOfNoInstallIsRefused(t *testing.T) {
	b := sealText(t, []byte(`{"madeAt":"2026-10-06T12:00:00Z"}`))

	_, err := Open(b, []byte(passphrase))

	require.ErrorContains(t, err, "no install")
}

// sealText seals text as it is, as a bundle of another build would.
func sealText(t *testing.T, text []byte) []byte {
	t.Helper()
	b, err := sealBytes(text, []byte(passphrase), rand.Reader)
	require.NoError(t, err)
	return b
}

func TestWhatABundleHoldsDoesNotPrint(t *testing.T) {
	c := full()

	for _, text := range []string{
		fmt.Sprint(c), fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), fmt.Sprintf("%s", c),
		fmt.Sprint(&c), fmt.Sprintf("%+v", *c.CA), fmt.Sprintf("%#v", c.CA), fmt.Sprintf("%v", []Contents{c}),
		fmt.Sprintf("%v", struct{ Held Contents }{c}),
	} {
		require.NotContains(t, text, "BBBB", "the key of the CA")
		require.NotContains(t, text, "cf-secret-token")
		require.NotContains(t, text, "123 34", "bytes of a credential")
	}
	text, err := json.Marshal(c)
	require.NoError(t, err)
	require.NotContains(t, string(text), "BBBB")
	require.NotContains(t, string(text), "cf-secret-token")
}

func TestAGeneratedPassphraseIs24CharactersOfAKnownAlphabet(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		p, err := Generate(rand.Reader)

		require.NoError(t, err)
		require.Len(t, p, 24)
		require.NoError(t, CheckPassphrase([]byte(p)))
		for _, r := range p {
			require.Contains(t, passphraseAlphabet, string(r))
		}
		seen[p] = true
	}
	require.Len(t, seen, 20)
	require.NotContains(t, passphraseAlphabet, "l", "the letters that read as digits are left out")
	require.NotContains(t, passphraseAlphabet, "o")
	require.NotContains(t, passphraseAlphabet, "0")
	require.NotContains(t, passphraseAlphabet, "1")
	require.Len(t, passphraseAlphabet, 32, "every byte of randomness picks a character without a bias")
}

func TestAGeneratedPassphraseFollowsTheRandomness(t *testing.T) {
	p, err := Generate(bytes.NewReader(make([]byte, 24)))
	require.NoError(t, err)
	require.Equal(t, strings.Repeat(string(passphraseAlphabet[0]), 24), p)

	_, err = Generate(bytes.NewReader(make([]byte, 23)))
	require.Error(t, err)

	_, err = Generate(iotest.ErrReader(errors.New("no entropy")))
	require.ErrorContains(t, err, "no entropy")
}

func TestAPassphraseIsCountedInCharactersNotBytes(t *testing.T) {
	require.NoError(t, CheckPassphrase([]byte(strings.Repeat("ž", 16))))
	require.Error(t, CheckPassphrase([]byte(strings.Repeat("ž", 15))))
}
