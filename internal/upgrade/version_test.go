package upgrade

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The order of dpkg, which apt keeps when it decides what is a downgrade.
func TestVersionsCompareAsDpkgComparesThem(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.2.3", "1.2.4", -1},
		{"1.10.0", "1.9.9", 1},
		{"1.2.3~rc.1", "1.2.3", -1},
		{"1.2.3~rc.1", "1.2.3~rc.2", -1},
		{"1.2.3~rc.10", "1.2.3~rc.9", 1},
		{"1.2.3", "1.2.3+b1", -1},
		{"1:0.1", "2.0", 1},
		{"0:1.0", "1.0", 0},
		{"1.0-1", "1.0-2", -1},
		{"1.0", "1.0-0", 0},
		{"1.0a", "1.0", 1},
		{"1.0~", "1.0", -1},
		{"1.0~~", "1.0~", -1},
		{"1.01", "1.1", 0},
		{"2026.9.3", "2026.10.0", -1},
		{"0.0.0~SNAPSHOT-0a1b2c3", "0.0.0~SNAPSHOT-f9e8d7c", -1},
		{"0.3.0~SNAPSHOT-abc", "0.3.0", -1},
	} {
		t.Run(tt.a+" "+tt.b, func(t *testing.T) {
			require.Equal(t, tt.want, sign(compareVersions(tt.a, tt.b)))
			require.Equal(t, -tt.want, sign(compareVersions(tt.b, tt.a)))
		})
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// A release is tagged v1.2.3-rc.1, its file is pco_1.2.3-rc.1_amd64.deb, and
// the package inside says 1.2.3~rc.1, as nfpm writes a pre-release.
func TestReleaseAndPackageVersionsMapOntoEachOther(t *testing.T) {
	for _, tt := range []struct{ release, deb string }{
		{"1.2.3", "1.2.3"},
		{"1.2.3-rc.1", "1.2.3~rc.1"},
		{"0.0.0-SNAPSHOT-3c3b62f", "0.0.0~SNAPSHOT-3c3b62f"},
	} {
		t.Run(tt.release, func(t *testing.T) {
			require.Equal(t, tt.deb, debVersion(tt.release))
			require.Equal(t, tt.release, releaseVersion(tt.deb))
		})
	}
}

func TestReleaseVersionsArePlain(t *testing.T) {
	for _, v := range []string{"1.2.3", "v1.2.3", "1.2.3-rc.1", "v0.0.0-SNAPSHOT-3c3b62f", "10.20.30"} {
		t.Run(v, func(t *testing.T) {
			got, err := cleanRelease(v)
			require.NoError(t, err)
			require.Equal(t, v[len(v)-len(got):], got)
		})
	}
	for _, v := range []string{"", "v", "1.2", "1.2.3.4", "1.2.3-", "1.2.3/../x", "1.2.3 ", "latest",
		"1.2.3-" + strings.Repeat("a", 70), "../1.2.3", "1.2.3_amd64"} {
		t.Run(v, func(t *testing.T) {
			_, err := cleanRelease(v)
			require.Error(t, err)
		})
	}
}

func TestCloudflaredVersionsAreItsCalendarVersions(t *testing.T) {
	for _, v := range []string{"2026.9.3", "2026.10.0"} {
		require.NoError(t, checkCloudflared(v), v)
	}
	for _, v := range []string{"", "2026.9", "26.9.3", "2026.9.3-1", "v2026.9.3", "2026.123.0", "2026.9.3/.."} {
		require.Error(t, checkCloudflared(v), v)
	}
}
