package store

import (
	"path/filepath"
	"strings"
)

// frozenSchema is the schema version of the frozen objects: leader.json and
// the entries of the node registry. Every build of a cluster must be able to
// tell who holds the lease while the controller moves from one build to
// another, so these two never change their version, gain fields only and are
// read leniently. Every other object has the global schemaVersion.
const frozenSchema = 1

// frozen reports whether the object of kind whose file is named stem, without
// its extension, is a frozen one.
func frozen(kind, stem string) bool {
	return kind == kindNodes || kind == kindMeta && stem == idLeader
}

// schemaAt is the schema version the object in the file at path is written
// and read with. A file of the store is <root>/<kind>/<stem>.json.
func schemaAt(path string) int {
	kind := filepath.Base(filepath.Dir(path))
	if frozen(kind, strings.TrimSuffix(filepath.Base(path), objectExt)) {
		return frozenSchema
	}
	return schemaVersion
}
