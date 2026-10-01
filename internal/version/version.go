// Package version holds build metadata injected at link time.
package version

// Set with -ldflags "-X" by the Makefile; the defaults apply to plain go builds.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
