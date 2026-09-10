package server

import (
	"net/http"
	"os"
)

// contentSecurityPolicy is the studio's CSP. It is enforceable as written
// because the SPA was already built to need nothing external:
//
//   - script-src 'self' — index.html carries no inline script, only
//     <script type="module" src>, and Monaco is bundled rather than pulled
//     from a CDN (see studio/src/lib/monaco.ts). This is the directive that
//     carries the weight: it is the boundary between an injected string and
//     executing code. Measured against the running SPA, it holds with zero
//     violations.
//   - style-src 'self' 'unsafe-inline' — measured, not conceded on principle.
//     The e2e run counts 234 `style-src-attr` and 2 `style-src-elem`
//     violations without it: the emotion-based CSS-in-JS under @lobehub/ui
//     injects <style> elements at runtime, and the component libraries set
//     style="" attributes. Removing it needs a nonce pipeline through those
//     libraries, not a policy edit. The exposure it leaves is CSS injection
//     (defacement, some data inference via selectors), categorically below
//     script execution, which stays refused.
//   - font-src 'self' — Geist ships as @fontsource, self-hosted on purpose.
//   - connect-src 'self' — apiBase() is a relative path and the WS URL is
//     built from location.host, so every call is same-origin.
//   - img-src and media-src add blob: — artifact and attachment previews are
//     fetched by the SPA and rendered from object URLs, images through <img>
//     and audio/video through <audio>/<video>. media-src does NOT inherit
//     img-src; omitting it silently sent media to default-src and blocked
//     every preview.
//   - worker-src adds blob: — Monaco's language services are emitted by the
//     bundler and started from our own origin, but a module-worker shim may
//     go through a blob URL.
//
// A handler that needs a different policy overrides it with its own Set:
// this middleware writes the header before the handler runs (the run-preview
// endpoint replaces it with a sandbox policy).
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; " +
	"media-src 'self' blob:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"worker-src 'self' blob:; " +
	// frame-src admits arbitrary http(s) because that is the Browser pane's
	// whole job: BrowserPane proxies through /api/runs/{id}/preview only for
	// `scope: internal`, and the DEFAULT scope is `external`, which iframes
	// the URL verbatim — one a workflow published or the operator typed. A
	// tighter value renders an empty frame with no in-product error, and
	// routing everything through the preview proxy is not the alternative it
	// looks like: that proxy fetches ONE document under a size cap, so a real
	// site loses every subresource.
	//
	// The give is small and bounded: frame-src governs what this page may
	// EMBED, a cross-origin frame cannot read our DOM, clickjacking is
	// frame-ancestors' business (still 'self'), and injecting the markup to
	// create a frame needs script execution, which script-src 'self' refuses.
	// On an https deployment `http:` is inert anyway — mixed content blocks
	// it — so it only serves a plaintext local studio.
	"frame-src 'self' blob: https: http:; " +
	"frame-ancestors 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'"

// BrowserGuard wraps a handler with the browser-facing protections the studio
// server applies: the CSRF origin gate, then the security headers.
//
// It is exported for the surfaces that build their OWN mux instead of going
// through Server.routes(). `iterion dispatch` serves the native board and the
// dispatcher REST API from a bare http.NewServeMux, so none of this reached
// them: a page the operator visited while the daemon ran could create a board
// card cross-origin, move it into the dispatcher-eligible state and force the
// poll — which runs a workflow, with tools, on the host. Loopback-bound is not
// a defence when the browser is on the host.
//
// port and publicURL feed the same allowlist Server uses, so a caller cannot
// drift from it: the predicate is Server.originGateAllows itself.
func BrowserGuard(port int, publicURL string, next http.Handler) http.Handler {
	s := &Server{cfg: Config{Port: port, PublicURL: publicURL}}
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.originGateAllows(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// securityHeaders sets the response headers the studio had been serving
// without. Measured on production before this change, the only one present
// was Strict-Transport-Security (added by the ingress): no CSP, no framing
// rule, no nosniff, no referrer policy — so the studio was clickjackable and
// nothing confined an XSS.
//
// HSTS is deliberately NOT set here. It is a transport promise that only the
// component terminating TLS can honestly make, the ingress already sends it,
// and emitting it from a plaintext local `iterion studio` would pin the
// operator's own loopback to HTTPS.
//
// ITERION_SECURITY_HEADERS=0 disables the whole set for a rollback without a
// redeploy.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("ITERION_SECURITY_HEADERS") == "0" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		// The policy ships on EVERY response, /api/ included. Skipping /api/
		// to keep a constant off the JSON hot path was a false economy: the
		// endpoint serving the least trustworthy markup in the product is
		// under /api/ — run artifacts, i.e. files an agent wrote — and it is
		// a document navigation, which the SPA's own policy does not govern.
		// A JSON response carrying an inert header costs less than reasoning,
		// per endpoint, about whether this one renders.
		//
		// A handler that needs a different policy still wins: this runs before
		// it, and its own Set replaces the value (the run preview swaps in a
		// sandbox policy).
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}
