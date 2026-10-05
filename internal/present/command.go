package present

import (
	"regexp"

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

// ArgForm is the pattern a value of kind has to match; empty for a kind that
// has none.
func ArgForm(kind string) string {
	if re, ok := argForms[kind]; ok {
		return re.String()
	}
	return ""
}

// CommandArg returns v when it has the form of its kind, else "" and the
// refusal "the <kind> has an unexpected form". A command offered to be copied
// into a root shell is made only of values that passed it: what the daemon
// says comes in part from guests and from Cloudflare.
func CommandArg(kind, v string) (string, string) {
	if re, ok := argForms[kind]; ok && re.MatchString(v) {
		return v, ""
	}
	return "", "the " + kind + " has an unexpected form"
}

// RotateCommand is the command that rotates the secret of the tunnel in
// account, or, when the account id has another form or the daemon would
// refuse the rotation, no command and why.
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
