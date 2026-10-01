package hostname

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	ok := []struct{ name, in, want string }{
		{"mixed case", "App.Example.COM", "app.example.com"},
		{"trailing dot", "example.com.", "example.com"},
		{"wildcard", "*.Shop.cz", "*.shop.cz"},
		{"wildcard trailing dot", "*.shop.cz.", "*.shop.cz"},
		{"hyphen inside label", "a-b.example.com", "a-b.example.com"},
		{"punycode", "xn--bcher-kva.de", "xn--bcher-kva.de"},
		{"63 char label", strings.Repeat("a", 63) + ".example.com", strings.Repeat("a", 63) + ".example.com"},
		{"digits in a label that is not last", "123.example.com", "123.example.com"},
		{"digits and letters in the last label", "example.c0m", "example.c0m"},
		{"wildcard over numeric labels", "*.10.example.com", "*.10.example.com"},
	}
	for _, tt := range ok {
		t.Run("valid "+tt.name, func(t *testing.T) {
			got, err := Normalize(tt.in)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	bad := []struct{ name, in string }{
		{"empty", ""},
		{"dot only", "."},
		{"single label", "localhost"},
		{"empty label", "a..example.com"},
		{"two trailing dots", "example.com.."},
		{"leading hyphen", "-a.example.com"},
		{"trailing hyphen", "a-.example.com"},
		{"underscore", "a_b.example.com"},
		{"wildcard in the middle", "a.*.example.com"},
		{"bare wildcard", "*"},
		{"wildcard with one label", "*.com"},
		{"wildcard with one label and dot", "*.com."},
		{"wildcard last", "foo.*"},
		{"wildcard prefix in label", "*a.example.com"},
		{"space inside", "exa mple.com"},
		{"leading space", " example.com"},
		{"64 char label", strings.Repeat("a", 64) + ".example.com"},
		{"too long", strings.Repeat("a.", 127) + "com"},
		{"non-ascii", "bücher.de"},
		{"kelvin sign", "\u212a.example.com"}, // lower-cases to an ASCII k
		{"ipv4 literal", "10.0.0.5"},
		{"ipv4 literal with trailing dot", "10.0.0.5."},
		{"numeric last label", "example.123"},
		{"wildcard over an ipv4 literal", "*.10.0.0.5"},
	}
	for _, tt := range bad {
		t.Run("invalid "+tt.name, func(t *testing.T) {
			_, err := Normalize(tt.in)
			require.Error(t, err)
		})
	}
}

func TestNormalizeLengthLimit(t *testing.T) {
	// Every label is valid, so only the total length decides.
	label := strings.Repeat("a", 63)
	name := func(last int) string {
		return strings.Join([]string{label, label, label, strings.Repeat("b", last)}, ".")
	}

	atLimit := name(61)
	require.Len(t, atLimit, 253)
	got, err := Normalize(atLimit)
	require.NoError(t, err)
	require.Equal(t, atLimit, got)

	overLimit := name(62)
	require.Len(t, overLimit, 254)
	_, err = Normalize(overLimit)
	require.Error(t, err)
}

func TestIsWildcard(t *testing.T) {
	require.True(t, IsWildcard("*.shop.cz"))
	require.False(t, IsWildcard("shop.cz"))
	require.False(t, IsWildcard("*shop.cz"))
}

func TestCompareOrdersBySpecificity(t *testing.T) {
	names := []string{"*.shop.cz", "api.shop.cz", "shop.cz", "*.a.shop.cz", "b.a.shop.cz", "www.shop.cz"}
	slices.SortFunc(names, Compare)
	require.Equal(t, []string{
		"b.a.shop.cz", "api.shop.cz", "www.shop.cz", "shop.cz", "*.a.shop.cz", "*.shop.cz",
	}, names)
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal", "api.shop.cz", "api.shop.cz", 0},
		{"exact before wildcard", "shop.cz", "*.shop.cz", -1},
		{"wildcard after exact", "*.shop.cz", "shop.cz", 1},
		{"exact beats wildcard with more labels", "shop.cz", "*.a.b.shop.cz", -1},
		{"more labels first", "a.b.shop.cz", "b.shop.cz", -1},
		{"fewer labels last", "b.shop.cz", "a.b.shop.cz", 1},
		{"more wildcard labels first", "*.a.shop.cz", "*.shop.cz", -1},
		{"lexical", "api.shop.cz", "www.shop.cz", -1},
		{"lexical reversed", "www.shop.cz", "api.shop.cz", 1},
		{"lexical wildcards", "*.a.shop.cz", "*.b.shop.cz", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Compare(tt.a, tt.b))
		})
	}
}

func TestCovers(t *testing.T) {
	tests := []struct {
		name          string
		pattern, host string
		want          bool
	}{
		{"one level below", "*.shop.cz", "api.shop.cz", true},
		{"any depth below", "*.shop.cz", "a.b.shop.cz", true},
		{"apex is not covered", "*.shop.cz", "shop.cz", false},
		{"whole labels only", "*.shop.cz", "notshop.cz", false},
		{"other zone", "*.shop.cz", "api.shop.com", false},
		{"exact pattern is not a wildcard", "api.shop.cz", "api.shop.cz", false},
		{"wildcard host below the pattern", "*.shop.cz", "*.a.shop.cz", true},
		{"wildcard does not cover itself", "*.shop.cz", "*.shop.cz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Covers(tt.pattern, tt.host))
		})
	}
}

func TestMatchZone(t *testing.T) {
	zones := []string{"example.com", "dev.example.com", "shop.cz"}
	cases := []struct{ host, want string }{
		{"example.com", "example.com"},
		{"app.example.com", "example.com"},
		{"x.dev.example.com", "dev.example.com"},
		{"dev.example.com", "dev.example.com"},
		{"*.dev.example.com", "dev.example.com"},
		{"*.example.com", "example.com"},
		{"*.shop.cz", "shop.cz"},
	}
	for _, tt := range cases {
		t.Run(tt.host, func(t *testing.T) {
			got, ok := MatchZone(tt.host, zones)
			require.True(t, ok)
			require.Equal(t, tt.want, got)
		})
	}

	for _, host := range []string{"badexample.com", "example.org", "cz", "*.cz"} {
		t.Run("no zone "+host, func(t *testing.T) {
			_, ok := MatchZone(host, zones)
			require.False(t, ok)
		})
	}
}

func TestMatchZoneIgnoresZoneOrder(t *testing.T) {
	got, ok := MatchZone("x.dev.example.com", []string{"dev.example.com", "example.com"})
	require.True(t, ok)
	require.Equal(t, "dev.example.com", got)
}

func TestMatchZoneWithoutZones(t *testing.T) {
	_, ok := MatchZone("example.com", nil)
	require.False(t, ok)
}

func TestDepth(t *testing.T) {
	require.Equal(t, 0, Depth("example.com", "example.com"))
	require.Equal(t, 1, Depth("app.example.com", "example.com"))
	require.Equal(t, 1, Depth("*.example.com", "example.com"))
	require.Equal(t, 2, Depth("a.b.example.com", "example.com"))
}

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		name          string
		pattern, host string
		want          bool
	}{
		{"star matches everything", "*", "anything.example.com", true},
		{"wildcard matches deep names", "*.example.com", "a.b.example.com", true},
		{"wildcard does not match apex", "*.example.com", "example.com", false},
		{"wildcard needs whole labels", "*.example.com", "badexample.com", false},
		{"exact equal", "example.com", "example.com", true},
		{"exact does not match below", "example.com", "a.example.com", false},
		{"wildcard matches identical wildcard", "*.shop.cz", "*.shop.cz", true},
		{"wildcard matches wildcard below", "*.shop.cz", "*.a.shop.cz", true},
		{"wildcard does not match its apex", "*.shop.cz", "shop.cz", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, MatchPattern(tt.pattern, tt.host))
		})
	}
}
