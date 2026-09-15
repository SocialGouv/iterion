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

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if normalizeOrigin(&url.URL{Scheme: scheme, Host: r.Host}) == origin {
			next.ServeHTTP(w, r)
			return
		}
		// Off the canonical host the answer depends on the navigation signal,
		// so a shared cache must key on it — otherwise it hands a cached 302
		// to a fetch, or a cached 200 to a navigation.
		w.Header().Add("Vary", "Sec-Fetch-Dest, Accept")

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
