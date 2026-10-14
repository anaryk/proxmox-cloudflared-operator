package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// backupDir is where finish-upgrade keeps its backup, below the local root.
const backupDir = "backup"

// FinishUpgrade ends an upgrade from an earlier release: from then on the
// previous release cannot be installed again, and the settings this release
// adds can be set. It is refused while a registered node that is not removed
// runs a build that does not read this release's store; a node that is down
// counts, so a dead node is forgotten first. Before anything changes, the
// store is copied below the local root of this node.
func (e *Engine) FinishUpgrade(ctx context.Context) (FinishUpgradeResult, error) {
	if err := e.acquireWriter(ctx); err != nil {
		return FinishUpgradeResult{}, err
	}
	defer e.release()
	up, found, err := e.d.Store.UpgradeState()
	switch {
	case err != nil:
		return FinishUpgradeResult{}, fmt.Errorf("reading the upgrade: %w", err)
	case !found:
		return FinishUpgradeResult{}, fmt.Errorf("%w: no upgrade from an earlier release is unfinished", ErrRefused)
	}
	nodes, err := e.d.Store.Nodes()
	if err != nil {
		return FinishUpgradeResult{}, fmt.Errorf("reading the node registry: %w", err)
	}
	if older := olderBuilds(nodes); len(older) > 0 {
		return FinishUpgradeResult{}, fmt.Errorf("%w: %s; a node that is down is forgotten first (pco node forget), "+
			"the others are upgraded first", ErrRefused, olderText(older))
	}
	if e.d.LocalDir == "" {
		return FinishUpgradeResult{}, fmt.Errorf("%w: this node has no local root to keep the backup in", ErrRefused)
	}
	dir := filepath.Join(e.d.LocalDir, backupDir, fmt.Sprintf("%d-%s", store.SchemaVersion(), e.d.Now().UTC().Format("20060102T150405Z")))
	if err := e.d.Store.BackupLocal(dir); err != nil {
		return FinishUpgradeResult{}, fmt.Errorf("%w: the backup of the store failed: %w; nothing was changed", ErrRefused, err)
	}
	if err := e.d.Store.DeleteUpgradeState(); err != nil {
		return FinishUpgradeResult{}, fmt.Errorf("finishing the upgrade: %w", err)
	}
	res := FinishUpgradeResult{From: up.From, Schema: store.SchemaVersion(), Backup: dir}
	e.adminEvent(ctx, "", fmt.Sprintf("the upgrade from %s is finished; the store was copied to %s first", up.From, dir))
	e.Trigger()
	return res, nil
}

// olderNode is a node whose build does not read this release's store.
type olderNode struct {
	name, state string
	schema      int
}

// olderBuilds are the registered nodes, removed ones aside, whose build
// reads an older schema of the store than this one, or whose entry does not
// say.
func olderBuilds(nodes []store.NodeEntry) []olderNode {
	var out []olderNode
	for _, n := range nodes {
		if n.State == store.NodeRemoved || n.Schema >= store.SchemaVersion() {
			continue
		}
		out = append(out, olderNode{name: n.Name, state: n.State, schema: n.Schema})
	}
	slices.SortFunc(out, func(a, b olderNode) int { return strings.Compare(a.name, b.name) })
	return out
}

func olderText(older []olderNode) string {
	parts := make([]string, 0, len(older))
	for _, n := range older {
		part := n.name
		if n.schema == 0 {
			part += " (an earlier release, without a store schema)"
		} else {
			part += fmt.Sprintf(" (store schema %d)", n.schema)
		}
		if n.state == store.NodeDown {
			part += ", which is down"
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("%s a build that does not read store schema %d: %s", nodesRun(len(older)), store.SchemaVersion(), andList(parts))
}

func nodesRun(n int) string {
	if n == 1 {
		return "1 node runs"
	}
	return fmt.Sprintf("%d nodes run", n)
}

// upgradeView is the unfinished upgrade as the state shows it; nil when
// there is none.
func (c *cycleRun) upgradeView() *UpgradeView {
	up, found, err := c.e.d.Store.UpgradeState()
	if err != nil || !found {
		return nil
	}
	v := &UpgradeView{From: up.From, Since: up.Since}
	entries := make([]store.NodeEntry, 0, len(c.nodes))
	for _, n := range c.nodes {
		entries = append(entries, n.Node)
	}
	for _, n := range olderBuilds(entries) {
		v.Older = append(v.Older, n.name)
	}
	return v
}

// ErrNoUpgrade is wrapped by ClusterStatus of a node that was never
// upgraded; kept for callers that tell the two apart.
var ErrNoUpgrade = errors.New("no unfinished upgrade")
