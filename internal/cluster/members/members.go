// Package members reads who is in the Proxmox cluster and whether it holds a
// quorum, from the files pmxcfs keeps on every node, and what identifies the
// cluster. A file that cannot be read whole is an error and never an answer:
// a node that reads half a file must not take it for a cluster without
// quorum, or for one without a quorum device.
package members

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
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

// Member is a node as pmxcfs lists it. IP is invalid when the file gives none,
// or none that is an address.
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
	// cluster in it, and the node is quorate by itself.
	Standalone bool
	Quorate    bool
	// Nodes are by name. A standalone node has none, or only itself.
	Nodes []Member
	// ReadAt is the time the read began, so that the age of what was read is
	// never understated.
	ReadAt time.Time
}

// Read parses the file at path, normally MembersFile.
func Read(path string, now time.Time) (Members, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Members{}, err
	}
	m, err := Parse(b, now)
	if err != nil {
		return Members{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

type clusterFile struct {
	Nodename string                     `json:"nodename"`
	Cluster  map[string]json.RawMessage `json:"cluster"`
	Nodelist map[string]struct {
		ID     int    `json:"id"`
		Online *int   `json:"online"`
		IP     string `json:"ip"`
	} `json:"nodelist"`
}

// Parse reads the content of the file. A cluster object with fields must say
// whether the cluster is quorate; one without any fields, or none, is a
// standalone node.
func Parse(b []byte, now time.Time) (Members, error) {
	var f clusterFile
	if err := json.Unmarshal(b, &f); err != nil {
		return Members{}, err
	}
	if f.Nodename == "" {
		return Members{}, errors.New("the file names no node")
	}
	m := Members{Self: f.Nodename, Standalone: len(f.Cluster) == 0, Quorate: true, ReadAt: now}
	if !m.Standalone {
		var err error
		if m.Cluster, m.Quorate, err = clusterFacts(f.Cluster); err != nil {
			return Members{}, err
		}
	}
	for name, n := range f.Nodelist {
		if name == "" {
			return Members{}, errors.New("a node of the list has no name")
		}
		if n.Online == nil {
			return Members{}, fmt.Errorf("node %s does not say whether it is online", name)
		}
		ip, _ := netip.ParseAddr(n.IP)
		m.Nodes = append(m.Nodes, Member{Name: name, ID: n.ID, Online: *n.Online != 0, IP: ip})
	}
	slices.SortFunc(m.Nodes, func(a, b Member) int { return strings.Compare(a.Name, b.Name) })
	return m, nil
}

func clusterFacts(c map[string]json.RawMessage) (name string, quorate bool, err error) {
	q, ok := c["quorate"]
	if !ok {
		return "", false, errors.New("the cluster does not say whether it is quorate")
	}
	var n int
	if err := json.Unmarshal(q, &n); err != nil {
		return "", false, fmt.Errorf("quorate: %w", err)
	}
	if raw, ok := c["name"]; ok {
		if err := json.Unmarshal(raw, &name); err != nil {
			return "", false, fmt.Errorf("the name of the cluster: %w", err)
		}
	}
	return name, n != 0, nil
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

func NewQuorum(path string, now func() time.Time) *Quorum {
	return &Quorum{path: path, now: now}
}

// Read reads the file and keeps what it read. A read that fails keeps the
// previous one, and Last still tells when that was.
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
