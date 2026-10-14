package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Runner runs a command on the node; the setup runner is one. The error of a
// command that fails says what it printed on stderr.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// pveshBackoff is how long a pvesh that did not run is not run again: each
// read meanwhile says why the last one failed.
const pveshBackoff = 15 * time.Second

// NewLocal returns a client that reads what belongs to node with pvesh on
// that node itself, as root: the handlers of the API run in pvesh, so the
// reads need neither pveproxy nor a token. Only reads under /nodes/<node>/
// are made; pvesh would hand any other node's to that node over SSH.
// ExecPvesh is the runner of the node.
func NewLocal(run Runner, node string) (*Client, error) { return newLocal(run, node, time.Now) }

func newLocal(run Runner, node string, now func() time.Time) (*Client, error) {
	if !validSegment(node) {
		return nil, fmt.Errorf("invalid node name %q", node)
	}
	base := &url.URL{Scheme: "https", Host: "localhost"}
	rt := &pvesh{run: run, prefix: "/" + apiPath + "/nodes/" + node + "/", now: now}
	hc := &http.Client{Transport: rt, CheckRedirect: noRedirect}
	return newClient(Config{}, base, hc), nil
}

// pvesh answers a GET of the API with what pvesh prints for it, in the
// envelope the API puts around it. A pvesh that ran and failed is the 500
// the API answers when its handler fails, with what pvesh said; one that
// did not run, or did not end in time, is no answer, and is not run again
// for pveshBackoff.
type pvesh struct {
	run    Runner
	prefix string
	now    func() time.Time

	mu       sync.Mutex
	until    time.Time
	notRunBy error
}

func (p *pvesh) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("pvesh: only reads are made, not %s", req.Method)
	}
	if !strings.HasPrefix(req.URL.Path, p.prefix) {
		return nil, fmt.Errorf("pvesh: %s is not of this node", req.URL.Path)
	}
	if err := p.resting(); err != nil {
		return nil, err
	}
	args := []string{"get", strings.TrimPrefix(req.URL.Path, "/"+apiPath), "--output-format", "json"}
	query := req.URL.Query()
	for _, key := range slices.Sorted(maps.Keys(query)) {
		for _, v := range query[key] {
			args = append(args, "--"+key, v)
		}
	}
	out, err := p.run.Run(req.Context(), "pvesh", args...)
	var exit interface{ ExitCode() int }
	switch {
	case err != nil && req.Context().Err() != nil:
		return nil, errors.Join(req.Context().Err(), err)
	case err != nil && errors.As(err, &exit):
		body, _ := json.Marshal(map[string]string{"message": err.Error()})
		return response(req, http.StatusInternalServerError, body), nil
	case err != nil:
		p.rest(err)
		return nil, err
	case len(out) > maxBodyBytes:
		return nil, fmt.Errorf("pvesh printed more than %d bytes", maxBodyBytes)
	}
	data := bytes.TrimSpace([]byte(out))
	if len(data) == 0 {
		data = []byte("null")
	}
	body := append(append([]byte(`{"data":`), data...), '}')
	return response(req, http.StatusOK, body), nil
}

// resting is why pvesh is not run now; nil when it may be.
func (p *pvesh) resting() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.notRunBy == nil || !p.now().Before(p.until) {
		return nil
	}
	return fmt.Errorf("pvesh is not run again until %s: %w", p.until.UTC().Format(time.RFC3339), p.notRunBy)
}

func (p *pvesh) rest(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.until, p.notRunBy = p.now().Add(pveshBackoff), err
}

func response(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// Unreachable says that err is no answer of the API for what was asked: no
// connection, none in time, or one cut short, or pveproxy, on this node or
// on the node it passed the request to, could not reach the node that
// answers it. A read of this node's own may then be made on the node
// instead. A certificate that does not verify, an answer that does not
// decode and an error of the caller are answers of a kind, and are not.
func Unreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || notVerified(err) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return unreachable(apiErr.Status)
	}
	var op *net.OpError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.As(err, &op),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET):
		return true
	case errors.As(err, &timeout):
		return timeout.Timeout()
	}
	return false
}
