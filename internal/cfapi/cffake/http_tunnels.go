package cffake

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// What the API gives of a listing of tunnels when no page size is asked for,
// and from least to most what it accepts.
const (
	defaultTunnelPage, minTunnelPage, maxTunnelPage = 20, 1, 1000
)

type wireTunnel struct {
	ID           string     `json:"id"`
	AccountTag   string     `json:"account_tag"`
	Name         string     `json:"name"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	DeletedAt    *time.Time `json:"deleted_at"`
	TunType      string     `json:"tun_type"`
	RemoteConfig bool       `json:"remote_config"`
}

func newWireTunnel(accountID string, t listedTunnel) wireTunnel {
	w := wireTunnel{
		ID: t.ID, AccountTag: accountID, Name: t.Name, Status: t.Status, CreatedAt: t.CreatedAt.UTC(),
		TunType: "cfd_tunnel", RemoteConfig: true,
	}
	if t.DeletedAt != nil {
		w.DeletedAt = utc(*t.DeletedAt)
	}
	return w
}

func (h *handler) listTunnels(r *http.Request, p params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page", "name", "is_deleted", "include_prefix"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultTunnelPage, minTunnelPage, maxTunnelPage)
	if err != nil {
		return reply{}, err
	}
	flt := tunnelFilter{name: q.Get("name"), prefix: q.Get("include_prefix")}
	switch q.Get("is_deleted") {
	case "":
	case "true":
		flt.deleted = deletedOnly
	case "false":
		flt.deleted = liveOnly
	default:
		return reply{}, badRequest("is_deleted must be true or false")
	}
	if h.ignoreIsDeleted {
		flt.deleted = liveAndDeleted
	}

	account := p["account"]
	if err := cfapi.CheckID("account id", account); err != nil {
		return reply{}, err
	}
	// The call it logs is the one the client makes of it.
	method, arg := "Tunnels", flt.prefix
	if flt.name != "" {
		method, arg = "FindTunnel", flt.name
	}
	matched, total, err := h.f.tunnelListing(r.Context(), method, account, arg, flt)
	if err != nil {
		return reply{}, err
	}
	if h.filteredTotals {
		total = len(matched)
	}
	page := pageOf(matched, pg)
	items := make([]wireTunnel, len(page))
	for i, t := range page {
		items[i] = newWireTunnel(account, t)
	}
	return reply{items, pg.info(len(items), total, false)}, nil
}

func (h *handler) createTunnel(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	data, err := readBody(r)
	if err != nil {
		return reply{}, err
	}
	var body struct {
		Name      *string `json:"name"`
		ConfigSrc *string `json:"config_src"`
	}
	if err := decodeObject(data, &body, "name", "config_src"); err != nil {
		return reply{}, err
	}
	switch {
	case body.Name == nil:
		return reply{}, badRequest("name is required")
	case body.ConfigSrc != nil && *body.ConfigSrc != "cloudflare":
		return reply{}, badRequest("config_src must be cloudflare: the fake does not run tunnels that are configured locally")
	}
	t, err := h.f.CreateTunnel(r.Context(), p["account"], *body.Name)
	if err != nil {
		return reply{}, err
	}
	return reply{result: newWireTunnel(p["account"], listedTunnel{Tunnel: t})}, nil
}

func (h *handler) deleteTunnel(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	if err := h.f.DeleteTunnel(r.Context(), p["account"], p["tunnel"]); err != nil {
		return reply{}, err
	}
	return reply{result: map[string]string{"id": p["tunnel"]}}, nil
}

func (h *handler) tunnelToken(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	token, err := h.f.TunnelToken(r.Context(), p["account"], p["tunnel"])
	if err != nil {
		return reply{}, err
	}
	return reply{result: token}, nil
}

type wireConnector struct {
	ID            string     `json:"id"`
	Version       string     `json:"version"`
	ConfigVersion int        `json:"config_version"`
	Conns         []wireConn `json:"conns"`
}

type wireConn struct {
	ID       string `json:"id"`
	OriginIP string `json:"origin_ip,omitempty"`
}

func (h *handler) listConnections(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	all, err := h.f.Connectors(r.Context(), p["account"], p["tunnel"])
	if err != nil {
		return reply{}, err
	}
	items := make([]wireConnector, len(all))
	for i, c := range all {
		items[i] = wireConnector{ID: c.ID, Version: c.Version, ConfigVersion: c.ConfigVersion, Conns: make([]wireConn, max(c.Connections, 0))}
		for n := range items[i].Conns {
			items[i].Conns[n] = wireConn{ID: fmt.Sprintf("%s-%d", c.ID, n+1), OriginIP: c.OriginIP}
		}
	}
	return reply{result: items}, nil
}

// wireRule is an ingress rule as the API spells it. The handler has its own
// reading of it, apart from the client's, so that the two are compared.
type wireRule struct {
	Hostname      string             `json:"hostname,omitempty"`
	Service       string             `json:"service"`
	OriginRequest *wireOriginRequest `json:"originRequest,omitempty"`
}

type wireOriginRequest struct {
	OriginServerName string `json:"originServerName,omitempty"`
	MatchSNIToHost   bool   `json:"matchSNItoHost,omitempty"`
	NoTLSVerify      bool   `json:"noTLSVerify,omitempty"`
	HTTPHostHeader   string `json:"httpHostHeader,omitempty"`
}

func newWireRule(r planner.IngressRule) wireRule {
	w := wireRule{Hostname: r.Hostname, Service: r.Service}
	opts := wireOriginRequest{
		OriginServerName: r.OriginServerName, MatchSNIToHost: r.MatchSNIToHost,
		NoTLSVerify: r.NoTLSVerify, HTTPHostHeader: r.HTTPHostHeader,
	}
	if opts != (wireOriginRequest{}) {
		w.OriginRequest = &opts
	}
	return w
}

// wireConfig is the answer to reading or writing a configuration.
type wireConfig struct {
	TunnelID string       `json:"tunnel_id"`
	Version  int          `json:"version"`
	Source   string       `json:"source"`
	Config   *wireIngress `json:"config"`
}

type wireIngress struct {
	Ingress       []wireRule       `json:"ingress,omitempty"`
	WarpRouting   wireWarpRouting  `json:"warp-routing"`
	OriginRequest *foreignSettings `json:"originRequest,omitempty"`
}

type wireWarpRouting struct {
	Enabled bool `json:"enabled"`
}

// foreignSettings is what a configuration holds that pco does not manage.
type foreignSettings struct {
	ConnectTimeout int `json:"connectTimeout"`
}

// newWireConfig is a configuration as Cloudflare answers for it. A tunnel that
// nothing was written to has none.
func newWireConfig(tunnelID string, version int, rules []planner.IngressRule, foreign bool) wireConfig {
	w := wireConfig{TunnelID: tunnelID, Version: version, Source: "cloudflare"}
	if version == 0 && len(rules) == 0 && !foreign {
		return w
	}
	w.Config = &wireIngress{WarpRouting: wireWarpRouting{Enabled: true}}
	for _, r := range rules {
		w.Config.Ingress = append(w.Config.Ingress, newWireRule(r))
	}
	if foreign {
		w.Config.OriginRequest = &foreignSettings{ConnectTimeout: 30}
	}
	return w
}

func (h *handler) getConfig(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	cfg, err := h.f.TunnelConfig(r.Context(), p["account"], p["tunnel"])
	if err != nil {
		return reply{}, err
	}
	return reply{result: newWireConfig(p["tunnel"], cfg.Version, cfg.Ingress, cfg.Foreign)}, nil
}

func (h *handler) putConfig(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	data, err := readBody(r)
	if err != nil {
		return reply{}, err
	}
	rules, err := parseIngress(data)
	if err != nil {
		return reply{}, err
	}
	version, err := h.f.PutTunnelConfig(r.Context(), p["account"], p["tunnel"], rules)
	if err != nil {
		return reply{}, err
	}
	return reply{result: newWireConfig(p["tunnel"], version, rules, false)}, nil
}

// parseIngress reads the body of a write of a configuration: {"config":
// {"ingress": [...]}}. Nothing else is accepted, as the fake keeps nothing
// else.
func parseIngress(data []byte) ([]planner.IngressRule, error) {
	var body struct {
		Config json.RawMessage `json:"config"`
	}
	if err := decodeObject(data, &body, "config"); err != nil {
		return nil, err
	}
	if absent(body.Config) {
		return nil, badRequest("config is required")
	}
	var cfg struct {
		Ingress []json.RawMessage `json:"ingress"`
	}
	if err := decodeObject(body.Config, &cfg, "ingress"); err != nil {
		return nil, err
	}
	var rules []planner.IngressRule
	for i, raw := range cfg.Ingress {
		var w struct {
			Hostname      string          `json:"hostname"`
			Service       string          `json:"service"`
			OriginRequest json.RawMessage `json:"originRequest"`
		}
		if err := decodeObject(raw, &w, "hostname", "service", "originRequest"); err != nil {
			return nil, inRule(i, err)
		}
		rule := planner.IngressRule{Hostname: w.Hostname, Service: w.Service}
		if !absent(w.OriginRequest) {
			var o wireOriginRequest
			if err := decodeObject(w.OriginRequest, &o, "originServerName", "matchSNItoHost", "noTLSVerify", "httpHostHeader"); err != nil {
				return nil, inRule(i, err, "originRequest")
			}
			rule.OriginServerName, rule.MatchSNIToHost, rule.NoTLSVerify, rule.HTTPHostHeader =
				o.OriginServerName, o.MatchSNIToHost, o.NoTLSVerify, o.HTTPHostHeader
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// absent says whether a field was left out of a body or is null.
func absent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// inRule says which rule of the ingress an error of reading it is about.
func inRule(i int, err error, path ...string) error {
	where := strings.Join(append([]string{fmt.Sprintf("ingress rule #%d", i+1)}, path...), ": ")
	var apiErr *cfapi.Error
	if errors.As(err, &apiErr) {
		return badRequest("%s: %s", where, apiErr.Message)
	}
	return fmt.Errorf("%s: %w", where, err)
}
