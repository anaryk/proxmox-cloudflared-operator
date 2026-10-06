package upgrade

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func env(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestTheOverridesTakeOnlyThisMachine(t *testing.T) {
	for _, tt := range []struct{ base, want string }{
		{"http://127.0.0.1:8788", "http://127.0.0.1:8788"},
		{"http://127.0.0.1:8788/", "http://127.0.0.1:8788"},
		{"http://127.0.0.1:8788/mirror/", "http://127.0.0.1:8788/mirror"},
		{"https://127.0.0.1:8443", "https://127.0.0.1:8443"},
		{"http://127.9.8.7", "http://127.9.8.7"},
		{"http://[::1]:8788", "http://[::1]:8788"},
	} {
		t.Run(tt.base, func(t *testing.T) {
			o, err := OverridesFrom(env(map[string]string{"PCO_UPGRADE_BASE": tt.base}))
			require.NoError(t, err)
			require.Equal(t, Overrides{Base: tt.want}, o)
			require.True(t, o.Set())
		})
	}

	for _, base := range []string{
		"http://10.0.0.1",
		"http://10.0.0.1:8788",
		"https://example.org",
		"https://github.com",
		"http://localhost:8788",
		"http://127.0.0.1.nip.io:8788",
		"http://0.0.0.0:8788",
		"http://[::ffff:10.0.0.1]:8788",
		"ftp://127.0.0.1",
		"127.0.0.1:8788",
		"http://user:secret@127.0.0.1:8788",
		"http://127.0.0.1:8788/?a=b",
		"http://127.0.0.1:8788/#top",
	} {
		t.Run(base, func(t *testing.T) {
			_, err := OverridesFrom(env(map[string]string{"PCO_UPGRADE_BASE": base}))
			require.EqualError(t, err, "PCO_UPGRADE_BASE must be an http or https URL of a loopback address, "+
				"such as http://127.0.0.1:8788, without credentials, query or fragment; it is for tests only")
		})
	}
}

func TestTheKeyringOverrideIsAnAbsolutePath(t *testing.T) {
	o, err := OverridesFrom(env(map[string]string{"PCO_UPGRADE_KEYRING": "/root/test.gpg"}))
	require.NoError(t, err)
	require.Equal(t, Overrides{Keyring: "/root/test.gpg"}, o)
	require.True(t, o.Set())

	_, err = OverridesFrom(env(map[string]string{"PCO_UPGRADE_KEYRING": "test.gpg"}))
	require.EqualError(t, err, "PCO_UPGRADE_KEYRING must be the absolute path of a keyring file; it is for tests only")
}

func TestNoOverrideIsNoOverride(t *testing.T) {
	o, err := OverridesFrom(env(nil))
	require.NoError(t, err)
	require.False(t, o.Set())
	require.False(t, Overridden(env(nil)))
}

// Whoever reads the line learns that a variable is set, valid or not.
func TestEitherVariableIsAnOverride(t *testing.T) {
	require.True(t, Overridden(env(map[string]string{"PCO_UPGRADE_BASE": "http://10.0.0.1"})))
	require.True(t, Overridden(env(map[string]string{"PCO_UPGRADE_KEYRING": "/k.gpg"})))
	require.Equal(t, "the release host or key is overridden (PCO_UPGRADE_BASE, PCO_UPGRADE_KEYRING); this is for tests only", OverrideLine)
}
