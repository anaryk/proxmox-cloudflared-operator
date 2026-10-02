package egress

import (
	"errors"
	"fmt"
	"os/user"
	"strconv"
)

// ConnectorUser is the system user the connectors run as, which the package
// creates through sysusers.d. Its uid differs from node to node, so it is
// looked up by name whenever it is needed.
const ConnectorUser = "pco-connector"

// ConnectorUID returns the uid of the connector user.
func ConnectorUID() (uint32, error) { return connectorUID(user.Lookup) }

func connectorUID(lookup func(string) (*user.User, error)) (uint32, error) {
	u, err := lookup(ConnectorUser)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return 0, fmt.Errorf("user %s does not exist: install the pco package or run systemd-sysusers", ConnectorUser)
	}
	if err != nil {
		return 0, fmt.Errorf("looking up user %s: %w", ConnectorUser, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("user %s has uid %q, which is not a number", ConnectorUser, u.Uid)
	}
	if uid == 0 {
		return 0, fmt.Errorf("user %s has uid 0: the filter would confine every process of root", ConnectorUser)
	}
	return uint32(uid), nil
}
