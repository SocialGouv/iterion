//go:build desktop

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	iserver "github.com/SocialGouv/iterion/pkg/server"
)

// assetProxyHandler is the http.Handler the desktop binary plugs into Wails'
// AssetServer. Wails treats it as the origin of all assets served at the
// AssetServer URL (wails:// on Mac/Linux, http://wails.localhost on Windows).
//
// SPA assets (HTML, JS, CSS, images) are served from the GUI binary's own
// embed (pkg/server.StaticFS) so UI updates ship with the desktop binary
// and don't require a daemon restart. Only /api/* requests are forwarded
// to the daemon's embedded HTTP server — that's the inter-process contract.
// Wails' runtime injection still wraps HTML responses regardless of source,
// so window.go.main.App.* and window.runtime.* remain available.
//
// WebSocket traffic NEVER reaches this handler: Wails' AssetServer
// short-circuits WS upgrades with 501 (intentional, AssetServer is HTTP-only).
// The studio dials WS endpoints directly at the daemon's
// http://127.0.0.1:<port>/api/ws[/runs/...] address.
//
// In local desktop mode the embedded server runs with DisableAuth=true so
// no token forwarding is needed — protection comes from loopback bind +
// Origin allowlisting.
type assetProxyHandler struct {
	app *App

	spa http.Handler // serves the SPA from the GUI's embedded StaticFS

	mu     sync.Mutex
	cached *cachedProxy
}

type cachedProxy struct {
	target *url.URL
	jar    *cloudTokenJar // nil for a local connection; set for cloud
	proxy  *httputil.ReverseProxy
}

// cloudRoundTripper wraps the base transport for a CLOUD connection with a
// single refresh-and-retry on 401. The background refresh loop is the
// primary defense (it refreshes before expiry); this is the safety net for
// the race where a token lapses between refreshes. On a second failure it
// emits cloud:auth-expired and passes the 401 through so the SPA re-logs in.
type cloudRoundTripper struct {
	base http.RoundTripper
	app  *App
	jar  *cloudTokenJar
}

func (t *cloudRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body so the request can be replayed after a refresh.
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	// 401 → refresh once, then replay the request with the new token.
	if rerr := t.jar.refreshNow(req.Context()); rerr != nil {
		if ae := asCloudAuthError(rerr); ae != nil && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden) {
			t.app.emitCloudAuthExpired(t.jar.connID)
		}
		return resp, nil // pass the original 401 through
	}
	resp.Body.Close()
	req2 := req.Clone(req.Context())
	if body != nil {
		req2.Body = io.NopCloser(bytes.NewReader(body))
	}
	if tok := t.jar.AccessToken(); tok != "" {
		req2.Header.Set("Authorization", "Bearer "+tok)
	}
	return t.base.RoundTrip(req2)
}

func newAssetProxyHandler(app *App) *assetProxyHandler {
	subFS, err := fs.Sub(iserver.StaticFS, "static")
	if err != nil {
		// pkg/server panics in this case too — keep parity.
		log.Fatalf("desktop asset_proxy: sub-FS init failed: %v", err)
	}
	return &assetProxyHandler{
		app: app,
		spa: iserver.SPAHandler(subFS),
	}
}

// proxyFor returns a *httputil.ReverseProxy targeting serverURL, reusing the
// cached proxy when neither the URL nor the active cloud jar has changed. A
// non-nil jar means the target is a REMOTE cloud instance: the proxy becomes
// an authenticating tunnel — it injects the Bearer, strips the wails-origin
// cookies, harvests rotated auth cookies, and retries once on 401. A nil jar
// is the historical local behaviour (loopback, DisableAuth, no token).
func (h *assetProxyHandler) proxyFor(serverURL string, jar *cloudTokenJar) (*httputil.ReverseProxy, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cached != nil && h.cached.target.String() == serverURL && h.cached.jar == jar {
		return h.cached.proxy, nil
	}
	target, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid serverURL: %w", err)
	}
	// Use the Rewrite hook instead of the deprecated Director field
	// (deprecated Go 1.26; Rewrite has been available since 1.20).
	// SetURL replicates NewSingleHostReverseProxy's URL/scheme/host
	// stitching; the extra Out.Host + Origin tweaks are the same as
	// the old Director path.
	targetHost := target.Host
	targetScheme := target.Scheme
	if targetScheme == "" {
		targetScheme = "http"
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Force the Host so the inner server logs and Origin allowlist see
			// its own host, not the AssetServer's "wails.localhost".
			r.Out.Host = targetHost
			// Rewrite the Origin header to match the proxy target. Without this,
			// pkg/server/server.go requireSafeOrigin (and CORS reflection) would
			// reject every state-changing API call because the SPA's true Origin
			// is the AssetServer's wails:// origin, which is not in the
			// target's allowlist. Origin rewriting is the same trick the
			// studio's vite dev proxy uses (studio/vite.config.ts).
			if r.In.Header.Get("Origin") != "" {
				r.Out.Header.Set("Origin", targetScheme+"://"+targetHost)
			}
			if jar != nil {
				// Cloud: authenticating tunnel. Strip the wails-origin cookies
				// (junk to the cloud) and inject the current access token. The
				// cloudRoundTripper re-injects a refreshed token on a 401 retry.
				r.Out.Header.Del("Cookie")
				if tok := jar.AccessToken(); tok != "" {
					r.Out.Header.Set("Authorization", "Bearer "+tok)
				}
			}
		},
	}
	if jar != nil {
		proxy.Transport = &cloudRoundTripper{base: http.DefaultTransport, app: h.app, jar: jar}
		proxy.ModifyResponse = func(resp *http.Response) error {
			// Harvest tokens rotated by SPA-driven auth mutations (e.g. an
			// org/team switch) from the Set-Cookie header, then strip those
			// cookies so they never reach the webview.
			var access, refresh string
			for _, c := range resp.Cookies() {
				switch c.Name {
				case cloudAuthCookieName:
					access = c.Value
				case cloudRefreshCookieName:
					refresh = c.Value
				}
			}
			if access != "" || refresh != "" {
				if err := jar.applyRotation(access, refresh); err != nil {
					log.Printf("desktop: cloud token rotation persist failed: %v", err)
				}
				stripSetCookies(resp, cloudAuthCookieName, cloudRefreshCookieName)
			}
			return nil
		}
	}
	h.cached = &cachedProxy{target: target, jar: jar, proxy: proxy}
	return proxy, nil
}

func (h *assetProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// SPA assets (everything not under /api/) are served from the GUI's
	// own embed. The SPA loads instantly even before the daemon is up;
	// the first /api/* fetch then waits on serverURL below.
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		h.spa.ServeHTTP(w, r)
		return
	}

	serverURL := h.waitForServerURL(r.Context(), 30*time.Second)
	if serverURL == "" {
		// Still no URL after the wait window — either the embedded
		// server failed to bind or daemon spawn timed out. The SPA is
		// already loaded at this point so the JS can surface a useful
		// error from the rejected fetch rather than a blank page.
		http.Error(w, "Iterion studio server failed to start within 30s — check daemon logs at ~/.iterion/daemons/", http.StatusServiceUnavailable)
		return
	}

	// For a cloud connection the proxy becomes an authenticating tunnel keyed
	// on the active token jar; for local it stays nil (loopback, DisableAuth).
	jar := h.app.activeCloudJar()

	proxy, err := h.proxyFor(serverURL, jar)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy.ServeHTTP(w, r)
}

// waitForServerURL polls a.serverURL with a short backoff until the
// onStartup flow finishes attaching/spawning. The WebView issues its
// initial GET / within ~100ms of process launch — well before the
// daemon spawn polls succeed (cli.RunStudio cold start is 5-10s). If
// we return 5xx on that first hit the WebView shows the error text
// permanently because no JS has loaded to retry. Blocking here makes
// the WebView appear to "load slowly" instead of showing a stuck
// error message, and the eventual load is the real SPA.
func (h *assetProxyHandler) waitForServerURL(ctx context.Context, max time.Duration) string {
	deadline := time.Now().Add(max)
	for {
		h.app.mu.RLock()
		serverURL := h.app.serverURL
		h.app.mu.RUnlock()
		if serverURL != "" {
			return serverURL
		}
		if time.Now().After(deadline) {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(150 * time.Millisecond):
		}
	}
}
