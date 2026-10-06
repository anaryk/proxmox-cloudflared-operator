package agentproto

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	remembered := epoch7()
	const applied = 42
	snap := func(change func(*Snapshot)) Snapshot {
		s := Snapshot{Protocol: 1, Epoch: epoch7(), Seq: 43, Node: "node2", State: Active}
		change(&s)
		return s
	}
	cases := []struct {
		name       string
		s          Snapshot
		remembered Epoch
		want       error
	}{
		{"the next seq of the epoch", snap(func(*Snapshot) {}), remembered, nil},
		{"the same seq again", snap(func(s *Snapshot) { s.Seq = applied }), remembered, nil},
		{"a higher epoch", snap(func(s *Snapshot) { s.Epoch.Generation, s.Epoch.Nonce = 8, "zz9" }), remembered, nil},
		{"a higher epoch starts its seq again", snap(func(s *Snapshot) { s.Epoch.Generation, s.Epoch.Nonce, s.Seq = 9, "zz9", 1 }), remembered, nil},
		{"the first epoch an agent hears", snap(func(s *Snapshot) { s.Epoch.Generation, s.Seq = 1, 0 }), Epoch{InstallID: "k3x9q2"}, nil},
		{"a generation of 2^53", snap(func(s *Snapshot) { s.Epoch.Generation = MaxGeneration }), remembered, nil},
		{"another node", snap(func(s *Snapshot) { s.Node = "node3" }), remembered, ErrOtherNode},
		{"another install", snap(func(s *Snapshot) { s.Epoch.InstallID = "zz77aa" }), remembered, ErrOtherInstall},
		{"a lower epoch", snap(func(s *Snapshot) { s.Epoch.Generation, s.Seq = 6, 900 }), remembered, ErrLowerEpoch},
		{"the generation with another nonce", snap(func(s *Snapshot) { s.Epoch.Nonce = "e5f6a7b8" }), remembered, ErrOtherNonce},
		{"a generation above 2^53", snap(func(s *Snapshot) { s.Epoch.Generation = MaxGeneration + 1 }), remembered, ErrGenerationBound},
		{"a seq that goes back", snap(func(s *Snapshot) { s.Seq = applied - 1 }), remembered, ErrSeqBack},
		{"a protocol this build does not speak", snap(func(s *Snapshot) { s.Protocol = ProtocolMax + 1 }), remembered, ErrProtocol},
		{"no protocol", snap(func(s *Snapshot) { s.Protocol = 0 }), remembered, ErrProtocol},
		{"no generation", snap(func(s *Snapshot) { s.Epoch.Generation = 0 }), Epoch{InstallID: "k3x9q2"}, ErrBadEpoch},
		{"no nonce", snap(func(s *Snapshot) { s.Epoch.Nonce = "" }), remembered, ErrBadEpoch},
		{"a nonce of other characters", snap(func(s *Snapshot) { s.Epoch.Nonce = "Nonce.b2" }), remembered, ErrBadEpoch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.Validate("node2", c.remembered, applied)
			if c.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, c.want)
		})
	}
}

func TestValidateNeedsTheInstallOfTheAgent(t *testing.T) {
	s := Snapshot{Protocol: 1, Epoch: epoch7(), Seq: 1, Node: "node2"}
	require.Error(t, s.Validate("node2", Epoch{}, 0))
}

func TestValidateSaysWhatItRefused(t *testing.T) {
	s := Snapshot{Protocol: 1, Epoch: Epoch{InstallID: "k3x9q2", Generation: 6, Nonce: "ff00"}, Node: "node2"}
	err := s.Validate("node2", epoch7(), 0)
	require.ErrorIs(t, err, ErrLowerEpoch)
	require.ErrorContains(t, err, "6.ff00")
	require.ErrorContains(t, err, "7.a1b2c3d4")
}
