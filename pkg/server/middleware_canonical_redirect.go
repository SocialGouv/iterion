package server

import (
	"net/http"
	"net/url"
	"strings"
)

// canonicalRedirect sends browser DOCUMENT navigations that arrive on a
// non-canonical host to publicURL, so a deployment published under several
// names still has ONE browser origin. Cookies, sessionStorage, the CSP and the
// WebSocket origin gate are all keyed on the origin, so a second name means a
// second, silently divergent copy of all four.
//
// Three things it deliberately does not touch:
//
//   - Anything under /api/. Inbound webhooks are configured with whichever host
//     the forge was given, the CLI and SDK with whichever host the operator
//     typed, and a redirect would arrive at the forge as a non-2xx delivery.
//     The API answers on every host it is reachable at, by design.
//   - Anything that is not a document navigation. A health probe, a fetch for
//     JSON, a preloaded module: redirecting those buys nothing and a probe
//     redirected off its own pod fails the pod.
//   - The scheme. http→https is the terminating proxy's promise (it also sends
//     HSTS); this middleware only ever changes the host.
//
// The canonical origin is resolved ONCE here, not per request: the value that
// decides where a user is sent must be the one validated at startup, not a
// string re-parsed under whatever the config map holds by then.
func canonicalRedirect(enabled bool, publicURL string, next http.Handler) http.Handler {
	if !enabled || publicURL == "" {
		return next
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Unreachable through New: Config.Validate refuses this pairing at
		// startup. Passing through rather than redirecting to a broken origin
		// keeps a misconfiguration from taking the surface down.
		return next
	}
	// normalizeOrigin is the rule the CSRF allowlist already applies to this
	// same value, and re-deriving it by hand is exactly what its doc comment
	// warns about: a browser serialises `https://host:443` as `host`, so
	// comparing the raw configured host sends every navigation to a target
	// whose Host header can never match it — an infinite redirect loop, with
	// the whole browser surface down. It also drops the path, query, fragment
	// and userinfo that would otherwise be welded in front of the request path.
	origin := normalizeOrigin(u)
	scheme := strings.ToLower(u.Scheme)

	onCanonical := func(host string) bool {
		return host != "" && normalizeOrigin(&url.URL{Scheme: scheme, Host: host}) == origin
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vary names every header the decision reads, unconditionally: the
		// decision is taken before we know which branch we are on, and a
		// shared cache that keyed only on the URL would hand a cached 302 to a
		// fetch, or a cached 200 to a navigation on a secondary host.
		w.Header().Add("Vary", "Sec-Fetch-Dest, Accept, X-Forwarded-Host")

		// EITHER host matching is enough to pass through. Taking the forwarded
		// host alone would let a request that already arrived on the canonical
		// origin — carrying a spoofed or simply unexpected X-Forwarded-Host —
		// be redirected to the identical URL, which is an unbounded loop
		// whenever the chain is consistently misconfigured. With both consulted
		// a header disagreement can only SKIP a redirect, never create one,
		// which is the asymmetry that made trusting the header safe in the
		// first place.
		if onCanonical(r.Host) || onCanonical(requestHost(r)) {
			next.ServeHTTP(w, r)
			return
		}

		// RequestURI() is not guaranteed to start with "/": an opaque request
		// target (`GET foo:.evil.example/ HTTP/1.1`) comes back verbatim, and
		// concatenating it would weld an attacker-chosen suffix onto the
		// hostname in Location. No browser can send that shape, but the
		// invariant this middleware states — it only ever changes the host,
		// to the canonical one — has to be true for every client.
		target := r.URL.RequestURI()
		if !isCanonicalRedirectable(r) || !strings.HasPrefix(target, "/") {
			next.ServeHTTP(w, r)
			return
		}
		// 302, not 301: a permanent redirect is cached by the browser past any
		// server-side change, so an operator who later wants both hosts serving
		// documents cannot take it back from the users who already saw it.
		http.Redirect(w, r, origin+target, http.StatusFound)
	})
}

// requestHost is the host the CLIENT addressed, which is not always r.Host: a
// proxy that rewrites Host to an internal Service name leaves the original in
// X-Forwarded-Host. Comparing the rewritten value would mean the canonical host
// never matches, so every navigation is redirected to a target whose Host can
// never match either — an infinite loop taking the whole browser surface down.
//
// Trusting the header is the safe direction here, and deliberately so: the
// redirect TARGET is always the configured origin and never anything from the
// request, so the only thing a spoofed value buys is NOT being redirected. The
// asymmetry runs entirely one way — a loop is catastrophic, a skipped redirect
// is nothing.
func requestHost(r *http.Request) string {
	fwd := r.Header.Get("X-Forwarded-Host")
	if fwd == "" {
		return r.Host
	}
	// A proxy chain appends; the first entry is the one the client addressed.
	if i := strings.IndexByte(fwd, ','); i >= 0 {
		fwd = fwd[:i]
	}
	if fwd = strings.TrimSpace(fwd); fwd != "" {
		return fwd
	}
	return r.Host
}

// isCanonicalRedirectable reports whether r is the kind of request a canonical
// redirect may move: a browser navigating to a page.
//
// Sec-Fetch-Dest is the authoritative signal and every current browser sends it
// on a navigation. Accept is the fallback for clients that do not, and it is
// checked for text/html specifically — a probe or an SDK call sends */*, which
// must not match, or a liveness probe would be redirected off its own pod.
func isCanonicalRedirectable(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return strings.EqualFold(dest, "document")
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}
