package cffake_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// What Cloudflare refuses and the handler has to refuse as well, so that a
// client that stops short of it is found out.

func TestABodyHasToSayItIsJSON(t *testing.T) {
	fx := newWireFixture()
	tun := "/client/v4/accounts/" + acct + "/cfd_tunnel"
	records := "/client/v4/zones/" + zone + "/dns_records"
	full := `{"type":"A","name":"a.example.com","content":"192.0.2.3","proxied":false,"comment":"","ttl":1}`
	for _, req := range []struct{ method, path, body string }{
		{"POST", tun, `{"name":"pco-ct"}`},
		{"PUT", tun + "/" + fx.tunnel + "/configurations", `{"config":{"ingress":[{"service":"http_status:404"}]}}`},
		{"POST", records, `{"type":"A","name":"ct.example.com","content":"192.0.2.9"}`},
		{"PATCH", records + "/" + fx.record, full},
	} {
		for _, contentType := range []string{
			"", "text/plain", "application/x-www-form-urlencoded", "application/xml", "application/jsonp",
			"application/json-patch+json", "application/jsonx", "json", "application/", "/json", "application/json,text/plain", `"application/json"`,
		} {
			t.Run(req.method+" "+req.path+" as "+contentType, func(t *testing.T) {
				rec := doTyped(t, fx.h, token, contentType, req.method, req.path, req.body)
				requireRefused(t, rec, http.StatusUnsupportedMediaType, codeUnsupportedType, "Content-Type must be application/json")
			})
		}
		for _, contentType := range []string{
			"application/json", "Application/JSON", "application/json; charset=utf-8", "application/json;charset=UTF-8", " application/json ",
		} {
			t.Run(req.method+" "+req.path+" as "+contentType, func(t *testing.T) {
				rec := doTyped(t, newWireFixture().h, token, contentType, req.method, req.path, req.body)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			})
		}
	}

	t.Run("what is refused is refused before the fake is asked", func(t *testing.T) {
		fx := newWireFixture()
		rec := doTyped(t, fx.h, token, "text/plain", "POST", tun, `{"name":"x"}`)
		require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
		require.Empty(t, fx.f.Calls())
	})

	t.Run("a request that has no body needs no type", func(t *testing.T) {
		for _, ep := range fx.endpoints() {
			if ep.body != "" {
				continue
			}
			rec := doTyped(t, fx.h, token, "", ep.method, ep.path, "")
			require.NotEqual(t, http.StatusUnsupportedMediaType, rec.Code, "%s %s", ep.method, ep.path)
		}
	})
}

func TestRecordTTLsAreOneOrFromThirtyToADay(t *testing.T) {
	records := "/client/v4/zones/" + zone + "/dns_records"
	body := func(ttl string) string {
		return `{"type":"A","name":"ttl.example.com","content":"192.0.2.9","proxied":false,"comment":"","ttl":` + ttl + `}`
	}
	for _, ttl := range []string{"0", "-1", "2", "29", "86401", "100000", "1000000000000", "1.5", `"1"`} {
		t.Run("refused "+ttl, func(t *testing.T) {
			fx := newWireFixture()
			for _, req := range []struct{ method, path string }{{"POST", records}, {"PATCH", records + "/" + fx.record}} {
				rec := do(t, fx.h, req.method, req.path, body(ttl))
				require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", req.method, rec.Body.String())
				decode(t, rec)
			}
			require.Empty(t, fx.f.Calls())
			require.Len(t, fx.f.RecordsIn(zone), 2)
		})
	}
	requireRefused(t, do(t, newWireFixture().h, "POST", records, body("0")), http.StatusBadRequest, codeBadRequest, "ttl must be 1 or from 30 to 86400")

	for _, ttl := range []string{"1", "30", "31", "300", "86400"} {
		t.Run("accepted "+ttl, func(t *testing.T) {
			rec := do(t, newWireFixture().h, "POST", records, body(ttl))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			fx := newWireFixture()
			rec = do(t, fx.h, "PATCH", records+"/"+fx.record, body(ttl))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}

	t.Run("a new record needs none", func(t *testing.T) {
		fx := newWireFixture()
		rec := do(t, fx.h, "POST", records, `{"type":"A","name":"ttl.example.com","content":"192.0.2.9"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"ttl":1`)
	})
}

func TestTheClientSendsOnlyATTLTheAPIAccepts(t *testing.T) {
	f := cffake.New()
	std(f)
	c := client(t, f)
	for _, ttl := range []int{0, -1, 1, 30, 300, 86400} {
		r := rec("A", fmt.Sprintf("t%d.example.com", ttl+1), "192.0.2.1")
		r.TTL = ttl
		_, err := c.CreateRecord(ctx, zone, r)
		require.NoError(t, err, "ttl %d", ttl)
	}
	_, err := c.CreateRecord(ctx, zone, cfapi.Record{Type: "A", Name: "low.example.com", Content: "192.0.2.1", TTL: 5})
	require.True(t, isStatus(http.StatusBadRequest)(err), "what the client is told to send is refused by the API: %v", err)
}

func TestAnExpiredOrDisabledTokenShowsOnlyAtVerify(t *testing.T) {
	// A simplification: Cloudflare refuses every call made with a token that
	// expired or was disabled, the fake keeps answering them. Deny is the way
	// to model a token that is refused.
	for _, status := range []string{"expired", "disabled"} {
		t.Run(status, func(t *testing.T) {
			fx := newWireFixture()
			fx.f.SetTokenStatus(status, nil)

			for _, ep := range fx.endpoints() {
				rec := do(t, fx.h, ep.method, ep.path, ep.body)
				require.Equal(t, ep.status, rec.Code, "%s %s: %s", ep.method, ep.path, rec.Body.String())
			}
			rec := do(t, fx.h, http.MethodGet, "/client/v4/user/tokens/verify", "")
			require.Equal(t, http.StatusOK, rec.Code)
			require.JSONEq(t, `{"id":"token-1","status":"`+status+`"}`, string(decode(t, rec).Result))

			fx.f.Deny("zones")
			requireRefused(t, do(t, fx.h, http.MethodGet, "/client/v4/zones", ""), http.StatusForbidden, 10000, "Authentication error")
		})
	}
}

func TestInventedCodesAreCloudflareLike(t *testing.T) {
	for status := 400; status <= 599; status++ {
		fx := newWireFixture()
		fx.f.FailNext("zones", 1, &cfapi.Error{Status: status})

		rec := do(t, fx.h, http.MethodGet, "/client/v4/zones", "")

		require.Equal(t, status, rec.Code)
		code := decode(t, rec).Errors[0].Code
		require.True(t, code >= 1000 || code == 971, "status %d has the code %d, which cannot be Cloudflare's", status, code)
	}
	fx := newWireFixture()
	for _, rec := range []*httptest.ResponseRecorder{
		do(t, fx.h, "GET", "/client/v4/zones?x=1", ""),
		do(t, fx.h, "POST", "/client/v4/accounts/acct1/cfd_tunnel", "{"),
		doTyped(t, fx.h, token, "text/plain", "POST", "/client/v4/accounts/acct1/cfd_tunnel", "{"),
		do(t, fx.h, "GET", "/client/v4/nope", ""),
		do(t, fx.h, "POST", "/client/v4/zones", ""),
		doAs(t, fx.h, "", "GET", "/client/v4/zones", ""),
	} {
		require.GreaterOrEqual(t, decode(t, rec).Errors[0].Code, 1000)
	}
}
