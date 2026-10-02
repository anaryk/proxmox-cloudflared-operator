package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// onVerify runs fn while it answers a token check.
type onVerify struct {
	cfapi.API
	fn func()
}

func (v onVerify) VerifyToken(ctx context.Context) (cfapi.TokenStatus, error) {
	v.fn()
	return v.API.VerifyToken(ctx)
}

// A credential given another token while its old one is checked keeps no
// report of the old one, also when a cycle saw the new token meanwhile.
func TestARecheckOfATokenReplacedMeanwhileKeepsNoReport(t *testing.T) {
	e := newEnv(t)
	var once sync.Once
	e.useAPI(testToken, onVerify{API: e.cf, fn: func() {
		once.Do(func() {
			require.NoError(t, e.store.SaveCredential(store.Credential{
				ID: testCred, Label: "main", Kind: "scoped", Token: store.NewSecret("rotated-token-0123456789"), AddedAt: t0,
			}))
			e.clock.advance(10 * time.Second)
			e.cycle()
		})
	}})
	e.cycle()

	e.eng.recheck(t.Context())

	e.eng.repMu.Lock()
	_, kept := e.eng.reports[testCred]
	e.eng.repMu.Unlock()
	require.False(t, kept, "the report was of the old token")
}
