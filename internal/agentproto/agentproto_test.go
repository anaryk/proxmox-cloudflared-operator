package agentproto

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEpochOrder(t *testing.T) {
	e := func(g int, nonce string) Epoch { return Epoch{InstallID: "0123456789ab", Generation: g, Nonce: nonce} }
	cases := []struct {
		name    string
		a, b    Epoch
		cmp     int
		same    bool
		install string
	}{
		{"the same epoch", e(7, "aa"), e(7, "aa"), 0, true, ""},
		{"the same generation, another nonce", e(7, "aa"), e(7, "bb"), 0, false, ""},
		{"a lower generation", e(6, "aa"), e(7, "aa"), -1, false, ""},
		{"a higher generation", e(8, "aa"), e(7, "zz"), 1, false, ""},
		{"the install is not compared", e(7, "aa"), e(7, "aa"), 0, true, "fedcba987654"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.install != "" {
				c.b.InstallID = c.install
			}
			cmp, same := c.a.Order(c.b)
			require.Equal(t, c.cmp, cmp)
			require.Equal(t, c.same, same)
		})
	}
}

func TestEpochQueryForm(t *testing.T) {
	e := epoch7()
	require.Equal(t, "7.a1b2c3d4", e.String())
	got, err := ParseEpoch(e.String())
	require.NoError(t, err)
	require.Equal(t, Epoch{Generation: 7, Nonce: "a1b2c3d4"}, got)

	top := strconv.Itoa(MaxGeneration) + ".n0nce"
	got, err = ParseEpoch(top)
	require.NoError(t, err)
	require.Equal(t, MaxGeneration, got.Generation)

	for _, s := range []string{
		"",
		"7",
		"7.",
		".a1b2",
		"0.a1b2",
		"07.a1b2",
		"+7.a1b2",
		"-7.a1b2",
		" 7.a1b2",
		"7.A1B2",
		"7.a1.b2",
		"7.a1b2 ",
		strconv.Itoa(MaxGeneration+1) + ".n0nce",
		"99999999999999999999.n0nce",
	} {
		t.Run(s, func(t *testing.T) {
			_, err := ParseEpoch(s)
			require.Error(t, err)
		})
	}
}

func TestNegotiate(t *testing.T) {
	cases := []struct {
		name              string
		agent, controller Range
		want              int
		ok                bool
	}{
		{"both speak one", Range{1, 1}, Range{1, 1}, 1, true},
		{"a newer agent", Range{1, 2}, Range{1, 1}, 1, true},
		{"a newer controller", Range{1, 1}, Range{1, 2}, 1, true},
		{"the highest both speak", Range{1, 2}, Range{1, 2}, 2, true},
		{"overlapping by one", Range{2, 3}, Range{1, 2}, 2, true},
		{"overlapping by one, the other way", Range{1, 2}, Range{2, 3}, 2, true},
		{"an agent too old", Range{1, 1}, Range{2, 3}, 0, false},
		{"an agent too new", Range{3, 4}, Range{1, 2}, 0, false},
		{"a range upside down", Range{2, 1}, Range{1, 2}, 0, false},
		{"no version", Range{0, 1}, Range{1, 1}, 0, false},
		{"nothing said", Range{}, Range{1, 1}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Negotiate(c.agent, c.controller)
			require.Equal(t, c.ok, ok)
			require.Equal(t, c.want, got)
		})
	}
}

func TestARefusalOutOfRangeIsTheOnlyOneToWaitOut(t *testing.T) {
	cases := map[string]bool{
		RefusalRemoved:      true,
		RefusalContested:    true,
		RefusalNotEnrolled:  true,
		RefusalOtherInstall: true,
		"added-later":       true,
		RefusalOutOfRange:   false,
		"":                  false,
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			require.Equal(t, want, Refusal{Code: code}.Definitive())
		})
	}
}
