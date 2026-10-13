// Package members reads who is in the Proxmox cluster and whether it holds a
// quorum, from the files pmxcfs keeps on every node, and what identifies the
// cluster. A file that cannot be read whole, or that says less than a node of
// a cluster must, is an error and never an answer: a node that reads half a
// file must not take it for a cluster without quorum, for one without a
// quorum device, or for a node that stands alone.
package members

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	MembersFile  = "/etc/pve/.members"
	CorosyncFile = "/etc/pve/corosync.conf"
	RootCAFile   = "/etc/pve/pve-root-ca.pem"
)

// Member is a node as pmxcfs lists it. IP is invalid when the file gives no
// address for it; one that is given and is no address is an error.
type Member struct {
	Name   string
	ID     int
	Online bool
	IP     netip.Addr
}

// Members is the cluster as one node sees it.
type Members struct {
	// Self is the name of the node that read the file.
	Self string
	// Cluster is the name of the cluster, empty on a standalone node. It is
	// for messages; the identity of a cluster is its CA, see CAFingerprint.
	Cluster string
	// Standalone is a node that was never part of a cluster: the file has no
	// cluster in it and no list of nodes, and the node is quorate by itself.
	Standalone bool
	Quorate    bool
	// Nodes are by name. A node of a cluster is among them; a standalone node
	// is the one node of its list, with no id and no address, as the file
	// names it and nothing else.
	Nodes []Member
	// ReadAt is the time the read began, so that the age of what was read is
	// never understated.
	ReadAt time.Time
}

// Read parses the file at path, normally MembersFile. A file without a cluster
// is a node that stands alone only while no corosync.conf lies beside it:
// pmxcfs prints no cluster until corosync has told it about one, so a member
// that has just started, or lost corosync, reads like that, and its file is an
// error until the cluster is there.
func Read(path string, now time.Time) (Members, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Members{}, err
	}
	m, err := Parse(b, now)
	if err != nil {
		return Members{}, fmt.Errorf("%s: %w", path, err)
	}
	if m.Standalone {
		beside := filepath.Join(filepath.Dir(path), filepath.Base(CorosyncFile))
		if err := absent(beside); err != nil {
			return Members{}, fmt.Errorf("%s has no cluster in it: %w", path, err)
		}
	}
	return m, nil
}

func absent(path string) error {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("%s exists, so the node is one of a cluster that corosync has not reported yet", path)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	}
	return err
}

// clusterFile is the file as pmxcfs prints it: the name of the node and a
// version always, the cluster and the list of its nodes together once
// corosync has told pmxcfs about a cluster, and neither before. Both are kept
// raw so that one that is there but empty, or null, which pmxcfs never
// prints, is told from one that is absent.
type clusterFile struct {
	Nodename string          `json:"nodename"`
	Cluster  json.RawMessage `json:"cluster"`
	Nodelist json.RawMessage `json:"nodelist"`
}

type nodeEntry struct {
	ID     int    `json:"id"`
	Online *int   `json:"online"`
	IP     string `json:"ip"`
}

// Parse reads the content of the file. A file with a cluster must say
// whether the cluster is quorate and list this node among the nodes; a file
// with neither a cluster nor a list is a node that stands alone, listed as
// the one node of its own list, and whether a corosync.conf lies beside it
// is for Read to see. Anything in between, a cluster without its list, a
// list without its cluster, a cluster or a list that is empty or null, is
// nothing pmxcfs prints and is an error.
func Parse(b []byte, now time.Time) (Members, error) {
	var f clusterFile
	if err := json.Unmarshal(b, &f); err != nil {
		return Members{}, err
	}
	if f.Nodename == "" {
		return Members{}, errors.New("the file names no node")
	}
	m := Members{Self: f.Nodename, Quorate: true, ReadAt: now}
	switch {
	case f.Cluster == nil && f.Nodelist == nil:
		m.Standalone = true
		m.Nodes = []Member{{Name: f.Nodename, Online: true}}
		return m, nil
	case f.Cluster == nil:
		return Members{}, errors.New("the file lists nodes but no cluster")
	case f.Nodelist == nil:
		return Members{}, errors.New("the file has a cluster but no list of its nodes")
	}
	cluster, err := object("the cluster", f.Cluster)
	if err != nil {
		return Members{}, err
	}
	if m.Cluster, m.Quorate, err = clusterFacts(cluster); err != nil {
		return Members{}, err
	}
	list, err := object("the list of nodes", f.Nodelist)
	if err != nil {
		return Members{}, err
	}
	if _, ok := list[f.Nodename]; !ok {
		return Members{}, fmt.Errorf("the list of nodes of the cluster does not have %s in it", f.Nodename)
	}
	for name, raw := range list {
		if name == "" {
			return Members{}, errors.New("a node of the list has no name")
		}
		var n nodeEntry
		if err := json.Unmarshal(raw, &n); err != nil {
			return Members{}, fmt.Errorf("node %s: %w", name, err)
		}
		online, err := flag("online of node "+name, n.Online)
		if err != nil {
			return Members{}, err
		}
		var ip netip.Addr
		if n.IP != "" {
			if ip, err = netip.ParseAddr(n.IP); err != nil {
				return Members{}, fmt.Errorf("the address of node %s: %w", name, err)
			}
		}
		m.Nodes = append(m.Nodes, Member{Name: name, ID: n.ID, Online: online, IP: ip})
	}
	slices.SortFunc(m.Nodes, func(a, b Member) int { return strings.Compare(a.Name, b.Name) })
	return m, nil
}

// object is what a JSON object with at least one member decodes to; null,
// an empty object or any other value is an error.
func object(what string, raw json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%s is empty", what)
	}
	return m, nil
}

func clusterFacts(c map[string]json.RawMessage) (name string, quorate bool, err error) {
	var q *int
	if raw, ok := c["quorate"]; ok {
		if err := json.Unmarshal(raw, &q); err != nil {
			return "", false, fmt.Errorf("quorate: %w", err)
		}
	}
	if quorate, err = flag("quorate", q); err != nil {
		return "", false, err
	}
	if raw, ok := c["name"]; ok {
		if err := json.Unmarshal(raw, &name); err != nil {
			return "", false, fmt.Errorf("the name of the cluster: %w", err)
		}
	}
	return name, quorate, nil
}

// flag is a 0 or a 1, as pmxcfs prints them. A missing value, null, or any
// other number is no answer.
func flag(what string, v *int) (bool, error) {
	switch {
	case v == nil:
		return false, fmt.Errorf("%s is missing", what)
	case *v == 0:
		return false, nil
	case *v == 1:
		return true, nil
	}
	return false, fmt.Errorf("%s is %d, not 0 or 1", what, *v)
}

// Quorum keeps the last read of the members file for those who ask often and
// must not wait for the file: the lease keeper and the engine.
type Quorum struct {
	path string
	now  func() time.Time

	// reading serializes the reads so that the last one kept is the newest;
	// mu guards what is kept and is never held across a read of the file.
	reading sync.Mutex
	mu      sync.Mutex
	last    Members
}

// NewQuorum keeps the reads of the file at path, normally MembersFile, and
// takes their times from now.
func NewQuorum(path string, now func() time.Time) *Quorum {
	return &Quorum{path: path, now: now}
}

// Read reads the file and keeps what it read. A read that fails keeps the
// previous one, and Last still tells when that was. Read waits for the file,
// and for a read that another goroutine has not finished; pmxcfs can hang, so
// the caller owns the timeout, by reading from a goroutine it can leave
// behind. Last never waits.
func (q *Quorum) Read() (Members, error) {
	q.reading.Lock()
	defer q.reading.Unlock()
	m, err := Read(q.path, q.now())
	if err != nil {
		return Members{}, err
	}
	q.mu.Lock()
	q.last = m
	q.mu.Unlock()
	return clone(m), nil
}

// Last is the last read that succeeded and the time it began, the zero value
// and the zero time before there was one.
func (q *Quorum) Last() (Members, time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return clone(q.last), q.last.ReadAt
}

func clone(m Members) Members {
	m.Nodes = slices.Clone(m.Nodes)
	return m
}
