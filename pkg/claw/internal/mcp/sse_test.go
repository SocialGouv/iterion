package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSSETransport_StaticAuthHeader(t *testing.T) {
	var receivedAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(Response{ID: 1})
	}))
	defer srv.Close()

	tr := NewSSETransport(srv.URL, "Bearer static-token")
	tr.httpClient = srv.Client()

	if _, err := tr.Send(context.Background(), Request{ID: 1, Method: "ping"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := receivedAuth.Load().(string); got != "Bearer static-token" {
		t.Errorf("expected static auth, got %q", got)
	}
}

func TestSSETransport_DynamicAuthFunc(t *testing.T) {
	var receivedAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(Response{ID: 1})
	}))
	defer srv.Close()

	var calls atomic.Int32
	tr := NewSSETransport(srv.URL, "Bearer fallback")
	tr.httpClient = srv.Client()
	tr.SetAuthFunc(func(ctx context.Context) (string, error) {
		calls.Add(1)
		return "Bearer dynamic-" + asString(calls.Load()), nil
	})

	if _, err := tr.Send(context.Background(), Request{ID: 1, Method: "ping"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := receivedAuth.Load().(string); got != "Bearer dynamic-1" {
		t.Errorf("expected dynamic auth, got %q", got)
	}

	if _, err := tr.Send(context.Background(), Request{ID: 2, Method: "ping"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := receivedAuth.Load().(string); got != "Bearer dynamic-2" {
		t.Errorf("expected dynamic auth refreshed, got %q", got)
	}
	if calls.Load() != 2 {
		t.Errorf("expected authFunc called 2x, got %d", calls.Load())
	}
}

func TestSSETransport_AuthFuncErrorAbortsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be reached when authFunc errors")
	}))
	defer srv.Close()

	tr := NewSSETransport(srv.URL, "")
	tr.httpClient = srv.Client()
	tr.SetAuthFunc(func(ctx context.Context) (string, error) {
		return "", errBoom
	})

	if _, err := tr.Send(context.Background(), Request{ID: 1, Method: "ping"}); err == nil {
		t.Fatal("expected error when authFunc fails")
	}
}

func TestSSETransport_AuthFuncReturnsEmptyHeader(t *testing.T) {
	// authFunc returning ("", nil) means "anonymous request" — the
	// transport must not set the header at all rather than sending
	// an empty Authorization line that breaks middleware regex
	// matchers.
	var hadAuthHeader atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := r.Header["Authorization"]
		hadAuthHeader.Store(ok)
		_ = json.NewEncoder(w).Encode(Response{ID: 1})
	}))
	defer srv.Close()

	tr := NewSSETransport(srv.URL, "Bearer should-not-be-used")
	tr.httpClient = srv.Client()
	tr.SetAuthFunc(func(ctx context.Context) (string, error) {
		return "", nil
	})

	if _, err := tr.Send(context.Background(), Request{ID: 1, Method: "ping"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if hadAuthHeader.Load() {
		t.Errorf("expected no Authorization header when authFunc returns empty string")
	}
}

func TestSSETransport_AuthFuncOverridesStaticHeader(t *testing.T) {
	// Static header set + authFunc set: the dynamic one MUST win,
	// otherwise rotating tokens via authFunc would silently emit the
	// stale static header.
	var lastHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastHeader.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(Response{ID: 1})
	}))
	defer srv.Close()

	tr := NewSSETransport(srv.URL, "Bearer STATIC")
	tr.httpClient = srv.Client()
	tr.SetAuthFunc(func(ctx context.Context) (string, error) {
		return "Bearer DYNAMIC", nil
	})

	if _, err := tr.Send(context.Background(), Request{ID: 1, Method: "ping"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := lastHeader.Load().(string); got != "Bearer DYNAMIC" {
		t.Errorf("expected DYNAMIC to win over STATIC, got %q", got)
	}
}

func TestSSETransport_AuthFuncSeesContext(t *testing.T) {
	// The ctx passed to Send should reach authFunc unchanged so it
	// can honour cancellation when refreshing tokens.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Response{ID: 1})
	}))
	defer srv.Close()

	type ctxKey struct{}
	var observed atomic.Value
	tr := NewSSETransport(srv.URL, "")
	tr.httpClient = srv.Client()
	tr.SetAuthFunc(func(ctx context.Context) (string, error) {
		if v := ctx.Value(ctxKey{}); v != nil {
			observed.Store(v)
		}
		return "", nil
	})

	parentCtx := context.WithValue(context.Background(), ctxKey{}, "marker-from-caller")
	if _, err := tr.Send(parentCtx, Request{ID: 1, Method: "ping"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, _ := observed.Load().(string); got != "marker-from-caller" {
		t.Errorf("authFunc did not receive caller's ctx, got %q", got)
	}
}

func TestSSETransport_NotifyHonoursAuthFunc(t *testing.T) {
	// Notify also uses resolveAuth — verify symmetry with Send.
	var lastHeader atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastHeader.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr := NewSSETransport(srv.URL, "")
	tr.httpClient = srv.Client()
	tr.SetAuthFunc(func(ctx context.Context) (string, error) {
		return "Bearer notify-token", nil
	})

	if err := tr.Notify(Notification{Method: "tick"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got := lastHeader.Load().(string); got != "Bearer notify-token" {
		t.Errorf("expected dynamic header in Notify, got %q", got)
	}
}

var errBoom = stringError("boom")

type stringError string

func (e stringError) Error() string { return string(e) }

func asString(n int32) string {
	switch n {
	case 1:
		return "1"
	case 2:
		return "2"
	}
	return "n"
}
