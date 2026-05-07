//go:build desktop

package main

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

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
// http://127.0.0.1:<port>/api/ws[/runs/...] address.
//
// In local desktop mode the embedded server runs with DisableAuth=true so
// no token forwarding is needed — protection comes from loopback bind +
// Origin allowlisting.
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
// cached proxy when the URL hasn't changed.
func (h *assetProxyHandler) proxyFor(serverURL string) (*httputil.ReverseProxy, error) {
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
		// loopback allowlist. Origin rewriting is the same trick editor's
		// vite dev proxy uses (editor/vite.config.ts).
		if req.Header.Get("Origin") != "" {
			req.Header.Set("Origin", "http://"+targetHost)
		}
	}
	h.cached = &cachedProxy{target: target, proxy: proxy}
	return proxy, nil
}

func (h *assetProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.app.mu.RLock()
	serverURL := h.app.serverURL
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

	proxy, err := h.proxyFor(serverURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy.ServeHTTP(w, r)
}
