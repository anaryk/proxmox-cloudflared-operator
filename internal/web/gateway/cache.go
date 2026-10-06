package gateway

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
)

// maxCached is how many encoded states the cache keeps, those of admins and
// readers together.
const maxCached = 32

// snapshot is a state as the daemon sent it, decoded when a reader first
// needs it.
type snapshot struct {
	digest string
	raw    []byte

	once sync.Once
	st   engine.State
	err  error
}

func (s *snapshot) state() (engine.State, error) {
	s.once.Do(func() {
		if err := json.Unmarshal(s.raw, &s.st); err != nil {
			s.err = fmt.Errorf("reading the state of the daemon: %w", err)
		}
	})
	return s.st, s.err
}

// encoded is an answer of the state ready to send: its ETag, its JSON as it
// is when it is small and else gzipped, and for a reader the hostnames of
// the reader's routes.
type encoded struct {
	etag  string
	body  []byte
	gz    []byte
	hosts map[string]bool
}

func encode(etag string, body []byte) *encoded {
	if len(body) > gzipOver {
		return &encoded{etag: etag, gz: gzipped(body)}
	}
	return &encoded{etag: etag, body: body}
}

// stateCache keeps the newest state the daemon sent and the encoded answers
// made of it: per digest for admins and per digest and visible set for
// readers, so that 200 readers do not filter and encode a megabyte on every
// cycle. The answer used longest ago goes first. Answers that are being made
// are made once for everyone who asks meanwhile.
type stateCache struct {
	mu      sync.Mutex
	latest  *snapshot
	entries map[string]*list.Element // of *cached
	order   *list.List               // used last first
	making  map[string]*making
	// filtered counts the states filtered for readers, for the tests.
	filtered int
}

type cached struct {
	key string
	enc *encoded
}

type making struct {
	done chan struct{}
	enc  *encoded
	err  error
}

func newStateCache() *stateCache {
	return &stateCache{entries: map[string]*list.Element{}, order: list.New(), making: map[string]*making{}}
}

// get returns the answer of key, made with fn when it is not kept.
func (c *stateCache) get(key string, fn func() (*encoded, error)) (*encoded, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		c.order.MoveToFront(e)
		c.mu.Unlock()
		return e.Value.(*cached).enc, nil
	}
	if m, ok := c.making[key]; ok {
		c.mu.Unlock()
		<-m.done
		return m.enc, m.err
	}
	m := &making{done: make(chan struct{})}
	c.making[key] = m
	c.mu.Unlock()

	m.enc, m.err = fn()
	c.mu.Lock()
	delete(c.making, key)
	if m.err == nil {
		c.entries[key] = c.order.PushFront(&cached{key: key, enc: m.enc})
		for c.order.Len() > maxCached {
			last := c.order.Back()
			delete(c.entries, last.Value.(*cached).key)
			c.order.Remove(last)
		}
	}
	c.mu.Unlock()
	close(m.done)
	return m.enc, m.err
}

func (c *stateCache) newest() *snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

func (c *stateCache) keep(s *snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latest = s
}

func (c *stateCache) countFiltered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filtered++
}

// current is the daemon's state now. The gateway asks with the digest of the
// state it holds, so that an unchanged state costs a 304; actor is the user
// it asks for, none when it asks for the streams.
func (g *Gateway) current(ctx context.Context, actor string) (*snapshot, error) {
	held := g.states.newest()
	up := upstream{method: http.MethodGet, path: "/v1/state", actor: actor}
	if held != nil && held.digest != "" {
		up.ifNoneMatch = `"` + held.digest + `"`
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	res, err := g.call(ctx, up)
	switch {
	case err != nil:
		return nil, err
	case res.status == http.StatusNotModified && held != nil:
		return held, nil
	case res.status != http.StatusOK || !isJSON(res.ctype):
		return nil, &daemonRefused{reply: res}
	}
	s := &snapshot{digest: strings.Trim(strings.TrimPrefix(res.etag, "W/"), `"`), raw: res.body}
	g.states.keep(s)
	return s, nil
}

// adminState is the state as the daemon sent it.
func (g *Gateway) adminState(s *snapshot) (*encoded, error) {
	if s.digest == "" {
		return encode("", s.raw), nil
	}
	return g.states.get("a:"+s.digest, func() (*encoded, error) {
		return encode(`"`+s.digest+`"`, s.raw), nil
	})
}

// readerState is the state filtered for the visible set of hash. Its ETag
// is the first 16 hex of the SHA-256 of the digest and the hash, so that it
// changes with either.
func (g *Gateway) readerState(s *snapshot, visible auth.Visible, hash string) (*encoded, error) {
	build := func() (*encoded, error) {
		st, err := s.state()
		if err != nil {
			return nil, err
		}
		g.states.countFiltered()
		filtered := FilterState(st, visible)
		body, err := json.Marshal(filtered)
		if err != nil {
			return nil, fmt.Errorf("encoding a reader's state: %w", err)
		}
		etag := ""
		if s.digest != "" {
			sum := sha256.Sum256([]byte(s.digest + "\n" + hash))
			etag = `"` + hex.EncodeToString(sum[:])[:16] + `"`
		}
		enc := encode(etag, body)
		enc.hosts = visibleHosts(st, visible)
		return enc, nil
	}
	if s.digest == "" {
		return build()
	}
	return g.states.get("r:"+s.digest+":"+hash, build)
}

// state answers GET /api/v1/state from the cache: as the daemon sent it to
// an admin, filtered to a reader, each with its ETag and 304.
func (g *Gateway) state(c *gin.Context, rule Rule, r *Reader) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), g.timeoutOf(rule))
	defer cancel()
	s, err := g.current(ctx, auth.SessionOf(c).Principal.Actor())
	if err != nil {
		g.fail(c, err)
		return
	}
	var enc *encoded
	if r == nil {
		enc, err = g.adminState(s)
	} else {
		enc, err = g.readerState(s, r.Visible, r.Hash)
	}
	if err != nil {
		g.fail(c, err)
		return
	}
	if enc.etag != "" && holds(c.GetHeader("If-None-Match"), enc.etag) {
		c.Header("ETag", enc.etag)
		c.Status(http.StatusNotModified)
		return
	}
	g.write(c, reply{status: http.StatusOK, etag: enc.etag, body: enc.body}, enc.gz)
}

// holds reports whether an If-None-Match names the entity tag.
func holds(ifNoneMatch, tag string) bool {
	for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}

// Reader is what the filters and checks of one call know of a reader: the
// guests it may see, the hash of that set, and through the gateway the
// state it is filtered from.
type Reader struct {
	Visible auth.Visible
	Hash    string

	g     *Gateway
	ctx   context.Context
	actor string
}

// state is the daemon's state now, as it is, and the reader's view of it.
func (r *Reader) state() (engine.State, *encoded, error) {
	s, err := r.g.current(r.ctx, r.actor)
	if err != nil {
		return engine.State{}, nil, err
	}
	st, err := s.state()
	if err != nil {
		return engine.State{}, nil, err
	}
	enc, err := r.g.readerState(s, r.Visible, r.Hash)
	if err != nil {
		return engine.State{}, nil, err
	}
	return st, enc, nil
}
