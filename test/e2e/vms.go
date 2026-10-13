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

// vm is a virtual machine from a Debian cloud image, on the bridge of the
// run with an address there, reached over SSH from the node with a key the
// run makes. The managed network gives it a second card, which DHCP
// configures. Without PCO_E2E_CLOUD_IMAGE the scenarios that need one skip.
type vm struct {
	vmid  int
	notes string
	tags  string
	addr  string // on the bridge of the run, as 10.77.0.65
	// noHotplug leaves network hotplug off: a card pco adds waits for a
	// restart of the machine.
	noHotplug bool
}

const (
	cloudImageVar = "PCO_E2E_CLOUD_IMAGE"
	vmStorageVar  = "PCO_E2E_VM_STORAGE"
	vmUser        = "e2e"
	sshWait       = 4 * time.Minute
)

func (v vm) owner() string { return "qemu/" + strconv.Itoa(v.vmid) }

func (v vm) body(port int) string { return fmt.Sprintf("e2e %d:%d", v.vmid, port) }

// cloudImage is the image the virtual machines are made from, "" when the
// run has none.
func cloudImage() string { return os.Getenv(cloudImageVar) }

// skipWithoutImage skips a test that needs a virtual machine when the run
// has no image for one.
func skipWithoutImage(t *testing.T) string {
	t.Helper()
	image := cloudImage()
	if image == "" {
		t.Skipf("%s names no cloud image: the QEMU parts of the managed network are not run", cloudImageVar)
	}
	if _, err := os.Stat(image); err != nil {
		t.Fatalf("%s: %v", cloudImageVar, err)
	}
	return image
}

// sshKey makes the key of the run once and returns the paths of its private
// and public halves.
func (s *suite) sshKey(t testing.TB) (private, public string) {
	t.Helper()
	private = filepath.Join(s.dir, "e2e_key")
	if _, err := os.Stat(private); err != nil {
		s.must(t, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "pco-e2e", "-f", private)
	}
	return private, private + ".pub"
}

// createVM makes the machine from the image, with cloud-init giving it its
// address and the key of the run, starts it and waits until SSH answers.
func (s *suite) createVM(t testing.TB, v vm, image string) {
	t.Helper()
	require.True(t, s.mine(v.vmid), "vmid %d", v.vmid)
	s.vms[v.vmid] = true
	id := strconv.Itoa(v.vmid)
	storage := envOr(vmStorageVar, s.disk)
	tags := v.tags
	if tags == "" {
		tags = gateTag
	}
	args := []string{"create", id, "--name", "e2e-" + id, "--memory", "512", "--cores", "1", "--ostype", "l26",
		"--scsihw", "virtio-scsi-single", "--net0", "virtio,bridge=" + bridge,
		"--serial0", "socket", "--vga", "serial0", "--tags", tags, "--description", v.notes}
	if v.noHotplug {
		args = append(args, "--hotplug", "disk")
	}
	s.must(t, "qm", args...)
	s.must(t, "qm", "set", id, "--scsi0", storage+":0,import-from="+image)
	// On the command line qm reads the keys from a file; the API takes
	// their text.
	_, public := s.sshKey(t)
	s.must(t, "qm", "set", id, "--ide2", storage+":cloudinit", "--boot", "order=scsi0",
		"--ciuser", vmUser, "--sshkeys", public,
		"--ipconfig0", "ip="+v.addr+"/24,gw="+s.lay.nodeAddr)
	s.must(t, "qm", "start", id)
	s.waitSSH(t, v)
	t.Logf("VM %d is up at %s", v.vmid, v.addr)
}

// ssh runs a command in the machine as its user, who may sudo, with the key
// of the run.
func (s *suite) ssh(v vm, command string) (string, error) {
	private := filepath.Join(s.dir, "e2e_key")
	return s.run("ssh", "-i", private, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", "-o", "ConnectTimeout=10",
		vmUser+"@"+v.addr, command)
}

func (s *suite) mustSSH(t testing.TB, v vm, command string) string {
	t.Helper()
	out, err := s.ssh(v, command)
	require.NoError(t, err, command)
	return out
}

func (s *suite) waitSSH(t testing.TB, v vm) {
	t.Helper()
	deadline := time.Now().Add(sshWait)
	var last error
	for time.Now().Before(deadline) {
		if _, err := s.ssh(v, "true"); err == nil {
			return
		} else {
			last = err
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("VM %d does not answer over SSH at %s within %s: %v", v.vmid, v.addr, sshWait, last)
}

// serveVM starts a responder in the machine that answers the body on port.
func (s *suite) serveVM(t testing.TB, v vm, port int) {
	t.Helper()
	script := fmt.Sprintf(`cat >/tmp/e2e-serve.py <<'PY'
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = (%q + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
PY
nohup setsid python3 /tmp/e2e-serve.py %d >/dev/null 2>&1 </dev/null &
sleep 1`, v.body(port), port)
	s.mustSSH(t, v, script)
}

// ifaceOf finds the interface of the machine with a MAC, once it appeared.
func (s *suite) ifaceOf(t testing.TB, v vm, mac string) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, _ := s.ssh(v, "ip -o link")
		for line := range strings.Lines(strings.ToLower(out)) {
			if !strings.Contains(line, strings.ToLower(mac)) {
				continue
			}
			f := strings.Fields(line)
			if len(f) > 1 {
				name, _, _ := strings.Cut(strings.TrimSuffix(f[1], ":"), "@")
				return name
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("VM %d has no interface with the MAC %s:\n%s", v.vmid, mac, out)
		}
		time.Sleep(2 * time.Second)
	}
}

// dhcp asks for an address on an interface of the machine with the client
// the image has, and returns the address it got. The client is stopped
// afterwards, so that port 68 is free for the renewal the suite sends
// itself.
func (s *suite) dhcp(t testing.TB, v vm, iface string) string {
	t.Helper()
	script := fmt.Sprintf(`set -e
IF=%s
sudo ip link set "$IF" up
if command -v dhclient >/dev/null; then sudo timeout 60 dhclient -1 -v "$IF" 2>&1 | tail -3
elif command -v dhcpcd >/dev/null; then sudo timeout 60 dhcpcd -1 -4 "$IF" 2>&1 | tail -3
elif command -v udhcpc >/dev/null; then sudo timeout 60 udhcpc -n -q -i "$IF" 2>&1 | tail -3
else echo "no DHCP client in the image" >&2; exit 3; fi
sudo pkill -f "dhclient.*$IF" 2>/dev/null || true
sudo pkill -f "dhcpcd.*$IF" 2>/dev/null || true
ip -4 -o addr show dev "$IF" | awk '{print $4}'`, iface)
	out := s.mustSSH(t, v, script)
	lines := strings.Fields(out)
	require.NotEmpty(t, lines, "no address on %s of VM %d:\n%s", iface, v.vmid, out)
	addr, _, _ := strings.Cut(lines[len(lines)-1], "/")
	return addr
}

// renew sends a DHCP renewal from the machine, as a client does at T1: a
// REQUEST with its address as ciaddr, unicast to the server, and waits for
// the answer on its own address. It returns what came back.
func (s *suite) renew(t testing.TB, v vm, iface, lease, mac, server string) string {
	t.Helper()
	script := fmt.Sprintf(`cat >/tmp/e2e-renew.py <<'PY'
import random, socket, struct, sys
iface, lease, mac, server = sys.argv[1:5]
xid = random.randrange(1 << 32)
chaddr = bytes.fromhex(mac.replace(":", "")) + b"\0" * 10
head = struct.pack("!BBBBIHH", 1, 1, 6, 0, xid, 0, 0) + socket.inet_aton(lease) + b"\0" * 12
pkt = head + chaddr + b"\0" * 192 + b"\x63\x82\x53\x63" + bytes([53, 1, 3, 255])
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.setsockopt(socket.SOL_SOCKET, 25, iface.encode())
s.bind((lease, 68))
s.settimeout(5)
s.sendto(pkt, (server, 67))
data, src = s.recvfrom(1500)
kind = 0
i = 240
while i < len(data) and data[i] != 255:
    if data[i] == 0:
        i += 1
        continue
    if data[i] == 53:
        kind = data[i + 2]
    i += 2 + data[i + 1]
print("answer from %%s:%%d yiaddr=%%s type=%%d xid=%%s" %% (src[0], src[1], socket.inet_ntoa(data[16:20]), kind, "same" if struct.unpack("!I", data[4:8])[0] == xid else "other"))
PY
sudo python3 /tmp/e2e-renew.py %s %s %s %s`, iface, lease, mac, server)
	out, err := s.ssh(v, script)
	require.NoError(t, err, "the renewal from VM %d got no answer: %s", v.vmid, out)
	return strings.TrimSpace(out)
}

// restartVM stops and starts the machine, which applies a card that waited
// for it, and waits until SSH answers again.
func (s *suite) restartVM(t testing.TB, v vm) {
	t.Helper()
	id := strconv.Itoa(v.vmid)
	s.must(t, "qm", "stop", id)
	s.must(t, "qm", "start", id)
	s.waitSSH(t, v)
	s.sync(t)
}

func (s *suite) destroyVM(vmid int) error {
	id := strconv.Itoa(vmid)
	if out, _ := s.run("qm", "status", id); strings.Contains(out, "running") {
		if _, err := s.run("qm", "stop", id); err != nil {
			return err
		}
	}
	_, err := s.run("qm", "destroy", id, "--purge", "1")
	return err
}

// qmConfig is a line of the machine's configuration by its key, "" when
// there is none.
func (s *suite) qmConfig(t testing.TB, vmid int, key string) string {
	t.Helper()
	for line := range strings.Lines(s.must(t, "qm", "config", strconv.Itoa(vmid))) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), key+": "); ok {
			return rest
		}
	}
	return ""
}
