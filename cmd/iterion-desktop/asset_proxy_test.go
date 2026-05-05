//go:build desktop

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAssetProxy_AttachesSessionCookie pins the contract that lets the SPA
// authenticate without ever knowing the token: the proxy strips any
// inbound iterion_session cookie (the AssetServer origin can never have a
// legitimate one) and re-adds the canonical token before forwarding. This
// is the single fix that makes window.go.main.App.* reachable on the
// editor SPA — before, the SPA at http://127.0.0.1:<port>/ had no Wails
// runtime injection because the AssetServer never saw its HTML; now the
// AssetServer hosts the SPA via this proxy, injects /wails/runtime.js +
// /wails/ipc.js, and the local server still gets authenticated calls.
func TestAssetProxy_AttachesSessionCookie(t *testing.T) {
	const tok = "session-token"
	var seenCookies string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCookies = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	app := &App{
		ctx:          context.Background(),
		serverURL:    upstream.URL + "/",
		sessionToken: tok,
	}
	h := newAssetProxyHandler(app)

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(seenCookies, sessionCookieName+"="+tok) {
		t.Errorf("upstream did not see session cookie: %q", seenCookies)
	}
}

// TestAssetProxy_StripsClientForgedSessionCookie defends against a future
// attack vector: any page (or stray Set-Cookie response) that manages to
// set iterion_session on the AssetServer origin should not be able to
// elevate its trust by having the proxy forward the client-supplied
// cookie. The proxy MUST drop incoming iterion_session cookies and only
// forward the canonical token from App.sessionToken.
func TestAssetProxy_StripsClientForgedSessionCookie(t *testing.T) {
	const realTok = "real-token"
	var seenCookies string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCookies = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	app := &App{
		ctx:          context.Background(),
		serverURL:    upstream.URL + "/",
		sessionToken: realTok,
	}
	h := newAssetProxyHandler(app)

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "forged"})
	req.AddCookie(&http.Cookie{Name: "other_cookie", Value: "preserved"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if strings.Contains(seenCookies, "iterion_session=forged") {
		t.Errorf("forged cookie reached upstream: %q", seenCookies)
	}
	if !strings.Contains(seenCookies, sessionCookieName+"="+realTok) {
		t.Errorf("real session cookie missing: %q", seenCookies)
	}
	if !strings.Contains(seenCookies, "other_cookie=preserved") {
		t.Errorf("non-iterion cookies must pass through, got %q", seenCookies)
	}
}

// TestAssetProxy_RewritesOriginToTarget pins the second piece of the puzzle:
// pkg/server's requireSafeOrigin checks the request's Origin header against
// a loopback allowlist. The SPA's true Origin is the Wails AssetServer
// (wails:// or http://wails.localhost), which the local server doesn't
// know about. The proxy rewrites Origin to the upstream's loopback host so
// the server-side allowlist + WS-upgrader's CheckOrigin both pass without
// having to teach pkg/server the AssetServer's origin scheme.
func TestAssetProxy_RewritesOriginToTarget(t *testing.T) {
	var seenOrigin string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenOrigin = r.Header.Get("Origin")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	app := &App{
		ctx:          context.Background(),
		serverURL:    upstream.URL + "/",
		sessionToken: "tok",
	}
	h := newAssetProxyHandler(app)

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	req.Header.Set("Origin", "wails://wails")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// upstream.URL is "http://127.0.0.1:NNNNN"; the rewritten Origin uses
	// the same scheme + host (defense-in-depth: a real loopback origin the
	// server's allowlist already accepts).
	wantPrefix := "http://" + strings.TrimPrefix(upstream.URL, "http://")
	if seenOrigin != wantPrefix {
		t.Errorf("Origin rewritten to %q, want %q", seenOrigin, wantPrefix)
	}
}

// TestAssetProxy_FailsClosedWhenServerNotReady proves the proxy doesn't
// silently leak through to a half-initialised state: if the App has no
// serverURL yet (e.g. window opens before the embedded server bound), the
// caller sees a 503 explanatory message. Wails' AssetServer wraps this in
// its runtime-injection path for "/" but streams it as-is for non-HTML
// paths — either way the user/dev can tell the editor isn't ready.
func TestAssetProxy_FailsClosedWhenServerNotReady(t *testing.T) {
	app := &App{
		ctx:          context.Background(),
		serverURL:    "",
		sessionToken: "tok",
	}
	h := newAssetProxyHandler(app)

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	body, _ := io.ReadAll(rr.Body)
	if !strings.Contains(string(body), "starting") {
		t.Errorf("expected user-friendly body mentioning startup, got %q", body)
	}
}

// TestAssetProxy_RebindsAfterServerURLChange pins the project-switch
// invariant: when a.serverURL changes (project switch rebinds the embedded
// server on a fresh ephemeral port via Port=0), the next proxy call must
// route to the NEW upstream — not the cached old one. A regression here
// would silently send all proxied calls to a dead listener after every
// project switch.
func TestAssetProxy_RebindsAfterServerURLChange(t *testing.T) {
	var hits1, hits2 int
	upstream1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits1++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream1.Close)
	upstream2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits2++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream2.Close)

	app := &App{
		ctx:          context.Background(),
		serverURL:    upstream1.URL + "/",
		sessionToken: "tok",
	}
	h := newAssetProxyHandler(app)

	// First request: routes to upstream1.
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if hits1 != 1 || hits2 != 0 {
		t.Fatalf("after first request: hits1=%d hits2=%d, want 1/0", hits1, hits2)
	}

	// Simulate project switch: serverURL flips to upstream2.
	app.mu.Lock()
	app.serverURL = upstream2.URL + "/"
	app.mu.Unlock()

	req = httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if hits1 != 1 || hits2 != 1 {
		t.Errorf("after project switch: hits1=%d hits2=%d, want 1/1 (proxy must rebind on serverURL change)", hits1, hits2)
	}
}
