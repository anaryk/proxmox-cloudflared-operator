package upgrade

import (
	"fmt"
	"regexp"
	"strings"
)

// maxVersion is more than any version of pco or cloudflared needs.
const maxVersion = 64

var (
	// releaseVersionRe is a release of pco without its v: 1.2.3, 1.2.3-rc.1, or
	// the 0.0.0-SNAPSHOT-<commit> of a snapshot.
	releaseVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)
	// cloudflaredVersionRe is a release of cloudflared, year.month.patch.
	cloudflaredVersionRe = regexp.MustCompile(`^[0-9]{4}\.[0-9]{1,2}\.[0-9]+$`)
	// packageVersionRe is what dpkg says of an installed package, in the
	// characters a Debian version may have.
	packageVersionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+~:-]*$`)
)

// cleanRelease returns a version of pco as the release names it, without the
// v of its tag.
func cleanRelease(v string) (string, error) {
	plain := strings.TrimPrefix(v, "v")
	if len(plain) > maxVersion || !releaseVersionRe.MatchString(plain) {
		return "", fmt.Errorf("%q is no version of pco: want one such as 1.2.3", shorten(v))
	}
	return plain, nil
}

func checkCloudflared(v string) error {
	if !cloudflaredVersionRe.MatchString(v) {
		return fmt.Errorf("%q is no version of cloudflared: want one such as 2026.9.3", shorten(v))
	}
	return nil
}

func checkPackageVersion(v string) error {
	if len(v) > maxVersion || !packageVersionRe.MatchString(v) {
		return fmt.Errorf("%q is no version of a package", shorten(v))
	}
	return nil
}

// shorten keeps an error about a value that is not a version short.
func shorten(v string) string {
	const keep = 40
	if len(v) <= keep {
		return v
	}
	return v[:keep] + "..."
}

// debVersion is the version the package of a release says it is: nfpm turns
// the pre-release of 1.2.3-rc.1 into 1.2.3~rc.1, which dpkg sorts before
// 1.2.3.
func debVersion(release string) string {
	return strings.Replace(release, "-", "~", 1)
}

// releaseVersion is debVersion the other way round: the version in the name
// of the release and of its files.
func releaseVersion(deb string) string {
	return strings.Replace(deb, "~", "-", 1)
}

// compareVersions orders two Debian versions as dpkg does: epoch, then the
// upstream version, then the revision, each compared in runs of letters and
// of digits, where ~ sorts before anything, even the end.
func compareVersions(a, b string) int {
	ea, ua, ra := splitVersion(a)
	eb, ub, rb := splitVersion(b)
	if c := compareNumbers(ea, eb); c != 0 {
		return c
	}
	if c := compareParts(ua, ub); c != 0 {
		return c
	}
	return compareParts(ra, rb)
}

// splitVersion splits [epoch:]upstream[-revision].
func splitVersion(v string) (epoch, upstream, revision string) {
	epoch = "0"
	if i := strings.IndexByte(v, ':'); i >= 0 {
		epoch, v = v[:i], v[i+1:]
	}
	if i := strings.LastIndexByte(v, '-'); i >= 0 {
		return epoch, v[:i], v[i+1:]
	}
	return epoch, v, ""
}

// compareParts is the comparison of dpkg's verrevcmp.
func compareParts(a, b string) int {
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		for (i < len(a) && !isDigit(a[i])) || (j < len(b) && !isDigit(b[j])) {
			ac, bc := order(a, i), order(b, j)
			if ac != bc {
				return ac - bc
			}
			i, j = min(i+1, len(a)), min(j+1, len(b))
		}
		si := i
		for i < len(a) && isDigit(a[i]) {
			i++
		}
		sj := j
		for j < len(b) && isDigit(b[j]) {
			j++
		}
		if c := compareNumbers(a[si:i], b[sj:j]); c != 0 {
			return c
		}
	}
	return 0
}

// order is the weight of the character at i of a run that is not digits: the
// end and a digit weigh nothing, ~ less than that, letters their code and the
// rest more than any letter.
func order(s string, i int) int {
	if i >= len(s) {
		return 0
	}
	c := s[i]
	switch {
	case isDigit(c):
		return 0
	case c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
		return int(c)
	case c == '~':
		return -1
	}
	return int(c) + 256
}

// compareNumbers compares two runs of digits of any length by their value.
func compareNumbers(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
