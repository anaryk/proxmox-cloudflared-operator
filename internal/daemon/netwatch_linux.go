//go:build linux

package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// watchLinksAndRoutes calls changed for every change of a link or an IPv4
// route of the node, until ctx ends: a leg's port that goes, a bridge that
// ifreload makes again, the routes of the service prefix taken away. It
// returns an error when it cannot subscribe, or when a subscription ends.
func watchLinksAndRoutes(ctx context.Context, changed func()) error {
	done := make(chan struct{})
	defer close(done)
	failed := make(chan error, 1)
	fail := func(err error) {
		select {
		case failed <- err:
		default:
		}
	}
	links := make(chan netlink.LinkUpdate, 64)
	if err := netlink.LinkSubscribeWithOptions(links, done, netlink.LinkSubscribeOptions{ErrorCallback: fail}); err != nil {
		return fmt.Errorf("watching the links: %w", err)
	}
	routes := make(chan netlink.RouteUpdate, 64)
	if err := netlink.RouteSubscribeWithOptions(routes, done, netlink.RouteSubscribeOptions{ErrorCallback: fail}); err != nil {
		return fmt.Errorf("watching the routes: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			return fmt.Errorf("watching the links and routes: %w", err)
		case _, ok := <-links:
			if !ok {
				return errors.New("watching the links: the subscription ended")
			}
			changed()
		case _, ok := <-routes:
			if !ok {
				return errors.New("watching the routes: the subscription ended")
			}
			changed()
		}
	}
}
