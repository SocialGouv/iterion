package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// sessionCookieName is the HttpOnly cookie set on the bootstrap GET / when
// the request carries a matching `?t=<token>` query string. Subsequent
// requests must echo the cookie or be rejected with 401.
const sessionCookieName = "iterion_session"

// sessionTokenMiddleware wraps the inner handler and enforces a session
// token. Two authentication paths exist:
//
//  1. **Cookie path** (default for proxied / same-origin traffic): the
//     desktop AssetServer reverse-proxy attaches the iterion_session cookie
//     to every forwarded request, so the SPA loaded on the AssetServer
//     origin (wails:// or http://wails.localhost) authenticates without
//     ever needing to know the token. CLI users land at
//     http://127.0.0.1:<port>/?t=<token> once, get the cookie set, and the
//     same flow applies thereafter.
//  2. **Query-param path** (for cross-origin requests that cannot carry the
//     cookie — notably the editor's WebSocket connections, which must dial
//     the local server directly because Wails' AssetServer rejects WS
//     upgrades with 501): if a request carries `?t=<token>` matching
//     SessionToken, it is authorised regardless of cookie presence. On the
//     bootstrap GET / path we additionally set the cookie and 302 (legacy
//     CLI flow). On any other path we just authorise the single request and
//     do not set a cookie — keeping the token off-disk for non-bootstrap
//     callers.
//
// CLI mode (`iterion editor`) leaves SessionToken empty, so this middleware
// is never installed by Server.New and the behaviour is byte-identical to
// before.
func (s *Server) sessionTokenMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health endpoints must remain reachable without a session
		// cookie — kubelet probes do not carry one. The endpoints
		// reveal only build version + dependency status, no run data.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		// Token query path: takes precedence over the cookie. Constant-time
		// compare to avoid leaking the token via timing.
		if t := r.URL.Query().Get("t"); t != "" {
			if subtle.ConstantTimeCompare([]byte(t), []byte(s.cfg.SessionToken)) != 1 {
				http.Error(w, "invalid session token", http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
				// Bootstrap: set cookie + redirect so the token leaves the
				// URL bar / referer immediately. Subsequent GETs from the
				// SPA carry the cookie naturally.
				http.SetCookie(w, &http.Cookie{
					Name:     sessionCookieName,
					Value:    s.cfg.SessionToken,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
				q := r.URL.Query()
				q.Del("t")
				redirect := r.URL.Path
				if encoded := q.Encode(); encoded != "" {
					redirect += "?" + encoded
				}
				http.Redirect(w, r, redirect, http.StatusFound)
				return
			}
			// Non-bootstrap path with valid token: authorise this request
			// only, no cookie. Used by the desktop SPA's WebSocket dialer
			// which lives on a different origin and cannot share the
			// HttpOnly cookie.
			next.ServeHTTP(w, r)
			return
		}

		// Every other request must carry the cookie.
		c, err := r.Cookie(sessionCookieName)
		if err != nil || c == nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.cfg.SessionToken)) != 1 {
			// Friendly hint for the bootstrap GET / if the user landed
			// here without a token — return 401 so the desktop bootstrap
			// page can detect and re-issue the redirect.
			if r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
				http.Error(w, "session required", http.StatusUnauthorized)
				return
			}
			// Strip "Origin" check noise: we still want CORS preflights to
			// land cleanly, otherwise the browser logs an opaque error.
			if r.Method == http.MethodOptions && strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "session required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
