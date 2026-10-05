//go:build !webui

// Package ui holds the web interface that pco web serves.
package ui

import (
	"embed"
	"io/fs"
)

// Built says whether this binary carries the interface.
const Built = false

//go:embed none
var none embed.FS

// Assets is one page that says "This build of pco has no web interface. Build
// it with make build UI=1."
func Assets() fs.FS {
	// fs.Sub fails only on a name that is not valid, and "none" is.
	sub, _ := fs.Sub(none, "none")
	return sub
}
