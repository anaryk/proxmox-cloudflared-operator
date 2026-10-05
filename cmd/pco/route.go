package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func (a *app) routeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Make and remove the routes an admin writes by hand",
	}
	manual := &cobra.Command{
		Use:   "manual",
		Short: "Manual routes: hostnames published to a guest or an address without its Notes",
	}
	manual.AddCommand(a.manualListCmd(), a.manualAddCmd(), a.manualRemoveCmd())
	cmd.AddCommand(manual)
	return cmd
}

func (a *app) manualListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the manual routes",
		Long: "List the manual routes by id, each with the revision it is at. A manual route owns its\n" +
			"hostname as manual/<id> and competes in the claims like any guest.\n\n" + jsonHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.json {
				raw, err := a.client().ManualRoutesRaw(ctx)
				if err != nil {
					return a.explain(ctx, err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			routes, err := a.client().ManualRoutes(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			return renderManualRoutes(cmd.OutOrStdout(), routes)
		},
	}
}

func renderManualRoutes(w io.Writer, routes []engine.ManualRouteView) error {
	s := &screen{w: w}
	if len(routes) == 0 {
		s.println("No manual routes.")
		return s.done()
	}
	t := s.table()
	t.row("ID", "REV", "HOSTNAME", "TARGET", "OPTIONS")
	for _, r := range routes {
		t.row(r.ID, fmt.Sprint(r.Rev), r.Hostname, manualTargetText(r.Target), dash(optionsText(r.Options)))
	}
	t.flush()
	return s.done()
}

// manualTargetText writes a target as a URL: http://10.0.5.20:9000, or
// https://qemu/101:8443 for a guest.
func manualTargetText(t engine.ManualTarget) string {
	host := t.Addr.String()
	if t.Kind == engine.TargetGuest {
		host = t.Guest
	}
	return fmt.Sprintf("%s://%s:%d", t.Scheme, host, t.Port)
}

// optionsText writes the options of a route as the Notes have them.
func optionsText(o model.RouteOptions) string {
	var out []string
	if o.NoTLSVerify {
		out = append(out, "no-tls-verify")
	}
	for _, opt := range []struct{ name, value string }{{"host-header", o.HostHeader}, {"sni", o.SNI}, {"via", o.Via}} {
		if opt.value != "" {
			out = append(out, opt.name+"="+opt.value)
		}
	}
	if o.AllowNode {
		out = append(out, "allow-node")
	}
	return strings.Join(out, ", ")
}

// manualFlags are the flags of pco route manual add.
type manualFlags struct {
	guest, address, id   string
	port                 uint16
	https, noTLSVerify   bool
	hostHeader, sni, via string
	allowNode            bool
}

// route is the manual route the flags describe for hostname.
func (f manualFlags) route(hostname string) (engine.ManualRouteView, error) {
	v := engine.ManualRouteView{
		ID: f.id, Hostname: hostname,
		Target:  engine.ManualTarget{Scheme: string(model.SchemeHTTP), Port: f.port},
		Options: model.RouteOptions{NoTLSVerify: f.noTLSVerify, HostHeader: f.hostHeader, SNI: f.sni, Via: f.via, AllowNode: f.allowNode},
	}
	if f.https {
		v.Target.Scheme = string(model.SchemeHTTPS)
	}
	switch {
	case (f.guest == "") == (f.address == ""):
		return v, errors.New("name the target with --guest or --address, one of them")
	case f.port == 0:
		return v, errors.New("--port is needed: the port of the service, from 1 to 65535")
	case f.guest != "":
		v.Target.Kind, v.Target.Guest = engine.TargetGuest, f.guest
	default:
		addr, err := netip.ParseAddr(f.address)
		if err != nil || !addr.Is4() {
			return v, fmt.Errorf("--address %q: want an IPv4 address", f.address)
		}
		v.Target.Kind, v.Target.Addr = engine.TargetAddress, addr
	}
	return v, nil
}

func (a *app) manualAddCmd() *cobra.Command {
	var f manualFlags
	cmd := &cobra.Command{
		Use:   "add <hostname> (--guest <owner> | --address <ipv4>) --port <port>",
		Short: "Make a manual route",
		Long: "Make a manual route that publishes a hostname to a guest, named as qemu/101 or lxc/200,\n" +
			"whose address is proven as for a route in its Notes, or to an IPv4 address inside the\n" +
			"trusted prefixes of the settings (trustedCIDRs). The options are those of the Notes.\n" +
			"--allow-node lets a route to an address point at a service of a node, which the egress\n" +
			"filter lets the connectors reach; a route to a guest never may. Without --id the daemon\n" +
			"gives the route an id of eight hex digits. It is published from the next cycle.\n\n" +
			"With --json the route as made is printed as JSON.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := f.route(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			made, err := a.client().CreateManualRoute(ctx, v)
			if err != nil {
				return a.explain(ctx, err)
			}
			if a.json {
				raw, err := json.Marshal(made)
				if err != nil {
					return fmt.Errorf("encoding the route: %w", err)
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("Made manual route %s: %s -> %s", model.ManualPrefix+made.ID, made.Hostname, manualTargetText(made.Target))
			if opts := optionsText(made.Options); opts != "" {
				s.printf(" (%s)", opts)
			}
			s.printf(".\nIt is published from the next cycle; pco routes shows how it fares.\n")
			return s.done()
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.guest, "guest", "", "the guest the route goes to, as qemu/101 or lxc/200")
	flags.StringVar(&f.address, "address", "", "the IPv4 address the route goes to")
	flags.Uint16Var(&f.port, "port", 0, "the port of the service")
	flags.BoolVar(&f.https, "https", false, "the service speaks HTTPS")
	flags.BoolVar(&f.noTLSVerify, "no-tls-verify", false, "do not check the certificate of an HTTPS service")
	flags.StringVar(&f.hostHeader, "host-header", "", "the Host header the service is sent")
	flags.StringVar(&f.sni, "sni", "", "the server name the TLS of an HTTPS service is asked for")
	flags.StringVar(&f.via, "via", "", "for a guest: the NIC (net0 to net31) or the address to reach it on")
	flags.BoolVar(&f.allowNode, "allow-node", false, "let a route to an address point at a service of a node")
	flags.StringVar(&f.id, "id", "", "the id of the route: 1 to 32 of a-z, 0-9 and -")
	return cmd
}

func (a *app) manualRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a manual route",
		Long: "Remove a manual route: its hostname is no longer published from the next cycle, unless\n" +
			"another owner claims it. The route is shown first, and the question needs a terminal; a\n" +
			"script passes --yes. The route is removed at the revision shown, and not when someone\n" +
			"changed it since.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.noJSON(cmd); err != nil {
				return err
			}
			ctx := cmd.Context()
			id := args[0]
			routes, err := a.client().ManualRoutes(ctx)
			if err != nil {
				return a.explain(ctx, err)
			}
			i := slices.IndexFunc(routes, func(r engine.ManualRouteView) bool { return r.ID == id })
			if i < 0 {
				return fmt.Errorf("there is no manual route %s: pco route manual list shows them", model.ManualPrefix+id)
			}
			r := routes[i]
			s := &screen{w: cmd.OutOrStdout()}
			s.printf("%s publishes %s -> %s (revision %d).\n", model.ManualPrefix+r.ID, r.Hostname, manualTargetText(r.Target), r.Rev)
			s.printf("Removing it stops publishing %s from the next cycle.\n", r.Hostname)
			if err := s.done(); err != nil {
				return err
			}
			ok, err := a.confirm(cmd, yes, fmt.Sprintf("Remove manual route %s? [y/N]", model.ManualPrefix+r.ID))
			if err != nil {
				return err
			}
			if !ok {
				return errAborted
			}
			if err := a.client().DeleteManualRoute(ctx, r.ID, r.Rev); err != nil {
				return a.explain(ctx, err)
			}
			s.printf("Removed manual route %s.\n", model.ManualPrefix+r.ID)
			return s.done()
		},
	}
	addYesFlag(cmd, &yes)
	return cmd
}
