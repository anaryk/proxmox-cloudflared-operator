package daemon

import (
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// cloudflareClients builds the Cloudflare clients of the daemon. A client is
// built for every cycle that finds a new token, and for every check and
// removal, so the budget cannot live in the client: one limiter per credential
// id is made on first use and handed to every client of that credential, for as
// long as the process runs. Each spends budget requests in 5 minutes.
type cloudflareClients struct {
	baseURL string // empty: the Cloudflare API
	now     func() time.Time
	budget  int

	mu       sync.Mutex
	limiters map[string]*cfapi.Limiter
}

func newCloudflareClients(baseURL string, now func() time.Time, budget int) *cloudflareClients {
	return &cloudflareClients{baseURL: baseURL, now: now, budget: budget, limiters: make(map[string]*cfapi.Limiter)}
}

// New is the engine's client factory. The token is revealed here and nowhere
// else.
func (f *cloudflareClients) New(c store.Credential) (cfapi.API, error) {
	return cfapi.New(cfapi.Options{
		BaseURL: f.baseURL,
		Token:   c.Token.Reveal(),
		Limiter: f.limiter(c.ID),
	})
}

// limiter returns the limiter of a credential. A token that is not stored yet,
// which is one that is being checked before it is added, has no id and no
// limiter here: its client paces itself.
func (f *cloudflareClients) limiter(id string) *cfapi.Limiter {
	if id == "" {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.limiters[id]
	if !ok {
		l = cfapi.NewCredentialLimiter(f.budget, f.now)
		f.limiters[id] = l
	}
	return l
}
