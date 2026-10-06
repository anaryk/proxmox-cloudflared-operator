package members

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Corosync is what pco needs of corosync.conf: who is in the cluster, how it
// reaches a quorum, and over which addresses its nodes talk to each other.
type Corosync struct {
	Nodes []string
	// QuorumDevice is a device in the quorum section, such as a qnetd.
	QuorumDevice bool
	TwoNode      bool
	// ExpectedVotes is zero when the file does not set it.
	ExpectedVotes int
	// WaitForAll is nil when the file does not set it.
	WaitForAll *bool
	// Links are the addresses of ring0_addr, ring1_addr and so on, in the
	// order of their numbers, for each node that has any. An address that is a
	// name, which corosync resolves itself, is left out.
	Links map[string][]netip.Addr
}

// ReadCorosync parses the file at path, normally CorosyncFile. A standalone
// node has none, and the error then is one for which errors.Is(err,
// fs.ErrNotExist) holds. A file without a node list or without a totem
// section is refused as one that was cut.
func ReadCorosync(path string) (Corosync, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Corosync{}, err
	}
	c, err := parseCorosync(b)
	if err != nil {
		return Corosync{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// section is a block of the file, `name {` up to `}`.
type section struct {
	name     string
	keys     map[string]string
	children []*section
}

func (s *section) sections(name string) []*section {
	var out []*section
	for _, c := range s.children {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// parseTree reads the file as corosync does, a line at a time: a comment is a
// line that starts with a number sign, a section opens with a name and a
// brace and closes with a brace, anything else is `key: value` split at the
// first colon, so that an IPv6 address stays whole in the value. A section
// that is never closed is a file cut short.
func parseTree(b []byte) (*section, error) {
	root := &section{keys: map[string]string{}}
	stack := []*section{root}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		cur := stack[len(stack)-1]
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case line == "}":
			if len(stack) == 1 {
				return nil, fmt.Errorf("line %d: a closing brace without a section", i+1)
			}
			stack = stack[:len(stack)-1]
		case strings.HasSuffix(line, "{"):
			name := strings.TrimSpace(strings.TrimSuffix(line, "{"))
			if name == "" || strings.Contains(name, ":") {
				return nil, fmt.Errorf("line %d: %q is no section", i+1, line)
			}
			s := &section{name: name, keys: map[string]string{}}
			cur.children = append(cur.children, s)
			stack = append(stack, s)
		default:
			key, value, ok := strings.Cut(line, ":")
			key = strings.TrimSpace(key)
			if !ok || key == "" {
				return nil, fmt.Errorf("line %d: %q is neither a section nor a key", i+1, line)
			}
			cur.keys[key] = strings.TrimSpace(value)
		}
	}
	if len(stack) > 1 {
		return nil, fmt.Errorf("the file ends inside the section %s", stack[len(stack)-1].name)
	}
	return root, nil
}

func parseCorosync(b []byte) (Corosync, error) {
	root, err := parseTree(b)
	if err != nil {
		return Corosync{}, err
	}
	c := Corosync{Links: map[string][]netip.Addr{}}
	for _, list := range root.sections("nodelist") {
		for _, n := range list.sections("node") {
			name := n.keys["name"]
			if name == "" {
				return Corosync{}, errors.New("a node of the list has no name")
			}
			c.Nodes = append(c.Nodes, name)
			if links := ringAddrs(n.keys); len(links) > 0 {
				c.Links[name] = links
			}
		}
	}
	if len(c.Nodes) == 0 {
		return Corosync{}, errors.New("the file lists no node")
	}
	// Every corosync.conf of Proxmox ends with its totem section. A file that
	// stops between two sections shows no unclosed brace, and would read as a
	// cluster without a quorum section, and so without a quorum device.
	if len(root.sections("totem")) == 0 {
		return Corosync{}, errors.New("the file has no totem section, so it may be cut short")
	}
	slices.Sort(c.Nodes)
	for _, q := range root.sections("quorum") {
		if err := readQuorum(&c, q); err != nil {
			return Corosync{}, err
		}
	}
	return c, nil
}

func readQuorum(c *Corosync, q *section) error {
	if len(q.sections("device")) > 0 {
		c.QuorumDevice = true
	}
	if n, ok, err := q.number("two_node"); err != nil {
		return err
	} else if ok {
		c.TwoNode = n == 1
	}
	if n, ok, err := q.number("wait_for_all"); err != nil {
		return err
	} else if ok {
		wait := n == 1
		c.WaitForAll = &wait
	}
	if n, ok, err := q.number("expected_votes"); err != nil {
		return err
	} else if ok {
		c.ExpectedVotes = n
	}
	return nil
}

// number is the value of key as a number that is not below zero, and whether
// the section sets the key at all.
func (s *section) number(key string) (n int, ok bool, err error) {
	value, ok := s.keys[key]
	if !ok {
		return 0, false, nil
	}
	n, err = strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, false, fmt.Errorf("%s is %q, not a number", key, value)
	}
	return n, true, nil
}

// ringAddrs are the addresses of ring<N>_addr in the order of N.
func ringAddrs(keys map[string]string) []netip.Addr {
	type ring struct {
		n    int
		addr netip.Addr
	}
	var rings []ring
	for key, value := range keys {
		num, ok := strings.CutPrefix(key, "ring")
		if !ok {
			continue
		}
		num, ok = strings.CutSuffix(num, "_addr")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		if a, err := netip.ParseAddr(value); err == nil {
			rings = append(rings, ring{n, a})
		}
	}
	slices.SortFunc(rings, func(a, b ring) int { return a.n - b.n })
	out := make([]netip.Addr, len(rings))
	for i, r := range rings {
		out[i] = r.addr
	}
	return out
}
