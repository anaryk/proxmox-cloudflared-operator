package setup

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// noProbe is the transport of the connector manager used here: setup reads
// which install a connector is of and never asks the connector whether it is
// ready.
type noProbe struct{}

func (noProbe) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("setup does not probe connectors")
}

// nodeConnectors are the connectors on this node, by the install their env
// file names.
type nodeConnectors struct {
	installs map[string]int // connectors by install id
	unknown  int            // connectors that name no install, or whose install could not be read
}

func (c nodeConnectors) none() bool { return len(c.installs) == 0 && c.unknown == 0 }

// lookForConnectors lists the connectors on the node, of every install, and
// reads the install of each through the connector manager.
func (s *Setup) lookForConnectors(ctx context.Context) (nodeConnectors, error) {
	dir := filepath.Join(s.paths().Local, tunnelsDir)
	m := connector.NewManager(unitControl{s.run}, dir, &http.Client{Transport: noProbe{}}, zerolog.Nop())
	ids, err := m.List(ctx)
	found := nodeConnectors{installs: make(map[string]int)}
	if err != nil && len(ids) == 0 {
		return found, fmt.Errorf("the connectors on this node could not be looked at, so setup cannot tell whether "+
			"another install runs here: %w; --new-install goes on without looking", err)
	}
	for _, id := range ids {
		st, err := m.Status(ctx, id)
		if err != nil || st.Install == "" {
			found.unknown++
			continue
		}
		found.installs[printableText(st.Install, 64)]++
	}
	return found, nil
}

// refusal says which installs the connectors belong to and what the admin can
// do.
func (c nodeConnectors) refusal() error {
	ids := slices.Sorted(maps.Keys(c.installs))
	var of []string
	for _, id := range ids {
		of = append(of, fmt.Sprintf("install %s (%s)", id, countOf(c.installs[id])))
	}
	if c.unknown > 0 {
		of = append(of, fmt.Sprintf("no known install (%s)", countOf(c.unknown)))
	}
	adopt, recoverCmd := "that install", "pco setup --recover"
	if len(ids) > 1 {
		adopt, recoverCmd = "one of them", recoverCmd+" --install-id <id>"
	}
	return fmt.Errorf("the store holds no install, but this node runs connectors of %s, which a new install would never prune: "+
		"run %s to adopt %s, or pco uninstall --keep-cloudflare to remove pco from this node and start over; "+
		"pco setup --new-install starts a new install beside them", strings.Join(of, ", "), recoverCmd, adopt)
}

func countOf(n int) string {
	if n == 1 {
		return "1 connector"
	}
	return fmt.Sprintf("%d connectors", n)
}

// refuseBesideConnectors refuses to create an install while connectors of
// another run on the node: after a lost store they are the install that
// --recover adopts, and a new install would leave them running for good. It
// looks before anything is created.
func (r *run) refuseBesideConnectors(ctx context.Context) error {
	_, found, err := r.st.Install()
	switch {
	case errors.Is(err, store.ErrNoRoot):
	case err != nil:
		return fmt.Errorf("reading the install: %w", err)
	case found:
		return nil
	}
	c, err := r.lookForConnectors(ctx)
	switch {
	case err != nil:
		return err
	case c.none():
		return nil
	}
	return c.refusal()
}
