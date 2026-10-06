//go:build !linux

package daemon

import (
	"context"
	"errors"
)

// watchLinksAndRoutes needs the netlink of Linux.
func watchLinksAndRoutes(context.Context, func()) error {
	return errors.New("watching the links and routes needs Linux")
}
