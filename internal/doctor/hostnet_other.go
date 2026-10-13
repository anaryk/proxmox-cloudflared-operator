//go:build !linux

package doctor

import "errors"

// netnsPinned has no namespace to look at off Linux.
func netnsPinned(string) error { return errors.New("network namespaces are Linux only") }
