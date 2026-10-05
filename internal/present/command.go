package present

import (
	"regexp"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// The kinds of value a command for a root shell may carry.
const (
	ArgAccount  = "account id"
	ArgOwner    = "owner"
	ArgHostname = "hostname"
)

// argForms are the forms of the kinds. The patterns mean the same in
// JavaScript, which the web interface reads them in.
var argForms = map[string]*regexp.Regexp{
	ArgAccount:  regexp.MustCompile(`^[0-9a-f]{32}$`),
	ArgOwner:    regexp.MustCompile(`^(qemu|lxc)/[0-9]+$`),
	ArgHostname: regexp.MustCompile(`^([a-z0-9-]{1,63}\.)+[a-z0-9-]{2,63}$`),
}

// noForm is the pattern of a kind that has none. It matches nothing, so that
// wherever it is used a value of that kind is refused.
const noForm = `[^\s\S]`

// ArgForm is the pattern a value of kind has to match, or one that matches
// nothing for a kind that has none.
func ArgForm(kind string) string {
	if re, ok := argForms[kind]; ok {
		return re.String()
	}
	return noForm
}

// CommandArg returns v when it has the form of its kind, else "" and why not:
// "the <kind> begins with a dash, which a command would take for an option",
// whatever the kind, or "the <kind> has an unexpected form". A command offered
// to be copied into a root shell is made only of values that passed it: what
// the daemon says comes in part from guests and from Cloudflare.
func CommandArg(kind, v string) (string, string) {
	if strings.HasPrefix(v, "-") {
		return "", "the " + kind + " begins with a dash, which a command would take for an option"
	}
	if re, ok := argForms[kind]; ok && re.MatchString(v) {
		return v, ""
	}
	return "", "the " + kind + " has an unexpected form"
}

// RotateCommand is the command that rotates the secret of the tunnel in
// account, or, when the account id has another form or engine.RotationTarget
// refuses the tunnel, no command and why. It does not look at the mode: in
// observe-only mode the daemon refuses the rotation when the command runs. A
// command shown is no promise that the daemon carries it out; the daemon
// decides when it is asked.
func RotateCommand(tunnels []engine.TunnelView, account string) (cmd, refused string) {
	id, refused := CommandArg(ArgAccount, account)
	if refused != "" {
		return "", refused
	}
	if _, err := engine.RotationTarget(tunnels, id); err != nil {
		return "", err.Error()
	}
	return "pco tunnel rotate --account " + id, ""
}
