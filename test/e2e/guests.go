//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// guest is an Alpine container on the bridge of the run that serves a line of
// HTTP on each of its ports. The networking scenarios put theirs on a bridge
// of their own, with an address there.
type guest struct {
	vmid  int
	notes string
	ports []int // 8080 when none is given
	// manual leaves the address out of the configuration: the start script
	// sets it, and Proxmox learns it only from the running container.
	manual bool
	// start are more lines of the start script, run before the servers.
	start []string
	// bridge and addr place the guest on another bridge than the run's,
	// with that address (as 10.50.0.10/24); vlan tags its port there.
	bridge string
	addr   string
	vlan   int
	// tags are the tags of the guest; the gate tag alone when empty.
	tags string
}

// ip is the address of the guest: 10.77.0.<vmid - 9000> on the bridge of the
// run, or the one given.
func (g guest) ip() string {
	if g.addr != "" {
		ip, _, _ := strings.Cut(g.addr, "/")
		return ip
	}
	return fmt.Sprintf("10.77.0.%d", g.vmid-9000)
}

func (g guest) owner() string { return "lxc/" + strconv.Itoa(g.vmid) }

// inRange says whether the id is one the suite may use: the range of the
// run, or the range of the networking scenarios.
func inRange(vmid int) bool {
	return (vmid >= firstVMID && vmid <= lastVMID) || (vmid >= netFirstVMID && vmid <= netLastVMID)
}

func (g guest) servedPorts() []int {
	if len(g.ports) == 0 {
		return []int{8080}
	}
	return g.ports
}

// body is what the guest answers on port.
func (g guest) body(port int) string { return fmt.Sprintf("e2e %d:%d", g.vmid, port) }

func (g guest) net0() string {
	ip := "ip=" + g.ip() + "/24"
	switch {
	case g.manual:
		ip = "ip=manual"
	case g.addr != "":
		ip = "ip=" + g.addr
	}
	spec := "name=eth0,bridge=" + g.bridgeOf() + "," + ip
	if g.vlan != 0 {
		spec += ",tag=" + strconv.Itoa(g.vlan)
	}
	return spec
}

// bridgeOf is the bridge of the guest's first card.
func (g guest) bridgeOf() string {
	if g.bridge != "" {
		return g.bridge
	}
	return bridge
}

func (g guest) tagsOf() string {
	if g.tags != "" {
		return g.tags
	}
	return gateTag
}

// startScript runs at every start of the container, from /etc/local.d.
func (g guest) startScript() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if g.manual {
		fmt.Fprintf(&b, "ip link set eth0 up\nip addr add %s/24 dev eth0\n", g.ip())
	}
	for _, line := range g.start {
		b.WriteString(line + "\n")
	}
	for _, port := range g.servedPorts() {
		fmt.Fprintf(&b, "setsid /usr/local/bin/e2e-serve %d </dev/null >/dev/null 2>&1 &\n", port)
	}
	return b.String()
}

// serveScript keeps one port open. Busybox nc keeps listening with -lk or
// with -ll, depending on how it was built, and refuses the option it does not
// know at once; without either it answers one connection at a time.
const serveScript = `#!/bin/sh
reply=/usr/local/bin/e2e-reply-$1
nc -lk -p "$1" -e "$reply" 2>/dev/null
nc -ll -p "$1" -e "$reply" 2>/dev/null
while :; do
	nc -l -p "$1" -e "$reply" || sleep 0.2
done
`

// replyScript reads the request and answers it with body.
func replyScript(body string) string {
	return fmt.Sprintf(`#!/bin/sh
while read -r line; do
	line=$(printf '%%s' "$line" | tr -d '\r')
	[ -z "$line" ] && break
done
body='%s'
printf 'HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %%d\r\nConnection: close\r\n\r\n%%s\n' $((${#body} + 1)) "$body"
`, body)
}

// create makes the guest, tagged with its notes, and starts it.
func (s *suite) create(t testing.TB, g guest) {
	t.Helper()
	require.True(t, inRange(g.vmid), "vmid %d", g.vmid)
	s.guests[g.vmid] = true
	id := strconv.Itoa(g.vmid)
	s.must(t, "pct", "create", id, s.tmpl,
		"--hostname", "e2e-"+id, "--ostype", "alpine", "--unprivileged", "1",
		"--memory", "64", "--swap", "0", "--cores", "1",
		"--rootfs", s.disk+":1", "--net0", g.net0(),
		"--tags", g.tagsOf(), "--description", g.notes)
	s.must(t, "pct", "start", id)
	s.waitExec(t, g.vmid)

	files := map[string]string{
		"/etc/local.d/e2e.start":   g.startScript(),
		"/usr/local/bin/e2e-serve": serveScript,
	}
	for _, port := range g.servedPorts() {
		files["/usr/local/bin/e2e-reply-"+strconv.Itoa(port)] = replyScript(g.body(port))
	}
	for path, content := range files {
		s.push(t, g.vmid, path, content)
	}
	s.exec(t, g.vmid, "rc-update add local default >/dev/null && /etc/local.d/e2e.start")
	t.Logf("guest %d is up at %s", g.vmid, g.ip())
}

// push writes a file into a running guest, executable.
func (s *suite) push(t testing.TB, vmid int, path, content string) {
	t.Helper()
	local := filepath.Join(s.dir, fmt.Sprintf("push-%d-%s", vmid, filepath.Base(path)))
	require.NoError(t, os.WriteFile(local, []byte(content), 0o600))
	s.exec(t, vmid, "mkdir -p "+filepath.Dir(path))
	s.must(t, "pct", "push", strconv.Itoa(vmid), local, path, "--perms", "0755")
}

// exec runs a shell command in a running guest.
func (s *suite) exec(t testing.TB, vmid int, command string) {
	t.Helper()
	s.must(t, "pct", "exec", strconv.Itoa(vmid), "--", "sh", "-c", command)
}

// try runs a shell command in a running guest and returns what it printed
// and whether it exited 0.
func (s *suite) try(vmid int, command string) (string, bool) {
	out, err := s.run("pct", "exec", strconv.Itoa(vmid), "--", "sh", "-c", command)
	return out, err == nil
}

// connects says whether a guest can open a TCP connection to an address and
// port: busybox nc, with nothing to send, exits 0 once connected.
func (s *suite) connects(vmid int, addr, port string) bool {
	_, ok := s.try(vmid, "timeout 4 nc -w 2 "+addr+" "+port+" </dev/null >/dev/null 2>&1")
	return ok
}

// waitExec waits until commands can run in a guest that was started.
func (s *suite) waitExec(t testing.TB, vmid int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		_, err := s.run("pct", "exec", strconv.Itoa(vmid), "--", "true")
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest %d does not run commands: %v", vmid, err)
		}
		time.Sleep(time.Second)
	}
}

func (s *suite) setNotes(t testing.TB, vmid int, notes string) {
	t.Helper()
	s.must(t, "pct", "set", strconv.Itoa(vmid), "--description", notes)
	s.sync(t)
}

func (s *suite) setTags(t testing.TB, vmid int, tags string) {
	t.Helper()
	id := strconv.Itoa(vmid)
	if tags == "" {
		s.must(t, "pct", "set", id, "--delete", "tags")
	} else {
		s.must(t, "pct", "set", id, "--tags", tags)
	}
	s.sync(t)
}

func (s *suite) stop(t testing.TB, vmid int) {
	t.Helper()
	s.must(t, "pct", "stop", strconv.Itoa(vmid))
	s.sync(t)
}

func (s *suite) start(t testing.TB, vmid int) {
	t.Helper()
	s.must(t, "pct", "start", strconv.Itoa(vmid))
	s.waitExec(t, vmid)
	s.sync(t)
}

// destroy removes a guest of the run.
func (s *suite) destroy(t testing.TB, vmid int) {
	t.Helper()
	require.NoError(t, s.destroyGuest(vmid))
	delete(s.guests, vmid)
}

func (s *suite) destroyGuest(vmid int) error {
	id := strconv.Itoa(vmid)
	if out, _ := s.run("pct", "status", id); strings.Contains(out, "running") {
		if _, err := s.run("pct", "stop", id); err != nil {
			return err
		}
	}
	_, err := s.run("pct", "destroy", id, "--purge", "1")
	return err
}

func (s *suite) destroyGuests(t *testing.T) {
	for vmid := range s.guests {
		if err := s.destroyGuest(vmid); err != nil {
			t.Errorf("destroying guest %d: %v", vmid, err)
		}
	}
	for vmid := range s.vms {
		if err := s.destroyVM(vmid); err != nil {
			t.Errorf("destroying VM %d: %v", vmid, err)
		}
	}
}

// destroyPconetGuests destroys the guests that may carry a card on pconet,
// before pco is uninstalled: a card left on the vnet keeps its zone.
func (s *suite) destroyPconetGuests(t *testing.T) {
	for vmid := range s.pconetGuests {
		var err error
		switch {
		case s.vms[vmid]:
			err = s.destroyVM(vmid)
			delete(s.vms, vmid)
		case s.guests[vmid]:
			err = s.destroyGuest(vmid)
			delete(s.guests, vmid)
		}
		if err != nil {
			t.Errorf("destroying guest %d: %v", vmid, err)
		}
	}
}

// setMAC gives eth0 of the guest another MAC while the link stays up, as a
// veth allows, and empties the guest's neighbour table, so that its next
// packet to the node is an ARP request that carries the new MAC.
func (s *suite) setMAC(t testing.TB, vmid int, mac string) {
	t.Helper()
	s.exec(t, vmid, "ip link set dev eth0 address "+mac+" && ip neigh flush dev eth0")
}

// hwaddr is the MAC of net0 in the configuration of the guest.
func (s *suite) hwaddr(t testing.TB, vmid int) string {
	t.Helper()
	cfg := s.must(t, "pct", "config", strconv.Itoa(vmid))
	for line := range strings.Lines(cfg) {
		rest, ok := strings.CutPrefix(line, "net0: ")
		if !ok {
			continue
		}
		for part := range strings.SplitSeq(strings.TrimSpace(rest), ",") {
			if mac, ok := strings.CutPrefix(part, "hwaddr="); ok {
				return strings.ToLower(mac)
			}
		}
	}
	t.Fatalf("guest %d has no hwaddr on net0:\n%s", vmid, cfg)
	return ""
}
