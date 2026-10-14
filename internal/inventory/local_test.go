package inventory

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestTheNodesOwnIsReadOnTheNodeWhenItsAPIDoesNotAnswer(t *testing.T) {
	ctx := context.Background()
	gone := &pve.APIError{Status: 595, Message: "Connection refused"}
	api, local := newFake(), newFake()
	for _, f := range []*fakeSource{api, local} {
		f.update(refApp, func(row *pve.Resource) { row.Node = "pve1" })
	}
	for _, ref := range []model.GuestRef{refWeb, refApp, refDB} {
		api.configErrs[ref] = gone
		api.ifaceErrs[ref] = gone
	}
	api.networkErrs["pve1"] = gone
	api.networkErrs["pve2"] = gone
	local.networks["pve1"] = []pve.NodeIface{{Name: "vmbr0", Type: "bridge", Active: true}}
	src := OnNode(api, "pve1", local)

	cfg, err := src.GuestConfig(ctx, "pve1", refWeb)
	require.NoError(t, err)
	require.Equal(t, "web-1", cfg.Values["name"])
	got, err := src.AgentInterfaces(ctx, "pve1", refWeb.VMID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	_, err = src.LXCInterfaces(ctx, "pve1", refApp.VMID)
	require.NoError(t, err)
	network, err := src.NodeNetwork(ctx, "pve1")
	require.NoError(t, err)
	require.Equal(t, "vmbr0", network[0].Name)

	// Another node's own is that node's: it is not read here.
	_, err = src.GuestConfig(ctx, "pve2", refDB)
	require.ErrorIs(t, err, gone)
	_, err = src.NodeNetwork(ctx, "pve2")
	require.ErrorIs(t, err, gone)
	require.Zero(t, local.count("config "+refDB.String()))
	require.Zero(t, local.count("network pve2"))
}

func TestTheNodesOwnIsReadOnTheNodeOnlyForWhatTheAPIDidNotAnswer(t *testing.T) {
	ctx := context.Background()
	api, local := newFake(), newFake()
	api.configErrs[refWeb] = &pve.APIError{Status: 500, Message: "Configuration file 'nodes/pve1/qemu-server/101.conf' does not exist"}
	api.networkErrs["pve1"] = &pve.APIError{Status: 403, Message: "Permission check failed"}
	src := OnNode(api, "pve1", local)

	_, err := src.GuestConfig(ctx, "pve1", refWeb)
	require.True(t, pve.IsNotFound(err))
	_, err = src.NodeNetwork(ctx, "pve1")
	require.ErrorContains(t, err, "Permission check failed")
	require.Zero(t, local.count("config "+refWeb.String()))
	require.Zero(t, local.count("network pve1"))
}

func TestTheNodesOwnThatCannotBeReadAnywhereSaysWhy(t *testing.T) {
	ctx := context.Background()
	api, local := newFake(), newFake()
	api.networkErrs["pve1"] = &pve.APIError{Status: 595, Message: "Connection refused"}
	local.networkErrs["pve1"] = errors.New("pvesh: exit status 2: ipcc_send_rec failed")
	src := OnNode(api, "pve1", local)

	_, err := src.NodeNetwork(ctx, "pve1")
	require.ErrorContains(t, err, "HTTP 595")
	require.ErrorContains(t, err, "read on this node instead: pvesh: exit status 2: ipcc_send_rec failed")
}
