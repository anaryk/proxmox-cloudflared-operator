package engine

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

const (
	// accessEvery is how often the access control of Proxmox is read.
	accessEvery = 60 * time.Second
	// accessKeptFor is how long what was read last stands while it cannot be
	// read again.
	accessKeptFor = 5 * time.Minute
	accessTimeout = 20 * time.Second

	// auditPriv is what pco's token must hold on the paths of the access
	// control for Proxmox to list all of it: without it, the ACL and the users
	// come back cut down to what the token may see, and still answer 200.
	auditPriv = "Sys.Audit"

	whyUnreadable = "the access control could not be read"
)

// auditPaths are where pco's token needs auditPriv.
var auditPaths = []string{"/access", "/access/groups"}

// Access is the part of the Proxmox client the engine reads access control
// with; *pve.Client satisfies it. Subnets and NodeDNS are read with it.
type Access interface {
	ACL(ctx context.Context) ([]pve.ACLEntry, error)
	Users(ctx context.Context) ([]pve.User, error)
	Groups(ctx context.Context) ([]pve.Group, error)
	Roles(ctx context.Context) ([]pve.Role, error)
	// Permissions answers for pco's own token when userid is empty.
	Permissions(ctx context.Context, userid, path string) (map[string][]string, error)
	Subnets(ctx context.Context) ([]pve.Subnet, error)
	NodeDNS(ctx context.Context, node string) (pve.NodeDNS, error)
}

// accessState is what was last read of the access control of Proxmox, and of
// the SDN subnets and the resolvers of the nodes, read with it. It is read
// and refreshed outside the cycle lock.
type accessState struct {
	refreshing sync.Mutex // held by the refresh that runs

	mu       sync.Mutex
	data     access.Data // without the pools, which come from each cycle's snapshot
	readAt   time.Time   // when data was read; zero while nothing was
	failedAt time.Time   // the first failure since data was read; zero after a success
	why      string      // what the last failure was
	nextAt   time.Time   // when the next read is due
	nodes    []string    // whose resolvers are read: the nodes the last cycle saw
	subnets  []pve.Subnet
	dns      map[string][]netip.Addr // by node
	factsErr string                  // why the subnets or a node's resolvers could not be read last
}

// accessView is what a cycle goes by.
type accessView struct {
	data       access.Data
	unreadable bool
	problem    string // why it is unreadable, as a problem line; empty without Access
	subnets    []pve.Subnet
	dns        map[string][]netip.Addr
	factsErr   string
}

// refreshAccess reads the access control when it is due: at the first cycle
// and every accessEvery after. A failure keeps what was read before; whether
// that still stands is the cycle's to decide.
func (e *Engine) refreshAccess(ctx context.Context) {
	if e.d.Access == nil {
		return
	}
	a := &e.access
	a.refreshing.Lock()
	defer a.refreshing.Unlock()
	now := e.d.Now()
	a.mu.Lock()
	due, nodes := !now.Before(a.nextAt), slices.Clone(a.nodes)
	a.mu.Unlock()
	if !due || ctx.Err() != nil {
		return
	}
	if !slices.Contains(nodes, e.d.Node) {
		nodes = append(nodes, e.d.Node)
	}
	ctx, cancel := e.timeout(ctx, accessTimeout)
	defer cancel()
	data, err := readAccess(ctx, e.d.Access)
	subnets, dns, factsErr := readFacts(ctx, e.d.Access, nodes)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextAt = now.Add(accessEvery)
	if err != nil {
		if a.failedAt.IsZero() {
			a.failedAt = now
		}
		a.why = err.Error()
		e.d.Log.Warn().Err(err).Msg("reading the access control of Proxmox failed")
	} else {
		a.data, a.readAt, a.failedAt, a.why = data, now, time.Time{}, ""
	}
	if subnets != nil {
		a.subnets = subnets
	}
	if a.dns == nil {
		a.dns = map[string][]netip.Addr{}
	}
	for node, servers := range dns {
		a.dns[node] = servers
	}
	a.factsErr = factsErr
}

// readAccess reads the four lists of the access control, and checks that pco's
// token may see all of them.
func readAccess(ctx context.Context, src Access) (access.Data, error) {
	for _, path := range auditPaths {
		perms, err := src.Permissions(ctx, "", path)
		if err != nil {
			return access.Data{}, err
		}
		if !slices.Contains(perms[path], auditPriv) {
			return access.Data{}, fmt.Errorf("pco's token does not hold %s on %s, so Proxmox lists only part of its access control", auditPriv, path)
		}
	}
	var d access.Data
	var err error
	if d.ACL, err = src.ACL(ctx); err != nil {
		return access.Data{}, err
	}
	if d.Users, err = src.Users(ctx); err != nil {
		return access.Data{}, err
	}
	if d.Groups, err = src.Groups(ctx); err != nil {
		return access.Data{}, err
	}
	if d.Roles, err = src.Roles(ctx); err != nil {
		return access.Data{}, err
	}
	return d, nil
}

// readFacts reads the SDN subnets and the resolvers of nodes. What could not
// be read is left out, with the first failure in words; subnets is nil when
// they could not be read.
func readFacts(ctx context.Context, src Access, nodes []string) (subnets []pve.Subnet, dns map[string][]netip.Addr, why string) {
	subnets, err := src.Subnets(ctx)
	if err != nil {
		why = err.Error()
		subnets = nil
	} else if subnets == nil {
		subnets = []pve.Subnet{}
	}
	dns = map[string][]netip.Addr{}
	for _, node := range nodes {
		got, err := src.NodeDNS(ctx, node)
		if err != nil {
			if why == "" {
				why = err.Error()
			}
			continue
		}
		dns[node] = got.Servers
	}
	return subnets, dns, why
}

// accessNow is what the cycle goes by at now, with the pools of the guests of
// snap, whose nodes the next read asks the resolvers of. Without Access,
// every guest counts as delegated.
func (e *Engine) accessNow(now time.Time, snap inventory.Snapshot) accessView {
	pools := make(map[model.GuestRef]string, len(snap.Guests))
	for _, g := range snap.Guests {
		if g.Pool != "" {
			pools[g.Ref] = g.Pool
		}
	}
	if e.d.Access == nil {
		return accessView{data: access.Data{Pools: pools}, unreadable: true}
	}
	a := &e.access
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(snap.Nodes) > 0 {
		a.nodes = a.nodes[:0]
		for _, n := range snap.Nodes {
			a.nodes = append(a.nodes, n.Name)
		}
	}
	v := accessView{data: a.data, subnets: slices.Clone(a.subnets), dns: make(map[string][]netip.Addr, len(a.dns)), factsErr: a.factsErr}
	v.data.Pools = pools
	for node, servers := range a.dns {
		v.dns[node] = slices.Clone(servers)
	}
	age := now.Sub(a.readAt)
	if !a.readAt.IsZero() && age >= 0 && age <= accessKeptFor {
		return v
	}
	v.unreadable = true
	switch {
	case a.failedAt.IsZero():
		v.problem = "the access control of Proxmox has not been read yet; every guest's observed routes wait for approval"
	default:
		v.problem = fmt.Sprintf("the access control of Proxmox could not be read since %s; every guest's observed routes wait for approval: %s",
			a.failedAt.UTC().Format(time.RFC3339), a.why)
	}
	return v
}

// delegates returns, by guest, the principals the guest is delegated to, as
// far as v knows; it reads the access control once per guest.
type delegates struct {
	v       accessView
	ownUser string
	byGuest map[model.GuestRef][]access.Principal
}

func (d *delegates) of(ref model.GuestRef) []access.Principal {
	if ps, ok := d.byGuest[ref]; ok {
		return ps
	}
	ps := access.Delegated(d.v.data, ref, d.ownUser)
	d.byGuest[ref] = ps
	return ps
}
