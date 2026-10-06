package pki

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDueForRenewal(t *testing.T) {
	cert, _ := issueClient(t, newCA(t, t0), hostID("node1"), t0)
	sixtyDays := 60 * 24 * time.Hour

	require.False(t, DueForRenewal(cert, t0))
	require.False(t, DueForRenewal(cert, t0.Add(sixtyDays-time.Second)))
	require.True(t, DueForRenewal(cert, t0.Add(sixtyDays)), "60 days after it was issued")
	require.True(t, DueForRenewal(cert, cert.NotAfter.Add(time.Hour)))
}

func TestDueForSelfRenewal(t *testing.T) {
	cert, _ := issueClient(t, newCA(t, t0), hostID("node1"), t0)
	thirtyDays := 30 * 24 * time.Hour

	require.False(t, DueForSelfRenewal(cert, t0))
	require.False(t, DueForSelfRenewal(cert, cert.NotAfter.Add(-thirtyDays-time.Second)))
	require.True(t, DueForSelfRenewal(cert, cert.NotAfter.Add(-thirtyDays)), "30 days before it ends")
	require.True(t, DueForSelfRenewal(cert, cert.NotAfter.Add(time.Hour)), "an expired one")
}

func TestMatchesPrefix(t *testing.T) {
	cert, _ := issueClient(t, newCA(t, t0), hostID("node1"), t0)
	fp := Fingerprint(cert)
	require.Len(t, fp, 64)
	require.Equal(t, strings.ToLower(fp), fp)

	var colons []string
	for i := 0; i < 8; i += 2 {
		colons = append(colons, strings.ToUpper(fp[i:i+2]))
	}
	wrong := []byte(fp[:8])
	if wrong[7] == '0' {
		wrong[7] = '1'
	} else {
		wrong[7] = '0'
	}

	cases := []struct {
		name  string
		typed string
		want  bool
	}{
		{"8 characters", fp[:8], true},
		{"7 characters", fp[:7], false},
		{"upper case", strings.ToUpper(fp[:8]), true},
		{"all of it", fp, true},
		{"with colons", strings.Join(colons, ":"), true},
		{"with spaces around", "  " + fp[:12] + "\n", true},
		{"one character off", string(wrong), false},
		{"not hexadecimal", "zzzzzzzz", false},
		{"longer than the fingerprint", fp + "00", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, MatchesPrefix(cert, c.typed))
		})
	}
}
