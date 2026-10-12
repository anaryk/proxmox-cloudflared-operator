package gateway

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// A reader who sees lxc/201 and lxc/205 gets of the managed network their
// cards and nothing of the others, and no MAC, identity, pveum line, pool
// name or firewall detail anywhere in it.
func TestAReaderSeesNoSecretOfTheManagedNetwork(t *testing.T) {
	st := populated(t)
	// A foreign card whose reason names the MAC of the leg, and a detail
	// that names a MAC.
	st.Managed.Foreign = append(st.Managed.Foreign, engine.ForeignView{
		GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 201}, Name: "new-1"}, NIC: 2, MAC: "02:50:43:4f:ff:01",
		Action: engine.ForeignLinkDown, Why: "a card on pconet with the MAC of pco's leg (02:50:43:4f:ff:01); pco sets its link down",
	})
	for i := range st.Managed.Attachments {
		if st.Managed.Attachments[i].Owner == "lxc/201" {
			st.Managed.Attachments[i].Detail = "adding net1 to lxc/201: the card 06:50:43:4f:76:02 is net3 already"
		}
	}
	st.Managed.Waiting = append(st.Managed.Waiting, engine.ManagedWaiting{
		GuestView: engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindLXC, VMID: 205}, Name: "new-1-clone"}, Identity: "mac:205",
		Why:       "needs-grant: pveum acl modify /vms/205 --users pco@pve --tokens 'pco@pve!vm9250' --roles PCOManaged",
	})

	got := FilterState(st, sees(201, 205)).Managed

	owners := func() []string {
		var out []string
		for _, a := range got.Attachments {
			out = append(out, "attached "+a.Owner)
		}
		for _, w := range got.Waiting {
			out = append(out, "waiting "+w.String())
		}
		for _, f := range got.Foreign {
			out = append(out, "foreign "+f.String())
		}
		return out
	}
	require.Equal(t, []string{"attached lxc/201", "waiting lxc/205", "foreign lxc/205", "foreign lxc/201"}, owners(),
		"the guests the reader may audit, and no other")
	require.Equal(t, "needs-grant", got.Waiting[0].Why, "a pveum line is cut to its name")
	require.Equal(t, "a card on pconet with the MAC of pco's leg (a MAC); pco sets its link down", got.Foreign[1].Why)
	require.Equal(t, "adding net1 to lxc/201: the card a MAC is net3 already", got.Attachments[0].Detail)
	require.Nil(t, got.Filtered)
	require.Empty(t, got.PoolGrant)

	data, err := json.Marshal(got)
	require.NoError(t, err)
	text := string(data)
	require.NotRegexp(t, regexp.MustCompile(`(?i)[0-9a-f]{2}(:[0-9a-f]{2}){5}`), text, "no MAC anywhere")
	for _, secret := range []string{"pveum", "mac:201", "mac:205", "uuid:", "PCOManaged", `"poolGrant"`, `"firewall"`, `"filtered"`, `"grantCmd"`, "firewall=1"} {
		require.NotContains(t, text, secret)
	}
	require.True(t, strings.Contains(text, `"identity":""`), "the identities are blank")
}

// The reader's view of the managed network cuts the confirmation reason to
// its name too, and leaves the operational reasons as they are.
func TestAReaderGetsTheReasonsOfTheManagedNetworkWithoutCommands(t *testing.T) {
	st := populated(t)
	got := FilterState(st, sees(204, 109)).Managed

	whys := map[int]string{}
	for _, w := range got.Waiting {
		whys[w.VMID] = w.Why
	}
	require.Equal(t, map[int]string{204: "needs-firewall-confirmation", 109: "guest is on node pve2, not pve1"}, whys)
}

func TestAReaderGetsTheManagedEventsWithoutMACs(t *testing.T) {
	events := []engine.Event{
		{Seq: 1, Kind: "managed", Subject: "lxc/201", Guest: "lxc/201", Message: "foreign card net2 (02:50:43:4f:ff:01) of lxc/201 was set link down: the MAC of pco's leg (02:50:43:4f:ff:01)"},
		{Seq: 2, Kind: "managed", Subject: "pconet", Message: "a frame from bc:24:11:00:99:99, a MAC no lease knows, was seen on the managed network; the guests of pve1 are read again"},
		{Seq: 3, Kind: "managed", Subject: "lxc/202", Guest: "lxc/202", Message: "lxc/202 is attached to the managed network: net1 at 10.203.118.3, by tag"},
		{Seq: 4, Kind: "admin", Subject: "lxc/201", Message: "lxc/201 (new-1) is approved for the managed network in identity mac:201", Actor: "alice@pve (ticket)"},
	}

	got := FilterEvents(events, sees(201), map[string]bool{})

	require.Len(t, got, 3, "the event of the hidden guest is gone")
	require.Equal(t, "foreign card net2 (a MAC) of lxc/201 was set link down: the MAC of pco's leg (a MAC)", got[0].Message)
	require.Equal(t, "a frame from a MAC, a MAC no lease knows, was seen on the managed network; the guests of pve1 are read again", got[1].Message)
	require.Equal(t, uint64(4), got[2].Seq)
	require.Equal(t, events[0].Message, "foreign card net2 (02:50:43:4f:ff:01) of lxc/201 was set link down: the MAC of pco's leg (02:50:43:4f:ff:01)",
		"the events given are left as they were")
}
