package main

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"unicode"
	"unicode/utf16"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/present"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

func TestTheGoldens(t *testing.T) {
	for _, tc := range []struct{ flag, golden string }{
		{"-words", "words.gen.ts"},
		{"-types", "types.gen.ts"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, run([]string{tc.flag}, &out))

			path := filepath.Join("testdata", tc.golden)
			if *update {
				require.NoError(t, os.WriteFile(path, out.Bytes(), 0o644))
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, string(want), out.String(), "the module changed; run the test with -update when that is intended")
		})
	}
}

// The ranges the interface gets are exactly the characters Printable
// replaces, over every code point.
func TestTheClassesAreWhatPrintableReplaces(t *testing.T) {
	var all [][2]rune
	for _, c := range printableClasses() {
		all = append(all, ranges(c.in)...)
	}
	in := func(r rune) bool {
		for _, rg := range all {
			if rg[0] <= r && r <= rg[1] {
				return true
			}
		}
		return false
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if utf16.IsSurrogate(r) {
			// string(r) is U+FFFD; JavaScript has no such string.
			continue
		}
		if replaced := present.Printable(string(r)) != string(r); replaced != in(r) {
			t.Fatalf("U+%04X: Printable replaces it: %v; in the ranges: %v", r, replaced, in(r))
		}
	}
}

func TestRangesAreSortedAndApart(t *testing.T) {
	for _, c := range printableClasses() {
		rs := ranges(c.in)
		require.NotEmpty(t, rs, c.name)
		for i, rg := range rs {
			require.LessOrEqual(t, rg[0], rg[1], c.name)
			if i > 0 {
				require.Greater(t, rg[0], rs[i-1][1]+1, "%s: U+%04X follows the range before it", c.name, rg[0])
			}
		}
	}
	require.Equal(t, [][2]rune{{0x0, 0x1f}, {0x7f, 0x9f}}, ranges(unicode.IsControl))
	require.Equal(t, [][2]rune{{0x61c, 0x61c}, {0x200e, 0x200f}, {0x2028, 0x202e}, {0x2066, 0x2069}}, ranges(present.IsBidi))
}

func TestRunNeedsToBeToldWhatToWrite(t *testing.T) {
	require.EqualError(t, run(nil, io.Discard), "nothing to write: pass -words or -types")
	require.EqualError(t, run([]string{"-words", "-types"}, io.Discard), "pass -words or -types, not both")
}
