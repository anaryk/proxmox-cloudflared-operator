package pki

import (
	"crypto/x509"
	"strings"
	"time"
)

// minPrefix is the shortest part of a fingerprint an admin may type to admit
// a node. 32 bits are enough because the CA draws the serial of every
// certificate it issues, so no one can grind a certificate to match them.
const minPrefix = 8

// DueForRenewal reports whether a leaf is RenewAfter or longer past its issue,
// when an agent asks the controller for a new one.
func DueForRenewal(cert *x509.Certificate, now time.Time) bool {
	issued := cert.NotBefore.Add(Backdate)
	return !now.Before(issued.Add(RenewAfter))
}

// DueForSelfRenewal reports whether a leaf ends within SelfRenewBefore, when a
// host node issues its own anew at daemon start, controller or not.
func DueForSelfRenewal(cert *x509.Certificate, now time.Time) bool {
	return !now.Before(cert.NotAfter.Add(-SelfRenewBefore))
}

// MatchesPrefix reports whether typed, at least eight hexadecimal digits in
// either case and with or without colons between the pairs, starts the
// fingerprint of cert.
func MatchesPrefix(cert *x509.Certificate, typed string) bool {
	typed = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(typed), ":", ""))
	if len(typed) < minPrefix || strings.Trim(typed, "0123456789abcdef") != "" {
		return false
	}
	return strings.HasPrefix(Fingerprint(cert), typed)
}
