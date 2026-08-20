package errtrack

import (
	"net/http"

	sentryhttp "github.com/getsentry/sentry-go/http"
)

// HTTPOptions parameterises HTTPMiddleware.
type HTTPOptions struct {
	// RouteName maps a request to the LOW-CARDINALITY name its
	// transaction should carry — the route pattern
	// ("GET /api/runs/{id}"), not the URL ("GET /api/runs/019f83…").
	//
	// It exists because the SDK reads http.Request.Pattern, which
	// net/http's mux stamps on the request only once routing has
	// happened — and any middleware in between that forwards a
	// r.WithContext() copy (an auth layer injecting an identity, say)
	// leaves the outer request's Pattern empty, so every distinct URL
	// would become its own transaction.
	//
	// Return "" to fall back to the SDK's own naming. nil is fine: the
	// middleware then never looks a route up.
	RouteName func(*http.Request) string
}

// HTTPMiddleware returns the middleware that turns each HTTP request
// into a Sentry transaction.
//
// **When tracing is off it returns the identity function**, so a
// deployment without SENTRY_TRACES_SAMPLE_RATE keeps byte-for-byte the
// handler chain it had — no wrapper, no per-request allocation, no
// route lookup. Call it after Init, which is what settles that.
//
// The wrapped chain keeps the caller's panic semantics: the SDK
// captures a panic escaping a handler and RE-PANICS, so net/http's own
// per-connection recovery still ends the request exactly as before.
func HTTPMiddleware(opts HTTPOptions) func(http.Handler) http.Handler {
	if !tracing.Load() {
		return func(next http.Handler) http.Handler { return next }
	}
	handler := sentryhttp.New(sentryhttp.Options{Repanic: true})
	return func(next http.Handler) http.Handler {
		traced := handler.Handle(next)
		if opts.RouteName == nil {
			return traced
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if name := opts.RouteName(r); name != "" {
				// A copy: the request belongs to the server, and the
				// mux downstream stamps the real pattern on its own
				// copy anyway. Pattern is informational — PathValue
				// reads the mux's match, not this field.
				clone := r.Clone(r.Context())
				clone.Pattern = name
				r = clone
			}
			traced.ServeHTTP(w, r)
		})
	}
}
