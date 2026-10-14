package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An attachment the inventory does not back with the guest's card, at its
// index on the managed network with its MAC, gives no sticky entry: the port
// it names would hold another card's MAC.
func TestAnAttachmentWithoutItsCardPinsNoPort(t *testing.T) {
	w, _ := legWorld(t)
	atts, err := w.store.Attachments()
	require.NoError(t, err)
	a := atts[mgdRef.String()]
	a.NIC = 3
	require.NoError(t, w.store.SaveAttachment(a))

	w.settle()

	require.Empty(t, w.networksOf(t, "pve2").Sticky)
}

// A service whose address behind its leg has no MAC pinned forwards
// nothing: any card could answer there.
func TestAServiceWithoutAPinnedMACForwardsNothing(t *testing.T) {
	e := newLegEnv(t)
	e.res.answerFrom(legHost, "not a MAC")
	e.warm()
	e.next()

	require.Empty(t, e.gw.last(t).services)
	require.Empty(t, e.gw.last(t).pins)
}
