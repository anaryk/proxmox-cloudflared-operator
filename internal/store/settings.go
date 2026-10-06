package store

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// The admission modes of Settings.Admission.
const (
	AdmissionTag     = "tag"     // a guest with the gate tag is published
	AdmissionApprove = "approve" // a guest is published once an admin approved its identity

	maxTagLen = 64

	// minPollInterval and minGrace are the least the settings may say. A
	// grace of a moment would remove a record as soon as one cycle missed
	// its name, which turns the guard against a passing failure off; cycles
	// closer together would only ask Proxmox and Cloudflare more.
	minPollInterval = 5 * time.Second
	minGrace        = 30 * time.Second
	// minHostnamesPerGuest is the least cap of the hostnames of one guest.
	minHostnamesPerGuest = 1

	// A proof of identity is made again at least this often and never less
	// often than it may stand at all.
	minReverifyInterval = 10 * time.Second
	maxReverifyInterval = resolve.DefaultMaxProofAge

	// Cloudflare allows a user 1200 requests in 5 minutes: a budget leaves at
	// least 50 of them to the dashboard and other tools.
	minCloudflareBudget = 100
	maxCloudflareBudget = 1150
)

// FieldError is a setting that is not valid. Field is its path in the JSON of
// the settings, such as "pollInterval" or "denyHosts[2]".
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Err.Error() }

func (e *FieldError) Unwrap() error { return e.Err }

// invalid is the FieldError of field, with the message format makes.
func invalid(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Err: fmt.Errorf(format, args...)}
}

// Limit is the range the settings accept for one of them: a Duration or an
// int, nil where there is no bound.
type Limit struct {
	Min any
	Max any
}

// Limits are the ranges the settings are checked against, by the JSON name of
// the setting, for those that have one.
func Limits() map[string]Limit {
	return map[string]Limit{
		"pollInterval":         {Min: Duration(minPollInterval)},
		"grace":                {Min: Duration(minGrace)},
		"reverifyInterval":     {Min: Duration(minReverifyInterval), Max: Duration(maxReverifyInterval)},
		"maxHostnamesPerGuest": {Min: minHostnamesPerGuest},
		"cloudflareBudget":     {Min: minCloudflareBudget, Max: maxCloudflareBudget},
	}
}

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
// have no field for is an error, so a build that does not know a field added
// later refuses the file rather than drop the field. Before the first release
// such a field, as identityMinimum was, comes without a schema version bump.
type Settings struct {
	GateTag      string            `json:"gateTag"`
	AllowHosts   []string          `json:"allowHosts,omitempty"`
	DenyHosts    []string          `json:"denyHosts,omitempty"`
	PollInterval Duration          `json:"pollInterval"`
	Grace        Duration          `json:"grace"`
	TrustStatic  bool              `json:"trustStatic,omitempty"`
	TrustedCIDRs []netip.Prefix    `json:"trustedCIDRs,omitempty"`
	ManualCIDRs  []netip.Prefix    `json:"manualCIDRs,omitempty"`
	Admission    string            `json:"admission"`          // "tag" or "approve"
	ZonePins     map[string]string `json:"zonePins,omitempty"` // zone name -> credential id
	ObserveOnly  bool              `json:"observeOnly"`
	// IdentityMinimum is the least identity level a guest's address must be
	// proven at to be served: "observed", "filtered" or "port".
	IdentityMinimum string `json:"identityMinimum"`
	// MaxHostnamesPerGuest is how many hostnames the Notes of one guest may
	// name; a guest that names more publishes none.
	MaxHostnamesPerGuest int `json:"maxHostnamesPerGuest"`
	// ReverifyInterval is how long a proof of identity that the watch of the
	// network vouches for stands before it is made again.
	ReverifyInterval Duration `json:"reverifyInterval"`
	// CloudflareBudget is how many requests in 5 minutes the clients of one
	// credential spend at most.
	CloudflareBudget int `json:"cloudflareBudget"`
}

// DefaultSettings returns the settings of a fresh install, which only
// observes until the admin applies.
func DefaultSettings() Settings {
	return Settings{
		GateTag:              "cf-tunnel",
		PollInterval:         Duration(10 * time.Second),
		Grace:                Duration(60 * time.Second),
		Admission:            AdmissionTag,
		ObserveOnly:          true,
		IdentityMinimum:      string(resolve.LevelPort),
		MaxHostnamesPerGuest: model.DefaultMaxHostnamesPerGuest,
		ReverifyInterval:     Duration(time.Minute),
		CloudflareBudget:     cfapi.DefaultBudget,
	}
}

// raiseToMinimums raises the poll interval and the grace to their minimums where
// they are below, and returns a note for each, naming the file they were read
// from.
func (s *Settings) raiseToMinimums(file string) []string {
	var notes []string
	for _, f := range []struct {
		name    string
		value   *Duration
		minimum time.Duration
	}{
		{"pollInterval", &s.PollInterval, minPollInterval},
		{"grace", &s.Grace, minGrace},
	} {
		if time.Duration(*f.value) >= f.minimum {
			continue
		}
		notes = append(notes, fmt.Sprintf("settings: %s is %s in %s, below the minimum of %s; %s is used until it is raised there",
			f.name, time.Duration(*f.value), file, f.minimum, f.minimum))
		*f.value = Duration(f.minimum)
	}
	if s.MaxHostnamesPerGuest < minHostnamesPerGuest {
		notes = append(notes, fmt.Sprintf("settings: maxHostnamesPerGuest is %d in %s, below the minimum of %d; %d is used until it is raised there",
			s.MaxHostnamesPerGuest, file, minHostnamesPerGuest, minHostnamesPerGuest))
		s.MaxHostnamesPerGuest = minHostnamesPerGuest
	}
	return notes
}

// normalized returns a copy of s with its patterns and zone names in their
// normal form, or a *FieldError naming the first field that is invalid.
func (s Settings) normalized() (Settings, error) {
	var err error
	if err = validateTag(s.GateTag); err != nil {
		return Settings{}, invalid("gateTag", "gateTag %q: %w", s.GateTag, err)
	}
	if s.AllowHosts, err = normalizePatterns("allowHosts", s.AllowHosts); err != nil {
		return Settings{}, err
	}
	if s.DenyHosts, err = normalizePatterns("denyHosts", s.DenyHosts); err != nil {
		return Settings{}, err
	}
	if time.Duration(s.PollInterval) < minPollInterval {
		return Settings{}, invalid("pollInterval", "pollInterval %s: at least %s", time.Duration(s.PollInterval), minPollInterval)
	}
	if time.Duration(s.Grace) < minGrace {
		return Settings{}, invalid("grace", "grace %s: at least %s", time.Duration(s.Grace), minGrace)
	}
	if s.MaxHostnamesPerGuest < minHostnamesPerGuest {
		return Settings{}, invalid("maxHostnamesPerGuest", "maxHostnamesPerGuest %d: at least %d", s.MaxHostnamesPerGuest, minHostnamesPerGuest)
	}
	switch every := time.Duration(s.ReverifyInterval); {
	case every < minReverifyInterval:
		return Settings{}, invalid("reverifyInterval", "reverifyInterval %s: at least %s", every, minReverifyInterval)
	case every > maxReverifyInterval:
		return Settings{}, invalid("reverifyInterval", "reverifyInterval %s: at most %s", every, maxReverifyInterval)
	}
	if s.CloudflareBudget < minCloudflareBudget || s.CloudflareBudget > maxCloudflareBudget {
		return Settings{}, invalid("cloudflareBudget", "cloudflareBudget %d: from %d to %d", s.CloudflareBudget, minCloudflareBudget, maxCloudflareBudget)
	}
	if s.Admission != AdmissionTag && s.Admission != AdmissionApprove {
		return Settings{}, invalid("admission", "admission %q: want %q or %q", s.Admission, AdmissionTag, AdmissionApprove)
	}
	switch resolve.Level(s.IdentityMinimum) {
	case resolve.LevelObserved, resolve.LevelFiltered, resolve.LevelPort:
	default:
		return Settings{}, invalid("identityMinimum", "identityMinimum %q: want %q, %q or %q",
			s.IdentityMinimum, resolve.LevelObserved, resolve.LevelFiltered, resolve.LevelPort)
	}
	if s.TrustedCIDRs, err = normalizePrefixes("trustedCIDRs", s.TrustedCIDRs); err != nil {
		return Settings{}, err
	}
	if s.ManualCIDRs, err = normalizePrefixes("manualCIDRs", s.ManualCIDRs); err != nil {
		return Settings{}, err
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

// normalizePrefixes returns a copy of the prefixes, nil when there are none;
// field names the setting in an error.
func normalizePrefixes(field string, prefixes []netip.Prefix) ([]netip.Prefix, error) {
	for i, p := range prefixes {
		if !p.IsValid() || !p.Addr().Is4() {
			at := fmt.Sprintf("%s[%d]", field, i)
			return nil, invalid(at, "%s %s: want an IPv4 prefix", at, p)
		}
	}
	if len(prefixes) == 0 {
		return nil, nil
	}
	return slices.Clone(prefixes), nil
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
			at := fmt.Sprintf("%s[%d]", field, i)
			return nil, invalid(at, "%s %q: %w", at, p, err)
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
		field := fmt.Sprintf("zonePins[%q]", zone)
		name, err := hostname.Normalize(zone)
		if err != nil {
			return nil, invalid(field, "zonePins: %w", err)
		}
		if pins[zone] == "" {
			return nil, invalid(field, "%s: the credential id is empty", field)
		}
		if _, dup := out[name]; dup {
			return nil, invalid(field, "%s: another key names zone %s too", field, name)
		}
		out[name] = pins[zone]
	}
	return out, nil
}
