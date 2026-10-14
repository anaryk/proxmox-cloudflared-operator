//go:build linux

package daemon

import (
	"context"
	"fmt"

	"github.com/vishvananda/netlink"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// watchLinksAndRoutes calls changed for every change of a link or an IPv4
// route of the node, until ctx ends: a leg's port that goes, a bridge that
// ifreload makes again, the routes of the service prefix taken away. A
// message the library skips, such as one from a sender other than the
// kernel, ends nothing; skipped is told of the first. It returns an error
// when it cannot subscribe, or when a subscription ends.
func watchLinksAndRoutes(ctx context.Context, changed func(), skipped func(error)) error {
	done := make(chan struct{})
	defer close(done)
	linkErrs, routeErrs := gateway.NewSubscriptionErrors(skipped), gateway.NewSubscriptionErrors(skipped)
	links := make(chan netlink.LinkUpdate, 64)
	if err := netlink.LinkSubscribeWithOptions(links, done, netlink.LinkSubscribeOptions{ErrorCallback: linkErrs.Report}); err != nil {
		return fmt.Errorf("watching the links: %w", err)
	}
	routes := make(chan netlink.RouteUpdate, 64)
	if err := netlink.RouteSubscribeWithOptions(routes, done, netlink.RouteSubscribeOptions{ErrorCallback: routeErrs.Report}); err != nil {
		return fmt.Errorf("watching the routes: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-links:
			if !ok {
				return linkErrs.Ended("watching the links")
			}
			changed()
		case _, ok := <-routes:
			if !ok {
				return routeErrs.Ended("watching the routes")
			}
			changed()
		}
	}
}
