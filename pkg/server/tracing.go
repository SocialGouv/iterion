package server

import "net/http"

// routePattern resolves a request to the route pattern the mux would
// match ("GET /api/runs/{id}"), which is the name a request-scoped
// Sentry transaction should carry: one per ROUTE, not one per URL.
//
// The lookup is needed because the auth layer forwards a
// r.WithContext() copy to the mux, so the pattern net/http stamps
// during routing never reaches the outermost middleware.
//
// Returns "" when nothing matches (the tracer then names the
// transaction itself) — and is only ever called when tracing is on.
func (s *Server) routePattern(r *http.Request) string {
	if s == nil || s.mux == nil {
		return ""
	}
	_, pattern := s.mux.Handler(r)
	return pattern
}
