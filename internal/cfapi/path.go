package cfapi

import (
	"errors"
	"net/url"
	"strings"
)

// joinPath builds a request path from segments, escaping each one. A segment
// that is empty, is "." or ".." or contains a slash is refused: it would not
// stay one path element, and an id of ".." must never turn a call on a record
// into a call on the zone.
func joinPath(segments ...string) (string, error) {
	if len(segments) == 0 {
		return "", errors.New("path has no segments")
	}
	escaped := make([]string, len(segments))
	for i, s := range segments {
		if err := checkSegment(s); err != nil {
			return "", err
		}
		escaped[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(escaped, "/"), nil
}

// checkPath checks a path that is already escaped, as do and list receive it:
// every segment must unescape cleanly and pass checkSegment, both as written
// and unescaped, since a server may decode "%2e%2e" before it routes.
func checkPath(path string) error {
	for raw := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if err := checkSegment(raw); err != nil {
			return err
		}
		segment, err := url.PathUnescape(raw)
		if err != nil {
			return errors.New("path has an invalid escape")
		}
		if err := checkSegment(segment); err != nil {
			return err
		}
	}
	return nil
}

func checkSegment(s string) error {
	switch {
	case s == "":
		return errors.New("path has an empty segment")
	case s == "." || s == "..":
		return errors.New(`path has a "." or ".." segment`)
	case strings.Contains(s, "/"):
		return errors.New("path segment contains a slash")
	}
	return nil
}
