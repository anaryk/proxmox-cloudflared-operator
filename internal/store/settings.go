package store

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

const (
	admissionTag     = "tag"
	admissionApprove = "approve"

	maxTagLen = 64
)

// tagPattern is what Proxmox accepts as a tag, in lower case.
var tagPattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9_\-+.]*$`)

// Duration is a time.Duration that is written as a string such as "10s".
type Duration time.Duration

// MarshalText writes the duration as time.Duration.String does.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalText reads what MarshalText writes.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// Settings is what the admin configures. They are decoded strictly, a key they
// have no field for is an error, so a field added later is a change of the
// schema and needs a schema version bump: a build that does not know it must
// refuse the file, not drop the field.
type Settings struct {
	GateTag      string            `json:"gateTag"`
	AllowHosts   []string          `json:"allowHosts,omitempty"`
	DenyHosts    []string          `json:"denyHosts,omitempty"`
	PollInterval Duration          `json:"pollInterval"`
	Grace        Duration          `json:"grace"`
	TrustStatic  bool              `json:"trustStatic,omitempty"`
	TrustedCIDRs []netip.Prefix    `json:"trustedCIDRs,omitempty"`
	Admission    string            `json:"admission"`          // "tag" or "approve"
	ZonePins     map[string]string `json:"zonePins,omitempty"` // zone name -> credential id
	ObserveOnly  bool              `json:"observeOnly"`
}

// DefaultSettings returns the settings of a fresh install, which only
// observes until the admin applies.
func DefaultSettings() Settings {
	return Settings{
		GateTag:      "cf-tunnel",
		PollInterval: Duration(10 * time.Second),
		Grace:        Duration(60 * time.Second),
		Admission:    admissionTag,
		ObserveOnly:  true,
	}
}

// normalized returns a copy of s with its patterns and zone names in their
// normal form, or an error naming the first field that is invalid.
func (s Settings) normalized() (Settings, error) {
	var err error
	if err = validateTag(s.GateTag); err != nil {
		return Settings{}, fmt.Errorf("gateTag %q: %w", s.GateTag, err)
	}
	if s.AllowHosts, err = normalizePatterns("allowHosts", s.AllowHosts); err != nil {
		return Settings{}, err
	}
	if s.DenyHosts, err = normalizePatterns("denyHosts", s.DenyHosts); err != nil {
		return Settings{}, err
	}
	if s.PollInterval <= 0 {
		return Settings{}, fmt.Errorf("pollInterval %s: must be positive", time.Duration(s.PollInterval))
	}
	if s.Grace <= 0 {
		return Settings{}, fmt.Errorf("grace %s: must be positive", time.Duration(s.Grace))
	}
	if s.Admission != admissionTag && s.Admission != admissionApprove {
		return Settings{}, fmt.Errorf("admission %q: want %q or %q", s.Admission, admissionTag, admissionApprove)
	}
	for i, p := range s.TrustedCIDRs {
		if !p.IsValid() || !p.Addr().Is4() {
			return Settings{}, fmt.Errorf("trustedCIDRs[%d] %s: want an IPv4 prefix", i, p)
		}
	}
	s.TrustedCIDRs = slices.Clone(s.TrustedCIDRs)
	if len(s.TrustedCIDRs) == 0 {
		s.TrustedCIDRs = nil
	}
	if s.ZonePins, err = normalizePins(s.ZonePins); err != nil {
		return Settings{}, err
	}
	return s, nil
}

func validateTag(tag string) error {
	switch {
	case len(tag) > maxTagLen:
		return fmt.Errorf("longer than %d characters", maxTagLen)
	case !tagPattern.MatchString(tag):
		return errors.New("want lower-case letters, digits and the characters _ - + ., the first one not - + or a dot")
	}
	return nil
}

// normalizePatterns returns the patterns in their normal form; field names
// the setting in an error.
func normalizePatterns(field string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	out := make([]string, len(patterns))
	for i, p := range patterns {
		n, err := hostname.NormalizePattern(p)
		if err != nil {
			return nil, fmt.Errorf("%s[%d] %q: %w", field, i, p, err)
		}
		out[i] = n
	}
	return out, nil
}

// normalizePins returns the zone pins with the zone names in their normal
// form. Two names that normalise alike are an error: which pin wins would
// depend on the order of a map.
func normalizePins(pins map[string]string) (map[string]string, error) {
	if len(pins) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pins))
	for _, zone := range slices.Sorted(maps.Keys(pins)) {
		name, err := hostname.Normalize(zone)
		if err != nil {
			return nil, fmt.Errorf("zonePins: %w", err)
		}
		if pins[zone] == "" {
			return nil, fmt.Errorf("zonePins[%q]: the credential id is empty", zone)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("zonePins[%q]: another key names zone %s too", zone, name)
		}
		out[name] = pins[zone]
	}
	return out, nil
}
