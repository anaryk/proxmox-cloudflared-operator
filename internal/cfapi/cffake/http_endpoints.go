package cffake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// The page sizes of the listings: what the API gives when none is asked for,
// and the largest it accepts.
const (
	defaultAccountPage, maxAccountPage = 20, 50
	defaultZonePage, maxZonePage       = 20, 50
	defaultTunnelPage, maxTunnelPage   = 20, 1000
	defaultRecordPage, maxRecordPage   = 100, 5_000_000
)

// Tokens.

// utc returns a copy of t in UTC, the zone Cloudflare writes its times in.
func utc(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

type wireToken struct {
	ID        string     `json:"id"`
	Status    string     `json:"status"`
	ExpiresOn *time.Time `json:"expires_on,omitempty"`
}

func (h *handler) verifyUser(r *http.Request, _ params) (reply, error) {
	return h.verify(r, "")
}

func (h *handler) verifyAccount(r *http.Request, p params) (reply, error) {
	return h.verify(r, p["account"])
}

func (h *handler) verify(r *http.Request, accountID string) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	st, err := h.f.verifyForm(r.Context(), accountID)
	if err != nil {
		return reply{}, err
	}
	w := wireToken{ID: st.ID, Status: st.Status}
	if st.ExpiresOn != nil {
		w.ExpiresOn = utc(*st.ExpiresOn)
	}
	return reply{result: w}, nil
}

// verifyForm answers one of the two forms of the token check, which Cloudflare
// splits by who owns the token: the user form, asked for with an empty account
// id, and the form of an account. A token is verified by the form of its owner
// and refused by the other.
func (f *Fake) verifyForm(ctx context.Context, accountID string) (cfapi.TokenStatus, error) {
	if accountID != "" {
		if err := cfapi.CheckID("account id", accountID); err != nil {
			return cfapi.TokenStatus{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(ctx, opVerify, "VerifyToken"); err != nil {
		return cfapi.TokenStatus{}, err
	}
	if f.tokenOwner != accountID {
		return cfapi.TokenStatus{}, &cfapi.Error{Status: http.StatusUnauthorized, Codes: []int{codeInvalidToken}, Message: "Invalid API Token"}
	}
	st := f.token
	if st.ExpiresOn != nil {
		expires := *st.ExpiresOn
		st.ExpiresOn = &expires
	}
	return st, nil
}

// Accounts and zones.

type wireAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (h *handler) listAccounts(r *http.Request, _ params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultAccountPage, maxAccountPage)
	if err != nil {
		return reply{}, err
	}
	all, err := h.f.Accounts(r.Context())
	if err != nil {
		return reply{}, err
	}
	page := pageOf(all, pg)
	items := make([]wireAccount, len(page))
	for i, a := range page {
		items[i] = wireAccount{ID: a.ID, Name: a.Name, Type: "standard"}
	}
	return reply{items, pg.info(len(items), len(all), true)}, nil
}

type wireZone struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Account struct {
		ID string `json:"id"`
	} `json:"account"`
}

func (h *handler) listZones(r *http.Request, _ params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultZonePage, maxZonePage)
	if err != nil {
		return reply{}, err
	}
	all, err := h.f.Zones(r.Context())
	if err != nil {
		return reply{}, err
	}
	page := pageOf(all, pg)
	items := make([]wireZone, len(page))
	for i, z := range page {
		items[i] = wireZone{ID: z.ID, Name: z.Name, Status: z.Status}
		items[i].Account.ID = z.AccountID
	}
	return reply{items, pg.info(len(items), len(all), true)}, nil
}

// Tunnels.

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

func newWireTunnel(accountID string, t cfapi.Tunnel) wireTunnel {
	return wireTunnel{
		ID: t.ID, AccountTag: accountID, Name: t.Name, Status: t.Status, CreatedAt: t.CreatedAt.UTC(),
		TunType: "cfd_tunnel", RemoteConfig: true,
	}
}

func (h *handler) listTunnels(r *http.Request, p params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page", "name", "is_deleted", "include_prefix"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultTunnelPage, maxTunnelPage)
	if err != nil {
		return reply{}, err
	}
	deleted := q.Get("is_deleted")
	if deleted != "" && deleted != "true" && deleted != "false" {
		return reply{}, badRequest("is_deleted must be true or false")
	}
	account := p["account"]
	matched, total, err := h.f.listTunnels(r.Context(), account, q.Get("name"), q.Get("include_prefix"))
	if err != nil {
		return reply{}, err
	}
	if deleted == "true" {
		matched = nil // the fake does not keep a tunnel it deleted
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

// listTunnels is the listing of the tunnels of an account, which the API
// filters by an exact name or by the prefix of a name. It returns the tunnels
// that match, and how many the account has. The call it logs is the one the
// client makes of it: FindTunnel for a name, Tunnels otherwise.
func (f *Fake) listTunnels(ctx context.Context, accountID, name, prefix string) (matched []cfapi.Tunnel, total int, err error) {
	if err := cfapi.CheckID("account id", accountID); err != nil {
		return nil, 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	method, arg := "Tunnels", prefix
	if name != "" {
		method, arg = "FindTunnel", name
	}
	if err := f.begin(ctx, opTunnelRead, method, accountID, arg); err != nil {
		return nil, 0, err
	}
	if err := f.account(accountID); err != nil {
		return nil, 0, err
	}
	for _, t := range f.tunnels {
		if t.account != accountID {
			continue
		}
		total++
		if (name == "" || t.Name == name) && strings.HasPrefix(t.Name, prefix) {
			matched = append(matched, t.Tunnel)
		}
	}
	return matched, total, nil
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
	return reply{result: newWireTunnel(p["account"], t)}, nil
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
	ID string `json:"id"`
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
			items[i].Conns[n].ID = fmt.Sprintf("%s-%d", c.ID, n+1)
		}
	}
	return reply{result: items}, nil
}

// Tunnel configurations.

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

// DNS records.

type wireRecord struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Name       string    `json:"name"`
	Content    string    `json:"content"`
	Proxied    bool      `json:"proxied"`
	TTL        int       `json:"ttl"`
	Comment    *string   `json:"comment"` // null for none, as the API sends it
	ModifiedOn time.Time `json:"modified_on"`
}

func newWireRecord(r cfapi.Record) wireRecord {
	w := wireRecord{ID: r.ID, Type: r.Type, Name: r.Name, Content: r.Content, Proxied: r.Proxied, TTL: r.TTL, ModifiedOn: r.ModifiedOn.UTC()}
	if r.Comment != "" {
		w.Comment = &r.Comment
	}
	return w
}

func (h *handler) listRecords(r *http.Request, p params) (reply, error) {
	q := r.URL.Query()
	if err := checkQuery(q, "page", "per_page", "type", "name", "comment.startswith"); err != nil {
		return reply{}, err
	}
	pg, err := readPaging(q, defaultRecordPage, maxRecordPage)
	if err != nil {
		return reply{}, err
	}
	filter := cfapi.RecordFilter{Type: q.Get("type"), Name: q.Get("name"), CommentPrefix: q.Get("comment.startswith")}
	all, err := h.f.Records(r.Context(), p["zone"], filter)
	if err != nil {
		return reply{}, err
	}
	page := pageOf(all, pg)
	items := make([]wireRecord, len(page))
	for i, rec := range page {
		items[i] = newWireRecord(rec)
	}
	return reply{items, pg.info(len(items), len(all), true)}, nil
}

// readRecord reads the body of a write of a record. A new record has to name
// its type, name and content. A change has to carry every field: Cloudflare
// leaves out of a change what the body does not carry, the fake replaces the
// record, and the client says everything it means.
func readRecord(r *http.Request, change bool) (cfapi.Record, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return cfapi.Record{}, err
	}
	data, err := readBody(r)
	if err != nil {
		return cfapi.Record{}, err
	}
	var body struct {
		Type    *string `json:"type"`
		Name    *string `json:"name"`
		Content *string `json:"content"`
		Proxied *bool   `json:"proxied"`
		Comment *string `json:"comment"`
		TTL     *int    `json:"ttl"`
	}
	if err := decodeObject(data, &body, "type", "name", "content", "proxied", "comment", "ttl"); err != nil {
		return cfapi.Record{}, err
	}
	for _, f := range []struct {
		name            string
		present, needed bool
	}{
		{"type", body.Type != nil, true},
		{"name", body.Name != nil, true},
		{"content", body.Content != nil, true},
		{"proxied", body.Proxied != nil, change},
		{"comment", body.Comment != nil, change},
		{"ttl", body.TTL != nil, change},
	} {
		if f.needed && !f.present {
			return cfapi.Record{}, badRequest("%s is required", f.name)
		}
	}
	rec := cfapi.Record{Type: deref(body.Type), Name: deref(body.Name), Content: deref(body.Content), Comment: deref(body.Comment)}
	if body.Proxied != nil {
		rec.Proxied = *body.Proxied
	}
	if body.TTL != nil {
		rec.TTL = *body.TTL
	}
	return rec, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *handler) createRecord(r *http.Request, p params) (reply, error) {
	rec, err := readRecord(r, false)
	if err != nil {
		return reply{}, err
	}
	created, err := h.f.CreateRecord(r.Context(), p["zone"], rec)
	if err != nil {
		return reply{}, err
	}
	return reply{result: newWireRecord(created)}, nil
}

func (h *handler) updateRecord(r *http.Request, p params) (reply, error) {
	rec, err := readRecord(r, true)
	if err != nil {
		return reply{}, err
	}
	rec.ID = p["record"]
	updated, err := h.f.UpdateRecord(r.Context(), p["zone"], rec)
	if err != nil {
		return reply{}, err
	}
	return reply{result: newWireRecord(updated)}, nil
}

func (h *handler) deleteRecord(r *http.Request, p params) (reply, error) {
	if err := checkQuery(r.URL.Query()); err != nil {
		return reply{}, err
	}
	if err := h.f.DeleteRecord(r.Context(), p["zone"], p["record"]); err != nil {
		return reply{}, err
	}
	return reply{result: map[string]string{"id": p["record"]}}, nil
}
