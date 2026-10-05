//go:build webui

// Package ui holds the web interface that pco web serves.
package ui

import (
	"embed"
	"io/fs"
)

// Built says whether this binary carries the interface.
const Built = true

// all: keeps a chunk whose name starts with an underscore.
//
//go:embed all:dist
var dist embed.FS

// Assets is the interface as Vite built it into dist by make ui.
func Assets() fs.FS {
	// fs.Sub fails only on a name that is not valid, and "dist" is.
	sub, _ := fs.Sub(dist, "dist")
	return sub
}
