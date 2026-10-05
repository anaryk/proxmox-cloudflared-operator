package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func installFile(p Paths) string { return filepath.Join(p.Cluster, "meta", "install.json") }

func applianceBlock() *ApplianceInstall {
	return &ApplianceInstall{
		VMID:      9200,
		Node:      "pve1",
		MACs:      []string{"bc:24:11:00:aa:b5", "bc:24:11:00:aa:b6"},
		Endpoints: []Endpoint{{Address: "10.92.0.1:8006", ServerName: "pve1"}},
		CAFile:    "/var/lib/pco/pve-ca.pem",
	}
}

func applianceInstall() Install {
	return Install{ID: "0123456789ab", CreatedAt: t0, Profile: ProfileAppliance, Appliance: applianceBlock()}
}

func TestInstallProfileRoundTrip(t *testing.T) {
	s, p := openStore(t)
	want := applianceInstall()
	require.NoError(t, s.SaveInstall(want))

	got, found, err := s.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)
	require.Equal(t, ProfileAppliance, got.ProfileName())

	raw, err := os.ReadFile(installFile(p))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"profile": "appliance"`)
	require.Contains(t, string(raw), `"serverName": "pve1"`)
}

func TestInstallWithoutProfileIsTheHostProfile(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0}))

	raw, err := os.ReadFile(installFile(p))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "profile", "an empty profile is not written")
	require.NotContains(t, string(raw), "appliance")

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

	for _, profile := range []string{"", ProfileHost} {
		require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0, Profile: profile}), "profile %q", profile)
	}
	require.NoError(t, s.SaveInstall(applianceInstall()))
}

func TestSaveInstallChecksTheApplianceBlock(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(i *Install)
		want   string
	}{
		{"an appliance without its block", func(i *Install) { i.Appliance = nil }, "records nothing about the appliance"},
		{"a block on the host profile", func(i *Install) { i.Profile = ProfileHost }, "only an appliance"},
		{"a block without a profile", func(i *Install) { i.Profile = "" }, "only an appliance"},
		{"no vmid", func(i *Install) { i.Appliance.VMID = 0 }, "vmid 0"},
		{"a negative vmid", func(i *Install) { i.Appliance.VMID = -1 }, "vmid -1"},
		{"no node", func(i *Install) { i.Appliance.Node = "" }, "node is empty"},
		{"no MAC", func(i *Install) { i.Appliance.MACs = nil }, "no MAC"},
		{"a MAC in capitals", func(i *Install) { i.Appliance.MACs = []string{"BC:24:11:00:AA:B5"} }, `"BC:24:11:00:AA:B5"`},
		{"a MAC with dashes", func(i *Install) { i.Appliance.MACs = []string{"bc-24-11-00-aa-b5"} }, `"bc-24-11-00-aa-b5"`},
		{"something else than a MAC", func(i *Install) { i.Appliance.MACs = []string{"eth0"} }, `"eth0"`},
		{"no endpoint", func(i *Install) { i.Appliance.Endpoints = nil }, "no endpoint"},
		{"an address without a port", func(i *Install) { i.Appliance.Endpoints[0].Address = "10.92.0.1" }, `"10.92.0.1"`},
		{"an address without a host", func(i *Install) { i.Appliance.Endpoints[0].Address = ":8006" }, `":8006"`},
		{"a port that is not a number", func(i *Install) { i.Appliance.Endpoints[0].Address = "pve1:https" }, `"pve1:https"`},
		{"port 0", func(i *Install) { i.Appliance.Endpoints[0].Address = "pve1:0" }, `"pve1:0"`},
		{"a port out of range", func(i *Install) { i.Appliance.Endpoints[0].Address = "pve1:65536" }, `"pve1:65536"`},
		{"no server name", func(i *Install) { i.Appliance.Endpoints[0].ServerName = "" }, "no server name"},
		{
			"a second endpoint without a server name",
			func(i *Install) {
				i.Appliance.Endpoints = append(i.Appliance.Endpoints, Endpoint{Address: "10.92.1.1:8006"})
			},
			"no server name",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			i := applianceInstall()
			tt.change(&i)

			err := s.SaveInstall(i)

			require.ErrorContains(t, err, tt.want)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

func TestTheApplianceBlockKeepsItsMACsSorted(t *testing.T) {
	s, _ := openStore(t)
	i := applianceInstall()
	i.Appliance.MACs = []string{"bc:24:11:00:aa:b6", "bc:24:11:00:aa:b5", "bc:24:11:00:aa:b6"}
	i.Appliance.Endpoints = append(i.Appliance.Endpoints, Endpoint{Address: "[fd00::1]:8006", ServerName: "pve1.example.com"})

	require.NoError(t, s.SaveInstall(i))

	got, _, err := s.Install()
	require.NoError(t, err)
	require.Equal(t, []string{"bc:24:11:00:aa:b5", "bc:24:11:00:aa:b6"}, got.Appliance.MACs)
	require.Equal(t, []string{"bc:24:11:00:aa:b6", "bc:24:11:00:aa:b5", "bc:24:11:00:aa:b6"}, i.Appliance.MACs,
		"the caller's install is not changed")
	require.Equal(t, i.Appliance.Endpoints, got.Appliance.Endpoints, "the first endpoint stays the first")
}

func TestDetectProfile(t *testing.T) {
	for _, tt := range []struct {
		name, content, want string
	}{
		{"host", "host\n", ProfileHost},
		{"appliance", "appliance\n", ProfileAppliance},
		{"appliance without a newline", "appliance", ProfileAppliance},
		{"appliance with white space around it", "  appliance \r\n\n", ProfileAppliance},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile")
			writeFile(t, path, tt.content)

			got, err := DetectProfile(path)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("missing", func(t *testing.T) {
		got, err := DetectProfile(filepath.Join(t.TempDir(), "profile"))
		require.NoError(t, err)
		require.Equal(t, ProfileHost, got)
	})

	for _, tt := range []struct {
		name, content, want string
	}{
		{"junk", "container\n", `"container"`},
		{"two words", "appliance host\n", `"appliance host"`},
		{"capitals", "Appliance\n", `"Appliance"`},
		{"empty", "", "empty"},
		{"white space only", " \n", "empty"},
		{"a long file", string(make([]byte, 4096)), "longer than"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile")
			writeFile(t, path, tt.content)

			got, err := DetectProfile(path)

			require.ErrorContains(t, err, tt.want)
			require.ErrorContains(t, err, path)
			require.Empty(t, got)
		})
	}

	t.Run("unreadable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "profile")
		require.NoError(t, os.Mkdir(path, 0o700))

		got, err := DetectProfile(path)

		require.ErrorContains(t, err, path, "a marker that cannot be read is never the host profile")
		require.Empty(t, got)
	})
}

func TestPathsFor(t *testing.T) {
	host, err := PathsFor(ProfileHost)
	require.NoError(t, err)
	require.Equal(t, DefaultPaths(), host)
	require.False(t, host.Durable, "pmxcfs refuses to sync a directory")

	appliance, err := PathsFor(ProfileAppliance)
	require.NoError(t, err)
	require.Equal(t, Paths{
		Cluster:    "/var/lib/pco/cluster",
		Private:    "/var/lib/pco/private",
		Local:      "/var/lib/pco",
		MountCheck: "/var/lib/pco/.volume",
		Durable:    true,
	}, appliance)
	require.Equal(t, "/var/lib/pco/daemon.lock", appliance.NodeLock(), "the lock keeps its path")

	for _, profile := range []string{"", "container"} {
		_, err := PathsFor(profile)
		require.ErrorContains(t, err, `"`+profile+`"`)
	}
}

// appliancePaths are the paths of the appliance moved below a directory of
// the test, and the volume marker, which is not there yet.
func appliancePaths(t *testing.T) (Paths, string) {
	t.Helper()
	p, err := PathsFor(ProfileAppliance)
	require.NoError(t, err)
	root := t.TempDir()
	rebase := func(path string) string {
		rel, err := filepath.Rel(ApplianceLocal, path)
		require.NoError(t, err)
		return filepath.Join(root, rel)
	}
	p.Cluster, p.Private, p.Local, p.MountCheck = rebase(p.Cluster), rebase(p.Private), rebase(p.Local), rebase(p.MountCheck)
	return p, p.MountCheck
}

func TestInitOnTheApplianceRefusesAVolumeWithoutItsMarker(t *testing.T) {
	p, _ := appliancePaths(t)
	s, err := Open(p)
	require.NoError(t, err)

	require.ErrorIs(t, s.Init(), ErrNotMounted)
	requireMissing(t, p.Cluster)
	requireMissing(t, p.Private)
}

func TestInitOnTheApplianceMakesTheTwoRootsBelowTheLocalOne(t *testing.T) {
	p, marker := appliancePaths(t)
	writeFile(t, marker, "")
	s, err := Open(p)
	require.NoError(t, err)

	require.NoError(t, s.Init())

	for _, dir := range []string{p.Cluster, p.Private} {
		require.DirExists(t, dir)
		require.Equal(t, p.Local, filepath.Dir(dir))
	}
	require.NoError(t, s.SaveInstall(applianceInstall()))
	require.NoError(t, s.SaveCredential(credentialOf("c1")))
}

func TestTheVolumeMarkerIsTheMountCheckOfTheAppliance(t *testing.T) {
	p, marker := appliancePaths(t)
	writeFile(t, marker, "")
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, s.SaveInstall(applianceInstall()))

	require.NoError(t, os.Remove(marker))

	_, _, err = s.Install()
	require.ErrorIs(t, err, ErrNotMounted)
	require.ErrorContains(t, err, marker)
	_, err = s.Credentials()
	require.ErrorIs(t, err, ErrNotMounted)

	writeFile(t, marker, "")
	_, found, err := s.Install()
	require.NoError(t, err)
	require.True(t, found)
}
