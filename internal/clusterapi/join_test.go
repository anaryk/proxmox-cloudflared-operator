package clusterapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	base := t.TempDir()
	s, err := store.Open(store.Paths{
		Cluster: filepath.Join(base, "cluster"), Private: filepath.Join(base, "private"), Local: filepath.Join(base, "local"),
	})
	require.NoError(t, err)
	require.NoError(t, s.Init())
	return s
}

func applianceID(node string, vmid int) pki.Identity {
	return pki.Identity{Kind: pki.KindAppliance, InstallID: testInstall, Node: node, VMID: vmid}
}

// joinSetup is a server that takes joins, with one join token of testNode in
// its store, and a controller that issues what it is asked for.
type joinSetup struct {
	h       *harness
	st      *store.Store
	tok     pki.JoinToken
	key     *ecdsa.PrivateKey
	csr     []byte
	joined  atomic.Int32
	misused atomic.Int32
	issue   func(csr []byte, facts JoinFacts) (*x509.Certificate, error)
}

func newJoinSetup(t *testing.T) *joinSetup {
	t.Helper()
	j := &joinSetup{st: openStore(t)}
	h := newHarnessWith(t, func(s *Server) {
		s.AcceptJoins(Joins{
			InstallID: testInstall,
			CASHA256:  func() string { return j.h.ca.SHA256() },
			Tokens:    j.st,
			Misused:   func(context.Context, store.JoinRecord) { j.misused.Add(1) },
		})
	})
	j.h = h
	var err error
	j.tok, err = pki.NewJoinToken(testInstall, testNode, []string{h.addr.String()}, h.ca, h.clock.now(), rand.Reader)
	require.NoError(t, err)
	require.NoError(t, j.st.SaveJoinToken(store.JoinRecord{
		ID: "tok1", Node: testNode, SecretSHA256: j.tok.SecretHash(), CreatedAt: h.clock.now(), Expires: j.tok.Expires,
	}))
	j.issue = func(csr []byte, facts JoinFacts) (*x509.Certificate, error) {
		return h.ca.IssueClient(csr, applianceID(facts.Node, facts.VMID), h.clock.now(), rand.Reader)
	}
	h.ctrl.setJoin(func(_ pki.JoinToken, csr []byte, _ netip.Addr, facts JoinFacts) ([]byte, error) {
		j.joined.Add(1)
		cert, err := j.issue(csr, facts)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), nil
	})
	j.key, err = pki.NewKey(rand.Reader)
	require.NoError(t, err)
	j.csr, err = pki.NewCSR(j.key, applianceID(testNode, 200), rand.Reader)
	require.NoError(t, err)
	return j
}

func (j *joinSetup) req() JoinRequest {
	return JoinRequest{
		Token: j.tok.Encode(), CSR: j.csr, Node: testNode, VMID: 200, MACs: []string{"bc:24:11:00:00:aa"},
		Build: "dev", Protocol: agentproto.Range{Min: 1, Max: 1},
	}
}

func (j *joinSetup) send(t *testing.T, req JoinRequest) *answer {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	return j.h.post(j.h.bare(), "/v1/agent/join", body)
}

func (j *joinSetup) uses(t *testing.T) []store.JoinUse {
	t.Helper()
	rec, _, found, err := j.st.JoinToken("tok1")
	require.NoError(t, err)
	require.True(t, found)
	return rec.Uses
}

// reencoded is the token with what edit changes in its JSON, as one who holds
// a token can write it.
func reencoded(t *testing.T, tok string, edit func(map[string]any)) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, pki.JoinPrefix))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	edit(m)
	raw, err = json.Marshal(m)
	require.NoError(t, err)
	return pki.JoinPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func TestAnUnusedJoinTokenIsAcceptedOnce(t *testing.T) {
	j := newJoinSetup(t)

	resp := j.send(t, j.req())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var answer CertificateResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&answer))
	block, _ := pem.Decode([]byte(answer.CertPEM))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	id, err := pki.IdentityOf(cert)
	require.NoError(t, err)
	require.Equal(t, applianceID(testNode, 200), id)
	caBlock, _ := pem.Decode([]byte(answer.CAPEM))
	require.NotNil(t, caBlock)
	require.Equal(t, j.h.ca.Cert.Raw, caBlock.Bytes, "the CA the agent pinned comes with the certificate")

	uses := j.uses(t)
	require.Len(t, uses, 1)
	require.Equal(t, loopback, uses[0].Source)
	require.Equal(t, 200, uses[0].VMID)
	require.Equal(t, []string{"bc:24:11:00:00:aa"}, uses[0].MACs)

	t.Run("a second use is refused, recorded and told", func(t *testing.T) {
		r := refusalOf(t, j.send(t, j.req()))
		require.Equal(t, RefusalRefused, r.Code)
		require.Equal(t, "refused", r.Message, "a bad token learns nothing")
		require.Len(t, j.uses(t), 2)
		require.Equal(t, int32(1), j.misused.Load())
		require.Equal(t, int32(1), j.joined.Load(), "the controller is asked once")
	})
	t.Run("a certificate that is sent is not looked at", func(t *testing.T) {
		body, err := json.Marshal(j.req())
		require.NoError(t, err)
		resp := j.h.post(j.h.client(j.h.ca, hostID("pve3")), "/v1/agent/join", body)
		require.Equal(t, RefusalRefused, refusalOf(t, resp).Code, "refused for the used token, not taken for the certificate")
	})
}

func TestTwoJoinsWithOneTokenAtOnceGetOneCertificate(t *testing.T) {
	j := newJoinSetup(t)
	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			body, err := json.Marshal(j.req())
			if err != nil {
				codes <- 0
				return
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, j.h.url("/v1/agent/join"), strings.NewReader(string(body)))
			if err != nil {
				codes <- 0
				return
			}
			<-start
			resp, err := j.h.bare().Do(req)
			if err != nil {
				codes <- 0
				return
			}
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		})
	}
	close(start)
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	require.Equal(t, map[int]int{http.StatusOK: 1, http.StatusForbidden: n - 1}, count)
	require.Len(t, j.uses(t), n, "every use is recorded")
	require.Equal(t, int32(1), j.joined.Load())
	require.Equal(t, int32(n-1), j.misused.Load())
}

func TestAJoinIsRefusedWhenAnythingDoesNotHold(t *testing.T) {
	other, err := pki.NewCA("fedcba987654", t0, rand.Reader)
	require.NoError(t, err)
	csrFor := func(t *testing.T, id pki.Identity) []byte {
		t.Helper()
		key, err := pki.NewKey(rand.Reader)
		require.NoError(t, err)
		csr, err := pki.NewCSR(key, id, rand.Reader)
		require.NoError(t, err)
		return csr
	}
	cases := map[string]func(t *testing.T, j *joinSetup, r *JoinRequest){
		"no token":      func(_ *testing.T, _ *joinSetup, r *JoinRequest) { r.Token = "" },
		"no such token": func(_ *testing.T, _ *joinSetup, r *JoinRequest) { r.Token = "pco1.e30" },
		"another prefix": func(_ *testing.T, _ *joinSetup, r *JoinRequest) {
			r.Token = "pco9." + strings.TrimPrefix(r.Token, pki.JoinPrefix)
		},
		"no secret": func(t *testing.T, _ *joinSetup, r *JoinRequest) {
			r.Token = reencoded(t, r.Token, func(m map[string]any) { delete(m, "secret") })
		},
		"another secret": func(t *testing.T, _ *joinSetup, r *JoinRequest) {
			r.Token = reencoded(t, r.Token, func(m map[string]any) {
				m["secret"] = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
			})
		},
		"no node":      func(_ *testing.T, _ *joinSetup, r *JoinRequest) { r.Node = "" },
		"another node": func(_ *testing.T, _ *joinSetup, r *JoinRequest) { r.Node = "pve3" },
		"the token rewritten for another node": func(t *testing.T, _ *joinSetup, r *JoinRequest) {
			r.Token = reencoded(t, r.Token, func(m map[string]any) { m["node"] = "pve3" })
			r.Node = "pve3"
		},
		"another install": func(t *testing.T, j *joinSetup, r *JoinRequest) {
			tok, err := pki.NewJoinToken("fedcba987654", testNode, []string{j.h.addr.String()}, other, j.h.clock.now(), rand.Reader)
			require.NoError(t, err)
			r.Token = tok.Encode()
		},
		"another CA": func(t *testing.T, j *joinSetup, r *JoinRequest) {
			r.Token = reencoded(t, r.Token, func(m map[string]any) { m["ca"] = strings.Repeat("0", 64) })
		},
		"expired": func(t *testing.T, j *joinSetup, r *JoinRequest) {
			j.h.clock.advance(pki.JoinTokenTTL)
			t.Cleanup(func() { j.h.clock.advance(-pki.JoinTokenTTL) })
		},
		"no vmid":         func(_ *testing.T, _ *joinSetup, r *JoinRequest) { r.VMID = 0 },
		"no csr":          func(_ *testing.T, _ *joinSetup, r *JoinRequest) { r.CSR = nil },
		"a csr of junk":   func(_ *testing.T, _ *joinSetup, r *JoinRequest) { r.CSR = []byte("junk") },
		"a csr of a host": func(t *testing.T, _ *joinSetup, r *JoinRequest) { r.CSR = csrFor(t, hostID(testNode)) },
		"a csr of another node": func(t *testing.T, _ *joinSetup, r *JoinRequest) {
			r.CSR = csrFor(t, applianceID("pve3", 200))
		},
		"a csr of another container": func(t *testing.T, _ *joinSetup, r *JoinRequest) {
			r.CSR = csrFor(t, applianceID(testNode, 201))
		},
		"a csr of another install": func(t *testing.T, _ *joinSetup, r *JoinRequest) {
			r.CSR = csrFor(t, pki.Identity{Kind: pki.KindAppliance, InstallID: "fedcba987654", Node: testNode, VMID: 200})
		},
		"a csr of a P-384 key": func(t *testing.T, _ *joinSetup, r *JoinRequest) {
			key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
			require.NoError(t, err)
			csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{applianceID(testNode, 200).URI()}}, key)
			require.NoError(t, err)
			r.CSR = csr
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			j := newJoinSetup(t)
			req := j.req()
			edit(t, j, &req)
			require.Equal(t, RefusalRefused, refusalOf(t, j.send(t, req)).Code)
			require.Empty(t, j.uses(t), "the token was not used")
			require.Zero(t, j.joined.Load(), "the controller was not asked")
		})
	}
}

func TestATokenWhoseExpiryWasRewrittenIsRefusedByTheStoredOne(t *testing.T) {
	j := newJoinSetup(t)
	j.h.clock.advance(pki.JoinTokenTTL)
	req := j.req()
	req.Token = reencoded(t, req.Token, func(m map[string]any) {
		m["exp"] = j.h.clock.now().Add(time.Hour).Format(time.RFC3339)
	})
	require.Equal(t, RefusalRefused, refusalOf(t, j.send(t, req)).Code)
	require.Zero(t, j.joined.Load())
}

func TestAJoinIsRefusedWhileJoinsAreOff(t *testing.T) {
	j := newJoinSetup(t)
	h := newHarness(t)
	h.ctrl.setJoin(func(pki.JoinToken, []byte, netip.Addr, JoinFacts) ([]byte, error) {
		t.Fatal("a host controller asked to join")
		return nil, nil
	})
	body, err := json.Marshal(j.req())
	require.NoError(t, err)
	require.Equal(t, RefusalRefused, refusalOf(t, h.post(h.bare(), "/v1/agent/join", body)).Code)
}

func TestAJoinFromASourceThatMayNotJoinIsRefused(t *testing.T) {
	j := newJoinSetup(t)
	j.h.auth.allowJoin(loopback, false)
	require.Equal(t, RefusalRefused, refusalOf(t, j.send(t, j.req())).Code)
	require.Empty(t, j.uses(t))
}

func TestACertificateTheControllerGotWrongIsNotHandedOut(t *testing.T) {
	cases := map[string]func(j *joinSetup, csr []byte, facts JoinFacts) (*x509.Certificate, error){
		"another identity": func(j *joinSetup, csr []byte, facts JoinFacts) (*x509.Certificate, error) {
			key, err := pki.NewKey(rand.Reader)
			if err != nil {
				return nil, err
			}
			other, err := pki.NewCSR(key, applianceID("pve3", facts.VMID), rand.Reader)
			if err != nil {
				return nil, err
			}
			return j.h.ca.IssueClient(other, applianceID("pve3", facts.VMID), j.h.clock.now(), rand.Reader)
		},
		"another key": func(j *joinSetup, _ []byte, facts JoinFacts) (*x509.Certificate, error) {
			key, err := pki.NewKey(rand.Reader)
			if err != nil {
				return nil, err
			}
			other, err := pki.NewCSR(key, applianceID(facts.Node, facts.VMID), rand.Reader)
			if err != nil {
				return nil, err
			}
			return j.h.ca.IssueClient(other, applianceID(facts.Node, facts.VMID), j.h.clock.now(), rand.Reader)
		},
		"another CA": func(j *joinSetup, csr []byte, facts JoinFacts) (*x509.Certificate, error) {
			ca, err := pki.NewCA(testInstall, t0, rand.Reader)
			if err != nil {
				return nil, err
			}
			return ca.IssueClient(csr, applianceID(facts.Node, facts.VMID), j.h.clock.now(), rand.Reader)
		},
	}
	for name, issue := range cases {
		t.Run(name, func(t *testing.T) {
			j := newJoinSetup(t)
			j.issue = func(csr []byte, facts JoinFacts) (*x509.Certificate, error) { return issue(j, csr, facts) }
			require.Equal(t, RefusalRefused, refusalOf(t, j.send(t, j.req())).Code)
		})
	}
}

func TestAJoinTooLargeIsRefused(t *testing.T) {
	j := newJoinSetup(t)
	resp := j.h.post(j.h.bare(), "/v1/agent/join", make([]byte, maxJoinBody+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestTheJoinIsRateLimitedPerSource(t *testing.T) {
	j := newJoinSetup(t)
	h := j.h
	c := h.bare()
	bad := []byte(`{"token":"nothing"}`)
	for i := range joinPerMinute {
		resp := h.post(c, "/v1/agent/join", bad)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "attempt %d", i)
	}
	resp := h.post(c, "/v1/agent/join", bad)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "60", resp.Header.Get("Retry-After"))
	resp = j.send(t, j.req())
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "a good token waits its minute as well")
	require.Empty(t, j.uses(t))

	h.clock.advance(time.Minute)
	require.Equal(t, http.StatusOK, j.send(t, j.req()).StatusCode)
}

func TestUseTokenFindsTheTokenByItsSecretAndNode(t *testing.T) {
	st := openStore(t)
	ca, err := pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	tok1, err := pki.NewJoinToken(testInstall, testNode, []string{"10.0.0.1:8644"}, ca, t0, rand.Reader)
	require.NoError(t, err)
	tok2, err := pki.NewJoinToken(testInstall, testNode, []string{"10.0.0.1:8644"}, ca, t0, rand.Reader)
	require.NoError(t, err)
	for i, tok := range []pki.JoinToken{tok1, tok2} {
		require.NoError(t, st.SaveJoinToken(store.JoinRecord{
			ID: "tok" + string(rune('1'+i)), Node: testNode, SecretSHA256: tok.SecretHash(), CreatedAt: t0, Expires: tok.Expires,
		}))
	}
	use := store.JoinUse{At: t0, Source: loopback, VMID: 200}
	rec, err := UseToken(st, tok2, use)
	require.NoError(t, err)
	require.Equal(t, "tok2", rec.ID)
	require.Len(t, rec.Uses, 1)

	rec, err = UseToken(st, tok2, use)
	require.NoError(t, err)
	require.Len(t, rec.Uses, 2, "the second use is recorded, for the controller to judge")

	t.Run("a record of another node with the same secret is left alone", func(t *testing.T) {
		require.NoError(t, st.SaveJoinToken(store.JoinRecord{
			ID: "tok3", Node: "pve3", SecretSHA256: tok1.SecretHash(), CreatedAt: t0, Expires: tok1.Expires,
		}))
		rec, err := UseToken(st, tok1, use)
		require.NoError(t, err)
		require.Equal(t, "tok1", rec.ID)
		other, _, _, err := st.JoinToken("tok3")
		require.NoError(t, err)
		require.Empty(t, other.Uses)
	})
	t.Run("unknown secret", func(t *testing.T) {
		tok, err := pki.NewJoinToken(testInstall, testNode, []string{"10.0.0.1:8644"}, ca, t0, rand.Reader)
		require.NoError(t, err)
		_, err = UseToken(st, tok, use)
		require.ErrorIs(t, err, store.ErrNoJoinToken)
		require.NotContains(t, err.Error(), tok.SecretHash())
	})
	t.Run("a token without a secret", func(t *testing.T) {
		_, err := UseToken(st, pki.JoinToken{Node: testNode}, use)
		require.ErrorIs(t, err, store.ErrNoJoinToken)
	})
}

// TestTwoUsesOfATokenAtOnceHaveOneFirst races two uses without the server's
// lock: the store alone keeps exactly one of them the first.
func TestTwoUsesOfATokenAtOnceHaveOneFirst(t *testing.T) {
	ca, err := pki.NewCA(testInstall, t0, rand.Reader)
	require.NoError(t, err)
	tok, err := pki.NewJoinToken(testInstall, testNode, []string{"10.0.0.1:8644"}, ca, t0, rand.Reader)
	require.NoError(t, err)
	for range 20 {
		st := openStore(t)
		require.NoError(t, st.SaveJoinToken(store.JoinRecord{
			ID: "tok1", Node: testNode, SecretSHA256: tok.SecretHash(), CreatedAt: t0, Expires: tok.Expires,
		}))
		var wg sync.WaitGroup
		var firsts atomic.Int32
		start := make(chan struct{})
		for vmid := range 2 {
			wg.Go(func() {
				<-start
				rec, err := UseToken(st, tok, store.JoinUse{At: t0, Source: loopback, VMID: 200 + vmid})
				if err == nil && len(rec.Uses) == 1 {
					firsts.Add(1)
				}
			})
		}
		close(start)
		wg.Wait()
		require.Equal(t, int32(1), firsts.Load())
		rec, _, _, err := st.JoinToken("tok1")
		require.NoError(t, err)
		require.Len(t, rec.Uses, 2)
	}
}
