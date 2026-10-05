package daemon

import (
	"errors"
	"os"
	"os/user"
	"strconv"

	"github.com/rs/zerolog"
)

// webName is the user and the group of the web UI, which may use the socket
// when they exist and the web UI is installed.
const webName = "pco-web"

// webUnit is the unit of the web UI, which its package installs.
const webUnit = "/usr/lib/systemd/system/pco-web.service"

// Accounts looks up users and groups by name, and says whether the web UI is
// installed: its unit file is there. A name nobody has is an error of the
// types os/user has for it.
type Accounts interface {
	LookupUser(name string) (*user.User, error)
	LookupGroup(name string) (*user.Group, error)
	WebInstalled() bool
}

type systemAccounts struct{}

func (systemAccounts) LookupUser(name string) (*user.User, error)   { return user.Lookup(name) }
func (systemAccounts) LookupGroup(name string) (*user.Group, error) { return user.LookupGroup(name) }
func (systemAccounts) WebInstalled() bool                           { return unitInstalled(webUnit) }

func unitInstalled(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// socketAccess returns the group that owns the socket, the users whose
// requests are answered and the user of the web UI, 0 for none. The group is
// pco-web when there is one, else the group of root; the users are root and
// pco-web when there is one. A user or a group that does not exist is the
// normal case of a node without the web UI. Without the unit of the web UI
// the name means nothing: anyone who may add a user could have made it, and
// only root is answered.
func socketAccess(a Accounts, log zerolog.Logger) (gid int, uids []uint32, web uint32) {
	gid, uids = 0, []uint32{0}
	if !a.WebInstalled() {
		_, uerr := a.LookupUser(webName)
		_, gerr := a.LookupGroup(webName)
		if uerr == nil || gerr == nil {
			log.Warn().Str("user", webName).Msg("the user or the group " + webName + " exists, but the web UI is not installed (no " +
				webUnit + "); it may not use the socket")
		}
		return gid, uids, 0
	}

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
		n, perr := strconv.ParseUint(u.Uid, 10, 32)
		switch {
		case perr != nil:
			log.Warn().Str("user", webName).Str("uid", u.Uid).Msg("the user id is not a number; only root may use the socket")
		case n == 0:
			log.Warn().Str("user", webName).Msg("the user " + webName + " is root; only root may use the socket, as the command line")
		default:
			web = uint32(n)
			uids = append(uids, web)
		}
	case !isUnknownUser(err):
		log.Warn().Err(err).Str("user", webName).Msg("looking up the user failed; only root may use the socket")
	}
	return gid, uids, web
}

func isUnknownUser(err error) bool {
	var unknown user.UnknownUserError
	return errors.As(err, &unknown)
}

func isUnknownGroup(err error) bool {
	var unknown user.UnknownGroupError
	return errors.As(err, &unknown)
}
