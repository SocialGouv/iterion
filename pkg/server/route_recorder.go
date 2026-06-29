package server

import (
	"net/http"
	"sort"
	"strings"
	"sync"
)

// recordingMux is a thin wrapper over *http.ServeMux that captures every
// (method, pattern) registered through Handle/HandleFunc. It is the single
// source of truth behind GET /api/openapi.json: the route inventory is the
// live routing table, so the published spec can never drift from the code.
//
// It embeds *http.ServeMux so it stays a drop-in http.Handler and so the few
// external registrars that need the concrete *http.ServeMux (native tracker,
// dispatcher, board MCP) can be handed s.mux.ServeMux directly — their routes
// simply aren't recorded, which is intentional (they're specialized sub-trees,
// not part of the public REST surface).
type recordingMux struct {
	*http.ServeMux
	mu     sync.Mutex
	routes []RouteInfo
}

// RouteInfo is a single recorded route: an HTTP method (empty = any) and the
// path pattern as registered (Go 1.22 ServeMux syntax, e.g. "/api/orgs/{id}").
type RouteInfo struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
}

func newRecordingMux() *recordingMux {
	return &recordingMux{ServeMux: http.NewServeMux()}
}

func (m *recordingMux) Handle(pattern string, h http.Handler) {
	m.record(pattern)
	m.ServeMux.Handle(pattern, h)
}

func (m *recordingMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.record(pattern)
	m.ServeMux.HandleFunc(pattern, h)
}

// record splits a "METHOD /path" pattern (method optional) and stores it.
func (m *recordingMux) record(pattern string) {
	method, path := splitRoutePattern(pattern)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes = append(m.routes, RouteInfo{Method: method, Pattern: path})
}

// Routes returns a stable, sorted copy of the recorded routes.
func (m *recordingMux) Routes() []RouteInfo {
	m.mu.Lock()
	out := make([]RouteInfo, len(m.routes))
	copy(out, m.routes)
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// splitRoutePattern parses a Go 1.22 ServeMux pattern into (method, path).
// "GET /api/foo" → ("GET", "/api/foo"); "/api/foo" → ("", "/api/foo"). A
// host component (rare here) is left attached to the path verbatim.
func splitRoutePattern(pattern string) (method, path string) {
	p := strings.TrimSpace(pattern)
	if i := strings.IndexByte(p, ' '); i >= 0 {
		method = strings.ToUpper(strings.TrimSpace(p[:i]))
		path = strings.TrimSpace(p[i+1:])
		return method, path
	}
	return "", p
}
