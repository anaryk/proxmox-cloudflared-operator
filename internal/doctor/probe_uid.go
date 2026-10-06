package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

const (
	// probeTimeout bounds a probe, the start of its process included; the
	// connection itself gets less in pco egress probe.
	probeTimeout = 8 * time.Second

	defaultPco = "/usr/bin/pco"
)

// The line pco egress probe prints for a connection that was made, for one that
// was refused, and for one that failed otherwise, with the reason after it.
const (
	ProbeConnected = "connected"
	ProbeRefused   = "refused"
	ProbeFailed    = "failed: "
)

// probeAsConnector connects to addr as the user the connectors run as, by
// running pco egress probe with the credentials of that user: the egress
// filter matches the user of the socket, so a connection of root says nothing
// of it. exe is the pco to run; empty is this one.
func probeAsConnector(ctx context.Context, exe, addr string) error {
	uid, gid, err := connectorIDs()
	if err != nil {
		return err
	}
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			exe = defaultPco
		}
	}
	return runProbe(ctx, exe, addr, &syscall.Credential{Uid: uid, Gid: gid})
}

// runProbe runs pco egress probe of exe for addr, with cred when there is one,
// in a directory and with an environment of nothing the caller has.
func runProbe(ctx context.Context, exe, addr string, cred *syscall.Credential) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var stdout, stderr capped
	stdout.max, stderr.max = maxVersionOutput, maxVersionOutput
	cmd := exec.CommandContext(ctx, exe, "egress", "probe", addr)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	if cred != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	}
	cmd.WaitDelay = time.Second
	runErr := cmd.Run()
	return probeResult(strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), runErr)
}

// probeResult reads what pco egress probe answered: its line says how the
// connection went, and anything else is a probe that did not run.
func probeResult(stdout, stderr string, runErr error) error {
	line, _, _ := strings.Cut(stdout, "\n")
	switch {
	case line == ProbeConnected && runErr == nil:
		return nil
	case line == ProbeRefused:
		return ErrRefused
	case strings.HasPrefix(line, ProbeFailed):
		return errors.New(strings.TrimPrefix(line, ProbeFailed))
	case runErr != nil && stderr != "":
		return fmt.Errorf("pco egress probe did not run: %w: %s", runErr, stderr)
	case runErr != nil:
		return fmt.Errorf("pco egress probe did not run: %w", runErr)
	}
	return fmt.Errorf("pco egress probe answered %q", line)
}

// connectorIDs is the user and the group of pco-connector.
func connectorIDs() (uid, gid uint32, err error) {
	u, err := user.Lookup(egress.ConnectorUser)
	var unknown user.UnknownUserError
	if errors.As(err, &unknown) {
		return 0, 0, fmt.Errorf("%s: %w", egress.ConnectorUser, egress.ErrNoConnectorUser)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("looking up user %s: %w", egress.ConnectorUser, err)
	}
	id, uerr := strconv.ParseUint(u.Uid, 10, 32)
	group, gerr := strconv.ParseUint(u.Gid, 10, 32)
	if uerr != nil || gerr != nil || id == 0 {
		return 0, 0, fmt.Errorf("user %s has uid %q and gid %q, which a probe cannot run as", egress.ConnectorUser, u.Uid, u.Gid)
	}
	return uint32(id), uint32(group), nil
}
