package api

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func (f *fakeEngine) Managed() (engine.ManagedView, error) {
	f.record(context.Background(), "managed")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.managed, f.err
}

func (f *fakeEngine) ApproveManaged(ctx context.Context, owner, identity string, accept bool) (store.Consent, error) {
	f.record(ctx, fmt.Sprintf("approve managed:%s:%s:%t", owner, identity, accept))
	if err := f.failure(); err != nil {
		return store.Consent{}, err
	}
	return store.Consent{Owner: owner, Identity: "uuid:101", At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), AcceptFirewallImpact: accept}, nil
}

func (f *fakeEngine) RevokeManaged(ctx context.Context, owner string) error {
	f.record(ctx, "revoke managed:"+owner)
	return f.failure()
}

func (f *fakeEngine) EnableManaged(ctx context.Context) error {
	f.record(ctx, "enable managed")
	return f.failure()
}

// testManaged is the managed network of a host with one attachment and one
// guest that waits for the gate.
func testManaged() engine.ManagedView {
	return engine.ManagedView{
		Enabled: true, Consent: "per-guest", AutoAllowed: true, TagHonoured: true, Subnet: netip.MustParsePrefix("10.203.118.0/24"),
		Leg: &engine.LegView{Node: "pve1", Addr: netip.MustParseAddr("10.203.118.1"), Up: true},
		Attachments: []engine.AttachmentView{{
			Attachment: store.Attachment{Owner: "lxc/201", Identity: "mac:201", NIC: 1, MAC: "06:50:43:4f:76:02", Addr: netip.MustParseAddr("10.203.118.2"),
				Consent: "tag", Since: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)},
			Guest: &engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 201}, Name: "new-1"}, State: engine.AttachmentAttached,
		}},
		Waiting: []engine.ManagedWaiting{{
			GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: "web-1"}, Identity: "uuid:101",
			Why: "no cycle has run since the start",
		}},
		Foreign: []engine.ForeignView{},
		Free:    252,
		Vnet:    engine.VnetView{OK: true, ZoneType: "simple", IsolatePorts: true},
	}
}

func TestTheManagedNetworkIsServed(t *testing.T) {
	f := &fakeEngine{managed: testManaged()}
	rec := do(newServer(f), http.MethodGet, "/v1/managed", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireGolden(t, "managed.json", rec.Body.Bytes())
	require.Equal(t, []string{"managed"}, f.called())
}

func TestTheManagedActionsGoToTheEngineWithTheActor(t *testing.T) {
	f := &fakeEngine{}
	s := New(f, "1.2.3", []uint32{0}, zerolog.Nop())

	rec := send(s, requestFrom(0, http.MethodPost, "/v1/managed/approve", `{"owner":"lxc/201","identity":"mac:201","acceptFirewallImpact":true}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"owner":"lxc/201","identity":"uuid:101","at":"2026-03-04T05:06:07Z","acceptFirewallImpact":true}`, rec.Body.String())
	require.Equal(t, "root (cli)", engine.ActorOf(f.lastCtx()))

	rec = send(s, requestFrom(0, http.MethodPost, "/v1/managed/revoke", `{"owner":"lxc/201"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, `{}`, rec.Body.String())
	require.Equal(t, "root (cli)", engine.ActorOf(f.lastCtx()))

	rec = send(s, requestFrom(0, http.MethodPost, "/v1/managed/enable", ``))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "root (cli)", engine.ActorOf(f.lastCtx()))

	require.Equal(t, []string{"approve managed:lxc/201:mac:201:true", "revoke managed:lxc/201", "enable managed"}, f.called())
}

func TestTheManagedRequestsAreRefused(t *testing.T) {
	for _, tt := range []struct {
		name, method, target, body string
		err                        error
		status                     int
	}{
		{"an approve without a body", http.MethodPost, "/v1/managed/approve", ``, nil, http.StatusBadRequest},
		{"an approve with a field nobody knows", http.MethodPost, "/v1/managed/approve", `{"owner":"lxc/201","yes":true}`, nil, http.StatusBadRequest},
		{"a revoke the engine refuses", http.MethodPost, "/v1/managed/revoke", `{"owner":"lxc/201"}`, engine.ErrNotFound, http.StatusNotFound},
		{"an enable the engine refuses", http.MethodPost, "/v1/managed/enable", `{}`, engine.ErrRefused, http.StatusConflict},
		{"an enable with a body nobody knows", http.MethodPost, "/v1/managed/enable", `{"vnet":"x"}`, nil, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(newServer(&fakeEngine{err: tt.err}), tt.method, tt.target, tt.body)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
		})
	}
}

func TestEveryManagedMethodOfTheClientOverTheSocket(t *testing.T) {
	f := &fakeEngine{managed: testManaged()}
	socket := socketPath(t)
	serve(t, newServer(f), socket)
	c := apiclient.New(socket)
	ctx := t.Context()

	v, err := c.Managed(ctx)
	require.NoError(t, err)
	require.Equal(t, testManaged(), v)
	raw, err := c.ManagedRaw(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"enabled":true`)
	consent, err := c.ApproveManaged(ctx, "lxc/201", "mac:201", true)
	require.NoError(t, err)
	require.Equal(t, "lxc/201", consent.Owner)
	require.True(t, consent.AcceptFirewallImpact)
	require.NoError(t, c.RevokeManaged(ctx, "lxc/201"))
	require.NoError(t, c.EnableManaged(ctx))

	require.Equal(t, []string{"managed", "managed", "approve managed:lxc/201:mac:201:true", "revoke managed:lxc/201", "enable managed"}, f.called())

	f.setErr(engine.ErrRefused)
	require.ErrorIs(t, c.EnableManaged(ctx), engine.ErrRefused)
}
