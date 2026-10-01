package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const (
	// maxPlainName is the longest safe form of an id that is used as it is.
	maxPlainName = 120

	// A longer one keeps its first hashedPrefix characters, so that a listing
	// of the directory is still readable, and gains a hash of the whole id.
	hashedPrefix = 100
	hashMarker   = "_h_"
	hashChars    = 16
)

var namePattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9._-]*$`)

// FileName maps an id to the name of its file, without the extension: the id
// is lower-cased, a leading "*." becomes "_wildcard." and "/" becomes "_". A
// result of more than 120 characters is cut to its first 100, followed by "_h_"
// and the first 16 hex characters of the SHA-256 of the id, so that every
// valid hostname, which may be 253 characters long, has a name.
//
// Different ids can share a name: ones that differ only by case, or by "/"
// against "_", or "*." against "_wildcard.", and in theory two long ones. A
// file therefore records the id it was written for, and the ids of an object
// are compared with it, never taken from the name. Anything that is not made
// of the characters of a plain file name is an error, which keeps every path
// under the root.
func FileName(id string) (string, error) {
	safe := lowerASCII(id)
	if rest, ok := strings.CutPrefix(safe, "*."); ok {
		safe = "_wildcard." + rest
	}
	safe = strings.ReplaceAll(safe, "/", "_")
	if !namePattern.MatchString(safe) || strings.Contains(safe, "..") {
		return "", fmt.Errorf("%q cannot be used as a file name", id)
	}
	if len(safe) <= maxPlainName {
		return safe, nil
	}
	sum := sha256.Sum256([]byte(id))
	return safe[:hashedPrefix] + hashMarker + hex.EncodeToString(sum[:])[:hashChars], nil
}

// lowerASCII lower-cases A-Z only: a name is ASCII, and the lower-casing of
// some other characters, such as the Kelvin sign, would be an ASCII letter.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
