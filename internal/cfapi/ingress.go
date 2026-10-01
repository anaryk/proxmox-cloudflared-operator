package cfapi

import (
	"encoding/json"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// wireRule is an ingress rule as the API spells it: the origin options live
// in a nested object that is left out when none is set.
type wireRule struct {
	Hostname      string             `json:"hostname,omitempty"` // none for the catch-all
	Service       string             `json:"service"`
	OriginRequest *wireOriginRequest `json:"originRequest,omitempty"`
}

type wireOriginRequest struct {
	OriginServerName string `json:"originServerName,omitempty"`
	MatchSNIToHost   bool   `json:"matchSNItoHost,omitempty"`
	NoTLSVerify      bool   `json:"noTLSVerify,omitempty"`
	HTTPHostHeader   string `json:"httpHostHeader,omitempty"`
}

func toWireRule(r planner.IngressRule) wireRule {
	w := wireRule{Hostname: r.Hostname, Service: r.Service}
	opts := wireOriginRequest{
		OriginServerName: r.OriginServerName,
		MatchSNIToHost:   r.MatchSNIToHost,
		NoTLSVerify:      r.NoTLSVerify,
		HTTPHostHeader:   r.HTTPHostHeader,
	}
	if opts != (wireOriginRequest{}) {
		w.OriginRequest = &opts
	}
	return w
}

// rule maps back to the flat form.
func (w wireRule) rule() planner.IngressRule {
	r := planner.IngressRule{Hostname: w.Hostname, Service: w.Service}
	if o := w.OriginRequest; o != nil {
		r.OriginServerName = o.OriginServerName
		r.MatchSNIToHost = o.MatchSNIToHost
		r.NoTLSVerify = o.NoTLSVerify
		r.HTTPHostHeader = o.HTTPHostHeader
	}
	return r
}

// wireIngress is the configuration that is written.
type wireIngress struct {
	Ingress []wireRule `json:"ingress"`
}

// wireConfig is the answer to reading or writing a tunnel configuration.
type wireConfig struct {
	Version *int            `json:"version"` // a pointer, so that a missing version is not read as 0
	Config  json.RawMessage `json:"config"`  // null for a tunnel that has none yet
}

func (w wireConfig) version() (int, error) {
	if w.Version == nil {
		return 0, fmt.Errorf("%w: no configuration version", errUnexpected)
	}
	return *w.Version, nil
}

// tunnelConfig maps the answer to a TunnelConfig. What the mapping to
// IngressRule drops is looked at on the way: if any of it is set, the result
// is Foreign.
func (w wireConfig) tunnelConfig() (TunnelConfig, error) {
	version, err := w.version()
	if err != nil {
		return TunnelConfig{}, err
	}
	cfg := TunnelConfig{Version: version}
	if isNull(w.Config) {
		return cfg, nil
	}

	var parts map[string]json.RawMessage
	if err := json.Unmarshal(w.Config, &parts); err != nil {
		return TunnelConfig{}, fmt.Errorf("decoding configuration: %w", err)
	}
	for key, raw := range parts {
		switch key {
		case "ingress":
		case "warp-routing":
			// Read-only and deprecated: Cloudflare sets it from the routes of
			// the tunnel and a write cannot clear it, so it says nothing about
			// what pco manages.
		default:
			if !isDefault(raw) {
				cfg.Foreign = true
			}
		}
	}
	if isNull(parts["ingress"]) {
		return cfg, nil
	}
	var rules []json.RawMessage
	if err := json.Unmarshal(parts["ingress"], &rules); err != nil {
		return TunnelConfig{}, fmt.Errorf("decoding ingress: %w", err)
	}
	for _, raw := range rules {
		rule, foreign, err := decodeRule(raw)
		if err != nil {
			return TunnelConfig{}, err
		}
		cfg.Ingress = append(cfg.Ingress, rule)
		cfg.Foreign = cfg.Foreign || foreign
	}
	return cfg, nil
}

// decodeRule reads one rule and says whether it carries a field or an origin
// option that IngressRule has no place for.
func decodeRule(raw json.RawMessage) (rule planner.IngressRule, foreign bool, err error) {
	if isNull(raw) {
		// Decoding null into a rule would leave it empty, and an empty rule is
		// not what Cloudflare sent.
		return planner.IngressRule{}, false, fmt.Errorf("%w: ingress rule is null", errUnexpected)
	}
	var w wireRule
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &w); err != nil {
		return planner.IngressRule{}, false, fmt.Errorf("decoding ingress rule: %w", err)
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return planner.IngressRule{}, false, fmt.Errorf("decoding ingress rule: %w", err)
	}
	for key, value := range fields {
		switch key {
		case "hostname", "service":
		case "originRequest":
			foreign = foreign || hasUnmappedOption(value)
		default:
			foreign = foreign || !isDefault(value)
		}
	}
	return w.rule(), foreign, nil
}

// hasUnmappedOption reports whether an originRequest object sets an option
// other than the four that IngressRule has.
func hasUnmappedOption(raw json.RawMessage) bool {
	var opts map[string]json.RawMessage
	if json.Unmarshal(raw, &opts) != nil {
		// Not an object: the typed decoding of the rule has refused every
		// shape but null.
		return false
	}
	for key, value := range opts {
		switch key {
		case "originServerName", "matchSNItoHost", "noTLSVerify", "httpHostHeader":
		default:
			if !isDefault(value) {
				return true
			}
		}
	}
	return false
}

// isDefault reports whether a JSON value says nothing: null, false, 0, an
// empty string, list or object, or an object that holds only such values. A
// setting that is off, as in {"enabled": false}, is not one worth keeping.
func isDefault(raw json.RawMessage) bool {
	var v any
	return json.Unmarshal(raw, &v) == nil && isZero(v)
}

func isZero(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case bool:
		return !v
	case float64:
		return v == 0
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		for _, item := range v {
			if !isZero(item) {
				return false
			}
		}
		return true
	}
	return false
}
