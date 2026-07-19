//go:build desktop

package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/SocialGouv/iterion/internal/httpx"
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

	spa   http.Handler // serves the SPA from the GUI's embedded StaticFS
	subFS fs.FS        // the "static" sub-FS, for scope-injected index.html

	mu      sync.Mutex
	caches  map[string]*cachedProxy // per-backend reverse proxies (multi-connection)
	indexMu sync.Mutex
	indexTL []byte // cached raw index.html (pre-injection)
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
		app:    app,
		spa:    iserver.SPAHandler(subFS),
		subFS:  subFS,
		caches: make(map[string]*cachedProxy),
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
	if h.caches == nil {
		h.caches = make(map[string]*cachedProxy)
	}
	if c, ok := h.caches[serverURL]; ok && c.jar == jar {
		return c.proxy, nil
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
	h.caches[serverURL] = &cachedProxy{target: target, jar: jar, proxy: proxy}
	return proxy, nil
}

func (h *assetProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Workspace panes: /x/<connID>/* is demultiplexed to the matching open
	// connection's backend (multi-connection mode). This must come first so a
	// scoped /x/<id>/api/... never falls into the legacy single-backend path.
	if strings.HasPrefix(r.URL.Path, "/x/") {
		h.serveScoped(w, r)
		return
	}

	// SPA assets (everything not under /api/) are served from the GUI's
	// own embed — shared by the workspace shell (unscoped) and every pane
	// (which references /assets/* at the root). The SPA loads instantly even
	// before the daemon is up; the first /api/* fetch then waits on serverURL.
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

// serveScoped handles /x/<connID>/* traffic for a workspace pane:
//   - /x/<id>/_ws/info      → JSON connWsInfo (how to dial the pane's WS)
//   - /x/<id>/_ws/ticket    → JSON {"ticket": …} (POST; cloud single-use WS ticket)
//   - /x/<id>/api/…         → demuxed to the connection's backend (Bearer for cloud)
//   - /x/<id>/…  (anything else) → the studio SPA index.html, scope-injected
//
// The connID is looked up in the workspace registry; an unknown/closed
// connection is a 404 so the pane surfaces a clear error instead of leaking to
// the wrong backend.
func (h *assetProxyHandler) serveScoped(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/x/")
	connID, sub, _ := strings.Cut(rest, "/")
	if connID == "" {
		http.Error(w, "missing connection id", http.StatusBadRequest)
		return
	}
	c := h.app.lookupConn(connID)
	if c == nil {
		http.Error(w, "connection not open: "+connID, http.StatusNotFound)
		return
	}

	switch {
	case sub == "_ws/info":
		info, err := wsInfoForConn(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, info)
		return
	case sub == "_ws/ticket":
		ticket, err := h.app.mintWsTicketForConn(r.Context(), c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]string{"ticket": ticket})
		return
	case sub == "api" || strings.HasPrefix(sub, "api/"):
		// Rewrite the path to the backend-native form (/api/…) before proxying
		// so the reverse proxy's SetURL joins it onto the target root cleanly.
		r.URL.Path = "/" + sub
		proxy, err := h.proxyFor(c.serverURL, c.jar)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		proxy.ServeHTTP(w, r)
		return
	default:
		h.serveScopedIndex(w, "/x/"+connID)
		return
	}
}

// serveScopedIndex serves the studio SPA entry document with the pane's scope
// injected as window.__ITERION_SCOPE__. The pane's client reads it to prefix
// every /api call and resolve its WS base, and passes it to wouter as the
// router base — so one shared bundle renders scoped to whichever backend the
// pane points at. Shared /assets/* are referenced at the root and served
// unscoped, so no per-scope asset duplication.
func (h *assetProxyHandler) serveScopedIndex(w http.ResponseWriter, scope string) {
	raw, err := h.indexHTML()
	if err != nil {
		http.Error(w, "index.html unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	inject := []byte(`<script>window.__ITERION_SCOPE__=` + jsonString(scope) + `;</script>`)
	var out []byte
	if i := bytes.Index(raw, []byte("<head>")); i >= 0 {
		out = make([]byte, 0, len(raw)+len(inject))
		out = append(out, raw[:i+len("<head>")]...)
		out = append(out, inject...)
		out = append(out, raw[i+len("<head>"):]...)
	} else {
		out = append(inject, raw...)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}

// indexHTML returns the raw embedded index.html, cached after first read.
func (h *assetProxyHandler) indexHTML() ([]byte, error) {
	h.indexMu.Lock()
	defer h.indexMu.Unlock()
	if h.indexTL != nil {
		return h.indexTL, nil
	}
	raw, err := fs.ReadFile(h.subFS, "index.html")
	if err != nil {
		return nil, err
	}
	h.indexTL = raw
	return raw, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	httpx.EncodeJSON(w, v)
}

// jsonString returns a safely-quoted JSON string literal for embedding in an
// inline <script>. json.Marshal escapes </script> injection vectors.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
