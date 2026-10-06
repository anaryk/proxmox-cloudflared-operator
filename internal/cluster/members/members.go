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
	// cluster in it and lists no node but this one, and the node is quorate by
	// itself.
	Standalone bool
	Quorate    bool
	// Nodes are by name. A node of a cluster is among them; a standalone node
	// has none, or only itself.
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
// whether the cluster is quorate, and the list of nodes must have this node in
// it. No cluster object, or one without fields, is a node that stands alone,
// provided it lists no other node; whether it has a corosync.conf is for Read
// to see.
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
		if _, ok := f.Nodelist[f.Nodename]; !ok {
			return Members{}, fmt.Errorf("the list of nodes of the cluster does not have %s in it", f.Nodename)
		}
	}
	for name, n := range f.Nodelist {
		if name == "" {
			return Members{}, errors.New("a node of the list has no name")
		}
		if m.Standalone && name != f.Nodename {
			return Members{}, fmt.Errorf("no cluster, but the list has node %s besides this one", name)
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
