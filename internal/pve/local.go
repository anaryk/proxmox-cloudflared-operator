package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Runner runs a command on the node; the setup runner is one. The error of a
// command that fails says what it printed on stderr.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// NewLocal returns a client that reads what belongs to node with pvesh on
// that node itself, as root: the handlers of the API run in pvesh, so the
// reads need neither pveproxy nor a token. Only reads under /nodes/<node>/
// are made; pvesh would hand any other node's to that node over SSH.
func NewLocal(run Runner, node string) (*Client, error) {
	if !validSegment(node) {
		return nil, fmt.Errorf("invalid node name %q", node)
	}
	base := &url.URL{Scheme: "https", Host: "localhost"}
	hc := &http.Client{Transport: pvesh{run: run, prefix: "/" + apiPath + "/nodes/" + node + "/"}, CheckRedirect: noRedirect}
	return newClient(Config{}, base, hc), nil
}

// pvesh answers a GET of the API with what pvesh prints for it, in the
// envelope the API puts around it. A pvesh that fails is the 500 the API
// answers when its handler fails, with what pvesh said.
type pvesh struct {
	run    Runner
	prefix string
}

func (p pvesh) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("pvesh: only reads are made, not %s", req.Method)
	}
	if !strings.HasPrefix(req.URL.Path, p.prefix) {
		return nil, fmt.Errorf("pvesh: %s is not of this node", req.URL.Path)
	}
	args := []string{"get", strings.TrimPrefix(req.URL.Path, "/"+apiPath), "--output-format", "json"}
	query := req.URL.Query()
	for _, key := range slices.Sorted(maps.Keys(query)) {
		for _, v := range query[key] {
			args = append(args, "--"+key, v)
		}
	}
	out, err := p.run.Run(req.Context(), "pvesh", args...)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, errors.Join(ctxErr, err)
		}
		body, _ := json.Marshal(map[string]string{"message": err.Error()})
		return response(req, http.StatusInternalServerError, body), nil
	}
	data := bytes.TrimSpace([]byte(out))
	if len(data) == 0 {
		data = []byte("null")
	}
	body := append(append([]byte(`{"data":`), data...), '}')
	return response(req, http.StatusOK, body), nil
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

// Unreachable says that err is no answer of the API for what was asked: the
// request got no answer at all, or pveproxy, on this node or on the node it
// passed the request to, could not reach the node that answers it. A read of
// this node's own may then be made on the node instead.
func Unreachable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return unreachable(apiErr.Status)
	}
	return true
}
