//go:build desktop

package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// sessionCookieName is the cookie name pkg/server's session middleware
// expects. Kept in lockstep with the constant of the same name in
// pkg/server/session.go — duplicated here rather than imported because
// pkg/server keeps the constant unexported (intentionally, to keep it from
// being a public extension point of the editor server).
const sessionCookieName = "iterion_session"

// assetProxyHandler is the http.Handler the desktop binary plugs into Wails'
// AssetServer. Wails treats it as the origin of all assets served at the
// AssetServer URL (wails:// on Mac/Linux, http://wails.localhost on Windows).
// We forward GET/POST/etc. requests to the embedded pkg/server (HTTP API +
// SPA static assets) and let Wails do its standard runtime injection on the
// HTML response, which makes window.go.main.App.* and window.runtime.*
// available to the editor SPA — exactly the cross-origin gap that motivated
// this proxy in the first place.
//
// WebSocket traffic NEVER reaches this handler: Wails' AssetServer
// short-circuits WS upgrades with 501 (intentional, AssetServer is HTTP-only).
// The editor SPA dials WS endpoints directly at the local server's
// http://127.0.0.1:<port>/api/ws[/runs/...] address, authenticated via the
// ?t=<sessionToken> query path the session middleware now accepts on
// non-bootstrap paths.
//
// The handler reads the current serverURL + sessionToken from App on every
// request because both can change across the App's lifetime: serverURL
// rebinds to a fresh ephemeral port on every project switch (pkg/cli.RunEditor
// uses Port=0), and sessionToken is generated once at startup but readable
// only after onStartup. Caching is per-target so a project switch invalidates
// the cached *httputil.ReverseProxy without leaking goroutines.
type assetProxyHandler struct {
	app *App

	mu     sync.Mutex
	cached *cachedProxy
}

type cachedProxy struct {
	target *url.URL
	proxy  *httputil.ReverseProxy
}

func newAssetProxyHandler(app *App) *assetProxyHandler {
	return &assetProxyHandler{app: app}
}

// proxyFor returns a *httputil.ReverseProxy targeting serverURL, reusing the
// cached proxy when the URL hasn't changed. The Director is closed over
// sessionToken so cookie injection happens once per proxy build.
func (h *assetProxyHandler) proxyFor(serverURL, sessionToken string) (*httputil.ReverseProxy, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cached != nil && h.cached.target.String() == serverURL {
		return h.cached.proxy, nil
	}
	target, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid serverURL: %w", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	targetHost := target.Host
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		// Force the Host so the inner server logs and Origin allowlist see
		// its own loopback host, not the AssetServer's "wails.localhost".
		req.Host = targetHost
		// Rewrite the Origin header to match the proxy target. Without this,
		// pkg/server/server.go requireSafeOrigin (and CORS reflection) would
		// reject every state-changing API call because the SPA's true Origin
		// is the AssetServer's wails:// origin, which is not in the
		// loopback allowlist. The session token on the cookie (added below)
		// is the actual authentication; Origin rewriting is the same trick
		// editor's vite dev proxy uses (editor/vite.config.ts).
		if req.Header.Get("Origin") != "" {
			req.Header.Set("Origin", "http://"+targetHost)
		}
		// Strip any inbound iterion_session cookie: the AssetServer origin
		// is never going to legitimately have one (the local server never
		// set it on this origin), and a malicious page injected via a
		// future Wails feature shouldn't be able to forge one. Re-attach
		// the canonical token cookie so the local server's middleware
		// authenticates the proxied call.
		stripCookie(req, sessionCookieName)
		if sessionToken != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
		}
	}
	h.cached = &cachedProxy{target: target, proxy: proxy}
	return proxy, nil
}

func (h *assetProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.app.mu.RLock()
	serverURL := h.app.serverURL
	sessionToken := h.app.sessionToken
	h.app.mu.RUnlock()

	if serverURL == "" {
		// Server not yet bound (or failed to start). Wails AssetServer's
		// runtime-injection wrapper records 5xx but doesn't substitute its
		// default index — the SPA bootstrap will simply retry on its next
		// load. Return a friendly message so the user sees something other
		// than a blank page if they manually navigate.
		http.Error(w, "Iterion editor server is starting…", http.StatusServiceUnavailable)
		return
	}

	proxy, err := h.proxyFor(serverURL, sessionToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy.ServeHTTP(w, r)
}

// stripCookie removes a single cookie by name from req.Header["Cookie"] in
// place, preserving the order of any remaining cookies. Used by the proxy
// director to drop client-supplied iterion_session cookies before re-adding
// the canonical one.
func stripCookie(req *http.Request, name string) {
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == name {
			continue
		}
		req.AddCookie(c)
	}
}
