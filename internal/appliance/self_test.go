package appliance

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// selfFixture is the self-identification of lxc/9250 in a process that
// started as incB, over a store whose leader.json was written at incA, with
// the facts and the uptimes the test changes.
type selfFixture struct {
	self      *Self
	st        *store.Store
	facts     Facts
	factsErr  error
	uptimes   map[model.GuestRef]time.Duration
	verifyErr error
}

const (
	incA = bootA + "/100"
	incB = bootA + "/200"
)

func newSelf(t *testing.T) *selfFixture {
	t.Helper()
	f := &selfFixture{
		st:      newStore(t),
		facts:   facts(byMount, time.Hour, macA),
		uptimes: upFor(time.Hour),
	}
	require.NoError(t, f.st.SaveWriter(planner.Writer{InstallID: "abc123", Generation: 5, Nonce: "n5", Incarnation: incA}))
	f.self = &Self{
		ID: me, InstallID: "abc123", Incarnation: incB, Store: f.st,
		Facts:       func() (Facts, error) { return f.facts, f.factsErr },
		Uptimes:     func(context.Context) (map[model.GuestRef]time.Duration, error) { return f.uptimes, nil },
		VerifyError: func() error { return f.verifyErr },
		Endpoint:    store.Endpoint{Address: "10.92.0.1:8006", ServerName: "pve1"},
		Flag:        filepath.Join(t.TempDir(), "pco-appliance", "identity-ok"),
		Rand:        zeros(),
		Now:         func() time.Time { return t0 },
	}
	return f
}

func (f *selfFixture) writer(t *testing.T) planner.Writer {
	t.Helper()
	w, _, err := f.st.Writer()
	require.NoError(t, err)
	return w
}

func TestTheFirstPassDrawsTheEpochAndNoLaterOne(t *testing.T) {
	f := newSelf(t)
	own := snap(lxc(ownVMID, Pool, macA))

	v := f.self.Check(t.Context(), own)

	require.True(t, v.OK)
	require.True(t, f.self.EpochDrawn())
	drawn := planner.Writer{InstallID: "abc123", Generation: 6, Nonce: "aaaaaaaa", Incarnation: incB}
	require.Equal(t, drawn, f.writer(t))
	require.NoFileExists(t, f.self.Flag, "the flag is the engine's to write")

	require.NoError(t, f.st.SaveWriter(planner.Writer{InstallID: "abc123", Generation: 9, Nonce: "n9", Incarnation: incA}))
	require.True(t, f.self.Check(t.Context(), own).OK)
	require.Equal(t, 9, f.writer(t).Generation, "decided once per process")
}

func TestACopyDrawsNothingAndLosesTheFlag(t *testing.T) {
	f := newSelf(t)
	require.NoError(t, f.self.SetFlag(true))
	f.facts = facts(byOther, time.Hour, macA)

	for range 3 {
		v := f.self.Check(t.Context(), snap(lxc(ownVMID, Pool, macA)))
		require.True(t, v.Copy)
	}

	require.NoFileExists(t, f.self.Flag)
	require.False(t, f.self.EpochDrawn())
	require.Equal(t, incA, f.writer(t).Incarnation)
	require.Equal(t, 5, f.writer(t).Generation)
}

func TestAVerdictThatIsNotOKDrawsNothing(t *testing.T) {
	f := newSelf(t)
	f.uptimes = nil

	v := f.self.Check(t.Context(), snap(lxc(ownVMID, Pool, macA)))

	require.False(t, v.OK)
	require.False(t, v.Copy)
	require.False(t, f.self.EpochDrawn())
	require.Equal(t, 5, f.writer(t).Generation)
}

func TestTheIncarnationOfTheStartThatWroteLeaderJSONKeepsTheEpoch(t *testing.T) {
	f := newSelf(t)
	f.self.Incarnation = incA

	require.True(t, f.self.Check(t.Context(), snap(lxc(ownVMID, Pool, macA))).OK)

	require.False(t, f.self.EpochDrawn())
	require.Equal(t, 5, f.writer(t).Generation)
}

func TestAnEpochThatCannotBeDecidedIsNotOK(t *testing.T) {
	f := newSelf(t)
	require.NoError(t, f.st.SaveWriter(planner.Writer{InstallID: "other1", Generation: 5, Nonce: "n5"}))

	v := f.self.Check(t.Context(), snap(lxc(ownVMID, Pool, macA)))

	require.False(t, v.OK)
	require.Contains(t, v.Why, "this container is lxc/9250, but its writer epoch could not be decided: leader.json names install other1")
	require.False(t, f.self.EpochDrawn())
}

func TestFactsThatCannotBeReadAreNotOK(t *testing.T) {
	f := newSelf(t)
	f.factsErr = errors.New("no /proc")

	v := f.self.Check(t.Context(), snap(lxc(ownVMID, Pool, macA)))

	require.Equal(t, Verdict{Why: "the facts of this container could not be read: no /proc"}, v)
}

// Failure mode 3: the certificate of the API stopped verifying.
func TestACertificateThatNoLongerVerifiesIsWhy(t *testing.T) {
	f := newSelf(t)
	f.verifyErr = errors.New("x509: certificate signed by unknown authority")
	f.uptimes = nil

	v := f.self.Check(t.Context(), inventory.Snapshot{Guests: []model.Guest{lxc(ownVMID, Pool, macA)}})

	require.Equal(t, "the certificate of 10.92.0.1:8006 no longer verifies under pve1 (the cluster CA or the pveproxy certificate changed?): "+
		"run pco appliance repair --vmid 9250 on the node", v.Why)
	require.False(t, v.Copy)

	f.facts = facts(byOther, time.Hour, macA)
	require.Contains(t, f.self.Check(t.Context(), inventory.Snapshot{}).Why, "this container is a copy", "a copy stays one")
}

func TestTheSegmentsComeFromTheLastCompleteSnapshot(t *testing.T) {
	f := newSelf(t)
	require.Empty(t, f.self.Segments(), "nothing before the first snapshot")

	own := lxc(ownVMID, Pool)
	own.NICs = []model.NIC{{Index: 0, MAC: macA, Bridge: "vmbr0", VLAN: 10}}
	f.self.Check(t.Context(), snap(own))
	require.Equal(t, map[string]resolve.Segment{"eth0": {Bridge: "vmbr0", VLAN: 10}}, f.self.Segments())

	own.NICs[0].VLAN = 20
	f.self.Check(t.Context(), inventory.Snapshot{Guests: []model.Guest{own}})
	require.Equal(t, map[string]resolve.Segment{"eth0": {Bridge: "vmbr0", VLAN: 10}}, f.self.Segments(), "an incomplete one changes nothing")
}

func TestBeforeTheFirstCycleOnlyAMountOfAnotherVMIDDecides(t *testing.T) {
	for _, tt := range []struct {
		name  string
		mount Source
		copy  bool
	}{
		{name: "another VMID", mount: byOther, copy: true},
		{name: "the recorded VMID", mount: byMount},
		{name: "an unknown form", mount: unknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newSelf(t)
			require.NoError(t, f.self.SetFlag(true))
			f.facts = facts(tt.mount, time.Hour, macA)

			why, isCopy := f.self.CopyBeforeCycle()

			require.Equal(t, tt.copy, isCopy)
			require.Equal(t, 5, f.writer(t).Generation, "no epoch")
			require.False(t, f.self.EpochDrawn())
			if tt.copy {
				require.Contains(t, why, "a volume of VMID 9295")
				require.NoFileExists(t, f.self.Flag)
			} else {
				require.Empty(t, why)
				require.FileExists(t, f.self.Flag, "left as it was")
			}
		})
	}
}
