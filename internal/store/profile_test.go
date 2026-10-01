package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func installFile(p Paths) string { return filepath.Join(p.Cluster, "meta", "install.json") }

func TestInstallProfileRoundTrip(t *testing.T) {
	s, p := openStore(t)
	want := Install{ID: "0123456789ab", CreatedAt: t0, Profile: ProfileAppliance}
	require.NoError(t, s.SaveInstall(want))

	got, found, err := s.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)
	require.Equal(t, ProfileAppliance, got.ProfileName())

	raw, err := os.ReadFile(installFile(p))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"profile": "appliance"`)
}

func TestInstallWithoutProfileIsTheHostProfile(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0}))

	raw, err := os.ReadFile(installFile(p))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "profile", "an empty profile is not written")

	got, _, err := s.Install()
	require.NoError(t, err)
	require.Empty(t, got.Profile)
	require.Equal(t, ProfileHost, got.ProfileName())
}

func TestInstallWrittenBeforeProfilesExistReadsAsHost(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, installFile(p),
		`{"schemaVersion":1,"rev":1,"id":"install","data":{"id":"0123456789ab","createdAt":"2026-10-01T12:00:00Z"}}`)

	got, found, err := s.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "0123456789ab", got.ID)
	require.Equal(t, ProfileHost, got.ProfileName())
}

func TestSaveInstallRefusesAnUnknownProfile(t *testing.T) {
	s, p := openStore(t)
	err := s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0, Profile: "container"})
	require.ErrorContains(t, err, `"container"`)
	require.Empty(t, stored(t, p.Cluster))

	for _, profile := range []string{"", ProfileHost, ProfileAppliance} {
		require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0, Profile: profile}), "profile %q", profile)
	}
}
