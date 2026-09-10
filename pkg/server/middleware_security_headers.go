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
	"frame-src 'self' blob:; " +
	"frame-ancestors 'self'; " +
	"base-uri 'self'; " +
	"form-action 'self'; " +
	"object-src 'none'"

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
