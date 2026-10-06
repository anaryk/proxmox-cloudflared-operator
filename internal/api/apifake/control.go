package apifake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

var _ api.Engine = (*Engine)(nil)

// maxControlBody bounds what a control takes: a state of the large
// scenarios is a few MiB.
const maxControlBody = 32 << 20

// Refusal is what POST /refuse takes: the method of api.Engine whose next
// call fails, the code of the answer (invalid, not_found, refused,
// unavailable, holder_changed or internal), its message and, for invalid,
// the field it is about. The credential, for AddCredential only, is the view
// of the credential as checked, which the answer carries when it is
// checked, as the daemon's refusal of a token does.
type Refusal struct {
	Method     string                 `json:"method"`
	Code       string                 `json:"code"`
	Message    string                 `json:"message"`
	Field      string                 `json:"field,omitempty"`
	Credential *engine.CredentialView `json:"credential,omitempty"`
}

// codes are the sentinels of the engine the API answers each code by.
var codes = map[string]error{
	"invalid":        engine.ErrInvalid,
	"not_found":      engine.ErrNotFound,
	"refused":        engine.ErrRefused,
	"unavailable":    engine.ErrBusy,
	"holder_changed": doctor.ErrHolderChanged,
	"internal":       nil,
}

// Refuse makes the next call of a method fail with the answer r asks for.
// Only a method of api.Engine that can fail can be refused.
func (e *Engine) Refuse(r Refusal) error {
	if !slices.Contains(refusable(), r.Method) {
		return fmt.Errorf("%q is no method of api.Engine that can fail; those are %v", r.Method, refusable())
	}
	sentinel, ok := codes[r.Code]
	switch {
	case !ok:
		return fmt.Errorf("%q is no code to refuse with: want invalid, not_found, refused, unavailable, holder_changed or internal", r.Code)
	case r.Message == "":
		return errors.New("a refusal needs a message")
	case r.Field != "" && r.Code != "invalid":
		return errors.New("only an invalid request is about a field")
	case r.Credential != nil && r.Method != "AddCredential":
		return errors.New("only the refusal of AddCredential carries a credential")
	}
	ref := refusal{err: &failure{sentinel, r.Message}, credential: r.Credential}
	if r.Field != "" {
		ref.err = &engine.FieldError{Field: r.Field, Err: errors.New(r.Message)}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refusals[r.Method] = ref
	return nil
}

// refusable are the methods of api.Engine that return an error.
func refusable() []string {
	t := reflect.TypeFor[api.Engine]()
	errType := reflect.TypeFor[error]()
	var out []string
	for i := range t.NumMethod() {
		m := t.Method(i)
		if n := m.Type.NumOut(); n > 0 && m.Type.Out(n-1) == errType {
			out = append(out, m.Name)
		}
	}
	return out
}

// ClearCalls forgets the calls recorded so far.
func (e *Engine) ClearCalls() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = nil
}

// Control serves the controls of the engine, for the browser suite and for
// development. It changes what the daemon serves without any check, so it
// must listen on loopback only, and it refuses a request for another host
// or from a page of another origin: a page open in a browser could
// otherwise steer it.
//
//	POST   /state           the body is a state, served from now on; the streams are told
//	POST   /cycle           a cycle, now, also while the streams are paused
//	POST   /event           the body is an event, or a list of them, added as one batch
//	POST   /traffic         the body is a TrafficChange
//	POST   /refuse          the body is a Refusal: the next call of the method fails
//	POST   /reset           back to the scenario as loaded, without a restart (Engine.Reset)
//	GET    /calls           the calls of the engine, oldest first; ?method= picks some
//	DELETE /calls           forgets them
//	POST   /streams/drop    ends every stream, as a restart would, without a new boot
//	POST   /streams/pause   stops the notices of every stream, and the cycles
//	POST   /streams/resume  sends them again
//	POST   /boot            restarts the daemon: a new boot, and every stream ends
//	GET    /subscribers     {"subscribers": n}, the streams open now
func (e *Engine) Control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /state", func(w http.ResponseWriter, r *http.Request) {
		var st engine.State
		if !decodeBody(w, r, &st) {
			return
		}
		answer(w, http.StatusOK, map[string]string{"digest": e.SetState(st)})
	})
	mux.HandleFunc("POST /event", func(w http.ResponseWriter, r *http.Request) {
		var events []engine.Event
		data, ok := readBody(w, r)
		if !ok {
			return
		}
		if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] != '[' {
			data = append(append([]byte("["), trimmed...), ']')
		}
		if err := strict(data, &events); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		answer(w, http.StatusOK, e.AddEvents(events...))
	})
	mux.HandleFunc("POST /traffic", func(w http.ResponseWriter, r *http.Request) {
		var c TrafficChange
		if !decodeBody(w, r, &c) {
			return
		}
		if err := e.ChangeTraffic(c); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /refuse", func(w http.ResponseWriter, r *http.Request) {
		var ref Refusal
		if !decodeBody(w, r, &ref) {
			return
		}
		if err := e.Refuse(ref); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, _ *http.Request) {
		if err := e.Reset(); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /calls", func(w http.ResponseWriter, r *http.Request) {
		methods := r.URL.Query()["method"]
		calls := slices.DeleteFunc(e.Calls(), func(c Call) bool { return len(methods) > 0 && !slices.Contains(methods, c.Method) })
		answer(w, http.StatusOK, nonNil(calls))
	})
	mux.HandleFunc("DELETE /calls", func(w http.ResponseWriter, _ *http.Request) {
		e.ClearCalls()
		w.WriteHeader(http.StatusNoContent)
	})
	for path, do := range map[string]func(){
		"POST /cycle": e.Cycle, "POST /streams/drop": e.DropStreams, "POST /streams/pause": e.Pause,
		"POST /streams/resume": e.Resume, "POST /boot": e.Restart,
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			do()
			w.WriteHeader(http.StatusNoContent)
		})
	}
	mux.HandleFunc("GET /subscribers", func(w http.ResponseWriter, _ *http.Request) {
		answer(w, http.StatusOK, map[string]int{"subscribers": e.Subscribers()})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := local(r); err != nil {
			fail(w, http.StatusForbidden, err)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// local refuses a request whose Host is not the loopback address the control
// listens on, by its address or as localhost, and one that names an Origin
// other than that, as a page of another origin does: a name that resolves
// to loopback only now, as with DNS rebinding, is not taken either.
func local(r *http.Request) error {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	listening, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	var at netip.AddrPort
	if listening != nil {
		at, _ = netip.ParseAddrPort(listening.String())
	}
	addr, err := netip.ParseAddr(host)
	switch {
	case host == "localhost" && (!at.IsValid() || at.Addr().IsLoopback()):
	case err == nil && addr.IsLoopback() && (!at.IsValid() || addr == at.Addr()):
	default:
		return fmt.Errorf("the controls answer for the loopback address they listen on, not for host %q", r.Host)
	}
	if at.IsValid() && port != "" && port != fmt.Sprint(at.Port()) {
		return fmt.Errorf("the controls listen on port %d, not %s", at.Port(), port)
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		return fmt.Errorf("the controls take no request of a page of %s", origin)
	}
	return nil
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxControlBody))
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("reading the body: %w", err))
		return nil, false
	}
	return data, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	data, ok := readBody(w, r)
	if !ok {
		return false
	}
	if err := strict(data, v); err != nil {
		fail(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

// strict decodes JSON that has nothing the type has no field for.
func strict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decoding the body: %w", err)
	}
	return nil
}

func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	answer(w, status, map[string]string{"error": err.Error()})
}
