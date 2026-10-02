package daemon

import (
	"errors"
	"os/user"
	"strconv"

	"github.com/rs/zerolog"
)

// webName is the user and the group of the web UI, which may use the socket
// when they exist.
const webName = "pco-web"

// Accounts looks up users and groups by name. A name nobody has is an error of
// the types os/user has for it.
type Accounts interface {
	LookupUser(name string) (*user.User, error)
	LookupGroup(name string) (*user.Group, error)
}

type systemAccounts struct{}

func (systemAccounts) LookupUser(name string) (*user.User, error)   { return user.Lookup(name) }
func (systemAccounts) LookupGroup(name string) (*user.Group, error) { return user.LookupGroup(name) }

// socketAccess returns the group that owns the socket and the users whose
// requests are answered. The group is pco-web when there is one, else the
// group of root; the users are root and pco-web when there is one. A user or a
// group that does not exist is the normal case of a node without the web UI.
func socketAccess(a Accounts, log zerolog.Logger) (gid int, uids []uint32) {
	gid, uids = 0, []uint32{0}

	g, err := a.LookupGroup(webName)
	switch {
	case err == nil:
		if n, perr := strconv.Atoi(g.Gid); perr == nil {
			gid = n
		} else {
			log.Warn().Str("group", webName).Str("gid", g.Gid).Msg("the group id is not a number; the socket stays with the group of root")
		}
	case !isUnknownGroup(err):
		log.Warn().Err(err).Str("group", webName).Msg("looking up the group failed; the socket stays with the group of root")
	}

	u, err := a.LookupUser(webName)
	switch {
	case err == nil:
		if n, perr := strconv.ParseUint(u.Uid, 10, 32); perr == nil {
			uids = append(uids, uint32(n))
		} else {
			log.Warn().Str("user", webName).Str("uid", u.Uid).Msg("the user id is not a number; only root may use the socket")
		}
	case !isUnknownUser(err):
		log.Warn().Err(err).Str("user", webName).Msg("looking up the user failed; only root may use the socket")
	}
	return gid, uids
}

func isUnknownUser(err error) bool {
	var unknown user.UnknownUserError
	return errors.As(err, &unknown)
}

func isUnknownGroup(err error) bool {
	var unknown user.UnknownGroupError
	return errors.As(err, &unknown)
}
