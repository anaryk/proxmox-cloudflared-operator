package egress

import (
	"os/user"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnectorUIDIsLookedUpByName(t *testing.T) {
	var asked string
	uid, err := connectorUID(func(name string) (*user.User, error) {
		asked = name
		return &user.User{Username: name, Uid: "996"}, nil
	})

	require.NoError(t, err)
	require.Equal(t, uint32(996), uid)
	require.Equal(t, "pco-connector", asked)
}

func TestAMissingConnectorUserSaysHowToCreateIt(t *testing.T) {
	_, err := connectorUID(func(name string) (*user.User, error) { return nil, user.UnknownUserError(name) })

	require.ErrorIs(t, err, ErrNoConnectorUser)
	require.EqualError(t, err, "pco-connector: the connector user does not exist: install the package or run systemd-sysusers")
}

func TestAConnectorUserThatCannotBeUsedIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		u    *user.User
		err  error
		want string
	}{
		"root":       {u: &user.User{Uid: "0"}, want: "uid 0"},
		"not a uid":  {u: &user.User{Uid: "S-1-5-18"}, want: "not a number"},
		"too large":  {u: &user.User{Uid: "4294967296"}, want: "not a number"},
		"lookup err": {err: errBoom, want: "boom"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := connectorUID(func(string) (*user.User, error) { return tc.u, tc.err })

			require.ErrorContains(t, err, tc.want)
		})
	}
}
