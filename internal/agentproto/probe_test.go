package agentproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAProbeNamesABridgeAndAnIPv4Address(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe Probe
		err   string
	}{
		{name: "a bridge", probe: Probe{Bridge: "vmbr1"}},
		{name: "a VLAN and an address", probe: Probe{Bridge: "vmbr1", VLAN: 20, Addr: netip.MustParseAddr("10.77.0.250")}},
		{name: "no bridge", probe: Probe{}, err: "no bridge"},
		{name: "a bridge of other characters", probe: Probe{Bridge: "vmbr1;reboot"}, err: "bridge"},
		{name: "a bridge too long", probe: Probe{Bridge: strings.Repeat("b", 16)}, err: "bridge"},
		{name: "a VLAN out of range", probe: Probe{Bridge: "vmbr1", VLAN: 4095}, err: "VLAN"},
		{name: "an IPv6 address", probe: Probe{Bridge: "vmbr1", Addr: netip.MustParseAddr("fd00::1")}, err: "IPv4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.probe.Check()
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestAProbeAnswerIsHeldToItsBounds(t *testing.T) {
	iface := ProbeIface{Name: "vmbr1", Bridge: "vmbr1", Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/24")}}
	many := func(n int) []ProbeIface {
		out := make([]ProbeIface, n)
		for i := range out {
			out[i] = ProbeIface{Name: fmt.Sprintf("eth%d", i)}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		answer ProbeAnswer
		err    string
	}{
		{name: "an answer", answer: ProbeAnswer{Interfaces: []ProbeIface{iface}, AnsweredBy: []string{"bc:24:11:00:00:01"}}},
		{name: "too many interfaces", answer: ProbeAnswer{Interfaces: many(MaxProbeIfaces + 1)}, err: "interfaces"},
		{name: "too many addresses", answer: ProbeAnswer{Interfaces: []ProbeIface{{Name: "vmbr1", Addrs: make([]netip.Prefix, MaxProbeAddrs+1)}}}, err: "addresses"},
		{name: "too many answers", answer: ProbeAnswer{AnsweredBy: make([]string, MaxProbeAnswers+1)}, err: "answered"},
		{name: "a MAC that is none", answer: ProbeAnswer{AnsweredBy: []string{"nobody"}}, err: "MAC"},
		{name: "a name of other characters", answer: ProbeAnswer{Interfaces: []ProbeIface{{Name: "eth0 up"}}}, err: "interface"},
		{name: "a bridge of other characters", answer: ProbeAnswer{Interfaces: []ProbeIface{{Name: "eth0", Master: "a/b"}}}, err: "interface"},
		{name: "a VLAN out of range", answer: ProbeAnswer{Interfaces: []ProbeIface{{Name: "vmbr1.5000", Bridge: "vmbr1", VLAN: 5000}}}, err: "VLAN"},
		{name: "a note too long", answer: ProbeAnswer{NotProbed: strings.Repeat("x", maxProbeText+1)}, err: "long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.answer.Check()
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestDecodeProbeAnswerRefusesWhatIsOverItsBounds(t *testing.T) {
	good, err := json.Marshal(ProbeAnswer{AnsweredBy: []string{"bc:24:11:00:00:01"}})
	require.NoError(t, err)
	got, err := DecodeProbeAnswer(good)
	require.NoError(t, err)
	require.Equal(t, []string{"bc:24:11:00:00:01"}, got.AnsweredBy)

	_, err = DecodeProbeAnswer(bytes.Repeat([]byte(" "), MaxProbeAnswerBytes+1))
	require.ErrorIs(t, err, ErrTooLarge)
	_, err = DecodeProbeAnswer([]byte(`{"answeredBy":["nobody"]}`))
	require.ErrorContains(t, err, "MAC")
	_, err = DecodeProbeAnswer([]byte(`{"answeredBy":[]} {}`))
	require.Error(t, err, "nothing may follow the answer")
}

func TestAStatusSaysAtMostOneLegOfEachMark(t *testing.T) {
	st := Status{Legs: make([]LegReport, MaxLegReports+1)}
	require.ErrorIs(t, CheckStatusBounds(st), ErrTooLarge)
	st.Legs = st.Legs[:MaxLegReports]
	require.NoError(t, CheckStatusBounds(st))
}
