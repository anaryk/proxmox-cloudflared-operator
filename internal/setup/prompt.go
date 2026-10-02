package setup

// Prompter asks the operator; a non-interactive implementation answers from
// Options.
//
// Info and Warn take a format of setup's own and arguments that may carry
// text of others, such as the output of a command or a message of Cloudflare:
// an implementation that writes to a terminal cleans the arguments of what a
// terminal acts on.
type Prompter interface {
	Confirm(question string, def bool) (bool, error)
	Secret(question string) (string, error) // no echo
	Info(format string, args ...any)
	Warn(format string, args ...any)
}
