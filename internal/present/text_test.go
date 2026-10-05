package present

import (
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
)

// The cases words.json cannot hold, as JSON is text: bytes that are no UTF-8.
func TestPrintableReplacesBytesThatAreNoUTF8(t *testing.T) {
	require.Equal(t, "a\ufffdb", Printable("a\xffb"))
	require.Equal(t, "\ufffd?", Printable("\xc3\x1b"))
}

func TestPrintableLeavesNothingToChance(t *testing.T) {
	// Every rune that is neither printable nor a space is gone, whatever the
	// category: this walks the first planes.
	for r := rune(0); r < 0x3000; r++ {
		out := Printable(string(r))
		for _, got := range out {
			require.False(t, unicode.IsControl(got), "U+%04X came out as a control character", r)
			require.False(t, IsBidi(got), "U+%04X came out as a bidirectional control", r)
			require.False(t, unicode.Is(unicode.Cf, got), "U+%04X came out as a format character", r)
		}
	}
}
