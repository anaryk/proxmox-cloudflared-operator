package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func (f *fakeEngine) Networks() ([]engine.NetworkView, error) {
	f.record(context.Background(), "networks")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networks, f.err
}

func (f *fakeEngine) Network(id string) (engine.NetworkDetail, error) {
	f.record(context.Background(), "network:"+id)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return engine.NetworkDetail{}, f.err
	}
	i := slices.IndexFunc(f.networks, func(v engine.NetworkView) bool { return v.ID == id })
	if i < 0 {
		return engine.NetworkDetail{}, fmt.Errorf("%w: network %s is not attached", engine.ErrNotFound, id)
	}
	return engine.NetworkDetail{
		NetworkView: f.networks[i],
		Services: []store.ServiceAddr{{
			Network: id, Addr: netip.MustParseAddr("10.77.0.11"), Service: netip.MustParseAddr("198.18.1.0"),
			AllocatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		}},
		Served: []string{"www.example.com"},
	}, nil
}

func (f *fakeEngine) CandidateNetworks(ctx context.Context) ([]engine.CandidateNetwork, error) {
	f.record(ctx, "candidates")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.candidates, f.err
}

func (f *fakeEngine) ProbeNetwork(ctx context.Context, r engine.ProbeRequest) (engine.ProbeResult, error) {
	call := fmt.Sprintf("probe:%s:%d:%v:%v", r.Bridge, r.VLAN, r.Prefix, r.Address)
	if r.Scope != "" || r.Node != "" || len(r.Legs) > 0 {
		call += fmt.Sprintf(" scope=%s node=%s legs=%v", r.Scope, r.Node, r.Legs)
	}
	f.record(ctx, call)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probed, f.err
}

func (f *fakeEngine) Attach(ctx context.Context, r engine.AttachRequest) (engine.NetworkView, error) {
	call := fmt.Sprintf("attach:%s:%d:%v:%v uplink=%t overlap=%t", r.Bridge, r.VLAN, r.Prefix, r.Address, r.ConfirmUplink, r.OverlapOK)
	if r.Scope != "" || r.Node != "" || len(r.Legs) > 0 {
		call += fmt.Sprintf(" scope=%s node=%s legs=%v", r.Scope, r.Node, r.Legs)
	}
	f.record(ctx, call)
	if err := f.failure(); err != nil {
		return engine.NetworkView{}, err
	}
	return testNetwork(), nil
}

func (f *fakeEngine) Detach(ctx context.Context, id string, rev int64, served []string) error {
	f.record(ctx, fmt.Sprintf("detach:%s:%d:%v", id, rev, served))
	return f.failure()
}

// testNetwork is vmbr1 attached at 10.77.0.250 with its leg up, serving one
// route.
func testNetwork() engine.NetworkView {
	return engine.NetworkView{
		Network: store.Network{
			ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
			Legs: map[string]netip.Addr{"pve1": netip.MustParseAddr("10.77.0.250")}, LegMAC: "02:50:43:4f:00:01", Mark: 1,
			OverlapOK: true, CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Rev: 2,
		},
		Legs:    []engine.LegView{{Node: "pve1", Addr: netip.MustParseAddr("10.77.0.250"), Up: true}},
		Targets: 1, Routes: 1,
	}
}

func testCandidates() []engine.CandidateNetwork {
	return []engine.CandidateNetwork{
		{Bridge: "vmbr0", Uplink: true, Direct: true, Guests: []engine.GuestAddr{}, Prefixes: []netip.Prefix{}},
		{
			Bridge: "vmbr1", Attached: "vmbr1",
			Guests: []engine.GuestAddr{{
				Guest: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"},
				Addr:  netip.MustParseAddr("10.77.0.11"),
			}},
			Prefixes: []netip.Prefix{netip.MustParsePrefix("10.77.0.0/24")},
		},
		{Bridge: "pconet", Vnet: true, Guests: []engine.GuestAddr{}, Prefixes: []netip.Prefix{}, Refused: "pconet is pco's managed network, which is not attached"},
	}
}

func TestTheNetworksAreListed(t *testing.T) {
	f := &fakeEngine{networks: []engine.NetworkView{testNetwork()}}

	rec := do(newServer(f), http.MethodGet, "/v1/networks", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "networks.json", rec.Body.Bytes())

	rec = do(newServer(&fakeEngine{}), http.MethodGet, "/v1/networks", "")
	require.JSONEq(t, `[]`, rec.Body.String())
}

func TestANetworkIsShownWithItsServiceAddresses(t *testing.T) {
	f := &fakeEngine{networks: []engine.NetworkView{testNetwork()}}

	rec := do(newServer(f), http.MethodGet, "/v1/networks/vmbr1", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "network.json", rec.Body.Bytes())

	rec = do(newServer(f), http.MethodGet, "/v1/networks/vmbr7", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "not_found", errorCode(t, rec))
}

func TestTheCandidatesAreListed(t *testing.T) {
	f := &fakeEngine{candidates: testCandidates()}

	rec := do(newServer(f), http.MethodGet, "/v1/networks/candidates", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "candidates.json", rec.Body.Bytes())
	require.Equal(t, []string{"candidates"}, f.called(), "candidates is no network id")
}

func TestAProbeGoesToTheEngine(t *testing.T) {
	f := &fakeEngine{probed: engine.ProbeResult{
		Bridge: "vmbr0", Prefix: netip.MustParsePrefix("192.168.4.0/24"), Address: netip.MustParseAddr("192.168.4.249"),
		Free: true, AnsweredBy: []string{}, Uplink: true, Warning: "the leg's MAC becomes visible on the LAN of vmbr0",
		Overlap: "192.168.4.0/24 overlaps the node's route on vmbr9",
	}}

	rec := do(newServer(f), http.MethodPost, "/v1/networks/probe", `{"bridge":"vmbr0"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "probe.json", rec.Body.Bytes())
	require.Equal(t, []string{"probe:vmbr0:0:invalid Prefix:invalid IP"}, f.called())

	rec = do(newServer(f), http.MethodPost, "/v1/networks/probe", `{"bridge":"vmbr1","vlan":20,"prefix":"10.77.0.0/24","address":"10.77.0.250"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "probe:vmbr1:20:10.77.0.0/24:10.77.0.250", f.called()[1])
}

func TestAnAttachAnswersWithTheNetwork(t *testing.T) {
	f := &fakeEngine{}

	rec := do(newServer(f), http.MethodPost, "/v1/networks",
		`{"bridge":"pcot1","prefix":"192.168.4.0/24","address":"192.168.4.249","confirmUplink":true,"overlapOK":true}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	requireGolden(t, "networks_attached.json", rec.Body.Bytes())
	require.Equal(t, []string{"attach:pcot1:0:192.168.4.0/24:192.168.4.249 uplink=true overlap=true"}, f.called())
}

// On a cluster a probe and an attach name the scope and the legs of the
// other nodes; an attach may name only those.
func TestTheLegsOfACusterNetworkGoToTheEngine(t *testing.T) {
	f := &fakeEngine{}

	rec := do(newServer(f), http.MethodPost, "/v1/networks/probe", `{"bridge":"vmbr2","vlan":20,"scope":"cluster","legs":{"pve2":"10.20.0.249"}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = do(newServer(f), http.MethodPost, "/v1/networks",
		`{"bridge":"vmbr2","vlan":20,"prefix":"10.20.0.0/24","scope":"cluster","legs":{"pve3":"10.20.0.248"},"confirmUplink":true}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = do(newServer(f), http.MethodPost, "/v1/networks", `{"bridge":"vmbr1","prefix":"10.77.0.0/24","address":"10.77.0.250","scope":"node","node":"pve2"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	require.Equal(t, []string{
		"probe:vmbr2:20:invalid Prefix:invalid IP scope=cluster node= legs=map[pve2:10.20.0.249]",
		"attach:vmbr2:20:10.20.0.0/24:invalid IP uplink=true overlap=false scope=cluster node= legs=map[pve3:10.20.0.248]",
		"attach:vmbr1:0:10.77.0.0/24:10.77.0.250 uplink=false overlap=false scope=node node=pve2 legs=map[]",
	}, f.called())
}

func TestADetachNamesTheRevisionAndTheRoutesServed(t *testing.T) {
	f := &fakeEngine{}

	rec := do(newServer(f), http.MethodDelete, "/v1/networks/vmbr1.20?rev=3&served=www.example.com&served=app.example.com", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = do(newServer(f), http.MethodDelete, "/v1/networks/vmbr1?rev=4", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, []string{"detach:vmbr1.20:3:[www.example.com app.example.com]", "detach:vmbr1:4:[]"}, f.called())
}

func TestTheNetworkRequestsAreRefusedWithTheirField(t *testing.T) {
	for _, tt := range []struct {
		name, method, target, body string
		err                        error
		status                     int
		code, field                string
	}{
		{"a probe of a prefix that does not parse", http.MethodPost, "/v1/networks/probe", `{"bridge":"vmbr1","prefix":"10.77.0/24"}`, nil,
			http.StatusBadRequest, "invalid", "prefix"},
		{"a probe of an IPv6 address", http.MethodPost, "/v1/networks/probe", `{"bridge":"vmbr1","address":"fd00::1"}`, nil,
			http.StatusBadRequest, "invalid", "address"},
		{"a probe with a key it does not take", http.MethodPost, "/v1/networks/probe", `{"bridge":"vmbr1","overlapOK":true}`, nil,
			http.StatusBadRequest, "invalid", ""},
		{"an attach without a prefix", http.MethodPost, "/v1/networks", `{"bridge":"vmbr1","address":"10.77.0.250"}`, nil,
			http.StatusBadRequest, "invalid", "prefix"},
		{"an attach without an address", http.MethodPost, "/v1/networks", `{"bridge":"vmbr1","prefix":"10.77.0.0/24"}`, nil,
			http.StatusBadRequest, "invalid", "address"},
		{"an attach with a leg of no address", http.MethodPost, "/v1/networks", `{"bridge":"vmbr1","prefix":"10.77.0.0/24","legs":{"pve2":"nowhere"}}`, nil,
			http.StatusBadRequest, "invalid", "legs"},
		{"a probe with a leg of an IPv6 address", http.MethodPost, "/v1/networks/probe", `{"bridge":"vmbr1","scope":"cluster","legs":{"pve2":"fd00::1"}}`, nil,
			http.StatusBadRequest, "invalid", "legs"},
		{"an attach the engine refuses for a field", http.MethodPost, "/v1/networks", `{"bridge":"pcot1","prefix":"10.50.0.0/24","address":"10.50.0.249"}`,
			&engine.FieldError{Field: "overlapOK", Err: errors.New("10.50.0.0/24 overlaps the node's route on vmbr9; " + engine.OverlapHint)},
			http.StatusBadRequest, "invalid", "overlapOK"},
		{"an attach the engine refuses", http.MethodPost, "/v1/networks", `{"bridge":"vmbr0","prefix":"192.168.4.0/24","address":"192.168.4.250"}`,
			fmt.Errorf("%w: the node reaches vmbr0 directly; nothing to attach", engine.ErrRefused), http.StatusConflict, "refused", ""},
		{"a detach without a revision", http.MethodDelete, "/v1/networks/vmbr1", "", nil, http.StatusBadRequest, "invalid", "rev"},
		{"a detach with a revision of no number", http.MethodDelete, "/v1/networks/vmbr1?rev=x", "", nil, http.StatusBadRequest, "invalid", "rev"},
		{"a detach with two revisions", http.MethodDelete, "/v1/networks/vmbr1?rev=1&rev=2", "", nil, http.StatusBadRequest, "invalid", "rev"},
		{"a detach of routes not confirmed", http.MethodDelete, "/v1/networks/vmbr1?rev=1", "",
			fmt.Errorf("%w: network vmbr1 serves 1 route through its leg: www.example.com", engine.ErrRefused), http.StatusConflict, "refused", ""},
		{"a detach of a network that is not there", http.MethodDelete, "/v1/networks/vmbr7?rev=1", "",
			fmt.Errorf("%w: network vmbr7 is not attached", engine.ErrNotFound), http.StatusNotFound, "not_found", ""},
		{"candidates while a cycle runs too long", http.MethodGet, "/v1/networks/candidates", "", engine.ErrBusy,
			http.StatusServiceUnavailable, "unavailable", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{err: tt.err}

			rec := do(newServer(f), tt.method, tt.target, tt.body)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			body := parseError(t, rec)
			require.Equal(t, tt.code, body.Code)
			require.Equal(t, tt.field, body.Field)
		})
	}
}

func TestEveryNetworkMethodOfTheClientOverTheSocket(t *testing.T) {
	probe := engine.ProbeResult{
		Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.250"), Free: true, AnsweredBy: []string{},
	}
	f := &fakeEngine{networks: []engine.NetworkView{testNetwork()}, candidates: testCandidates(), probed: probe}
	socket := socketPath(t)
	serve(t, newServer(f), socket)
	c := apiclient.New(socket)
	ctx := t.Context()

	nets, err := c.Networks(ctx)
	require.NoError(t, err)
	require.Equal(t, f.networks, nets)
	n, err := c.Network(ctx, "vmbr1")
	require.NoError(t, err)
	require.Equal(t, testNetwork(), n.NetworkView)
	require.Equal(t, []string{"www.example.com"}, n.Served)
	candidates, err := c.CandidateNetworks(ctx)
	require.NoError(t, err)
	require.Equal(t, testCandidates(), candidates)
	res, raw, err := c.ProbeNetwork(ctx, engine.ProbeRequest{Bridge: "vmbr1", Address: netip.MustParseAddr("10.77.0.250")})
	require.NoError(t, err)
	require.Equal(t, probe, res)
	require.Contains(t, string(raw), `"free":true`)
	attached, err := c.Attach(ctx, engine.AttachRequest{Bridge: "vmbr1", VLAN: 20, Prefix: netip.MustParsePrefix("10.77.0.0/24"),
		Address: netip.MustParseAddr("10.77.0.250"), OverlapOK: true})
	require.NoError(t, err)
	require.Equal(t, testNetwork(), attached)
	require.NoError(t, c.Detach(ctx, "vmbr1.20", 3, []string{"www.example.com", "app.example.com"}))
	require.NoError(t, c.Detach(ctx, "vmbr1", 4, nil))

	require.Equal(t, []string{
		"networks", "network:vmbr1", "candidates", "probe:vmbr1:0:invalid Prefix:10.77.0.250",
		"attach:vmbr1:20:10.77.0.0/24:10.77.0.250 uplink=false overlap=true", "detach:vmbr1.20:3:[www.example.com app.example.com]", "detach:vmbr1:4:[]",
	}, f.called())

	_, err = c.Network(ctx, "vmbr7")
	require.ErrorIs(t, err, engine.ErrNotFound)
}

func TestTheNetworkActionsCarryTheActor(t *testing.T) {
	f := &fakeEngine{}
	s := New(f, "1.2.3", []uint32{0}, zerolog.Nop())

	rec := send(s, requestFrom(0, http.MethodPost, "/v1/networks", `{"bridge":"vmbr1","prefix":"10.77.0.0/24","address":"10.77.0.250"}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Equal(t, "root (cli)", engine.ActorOf(f.lastCtx()))

	rec = send(s, requestFrom(0, http.MethodDelete, "/v1/networks/vmbr1?rev=2", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "root (cli)", engine.ActorOf(f.lastCtx()))
}
