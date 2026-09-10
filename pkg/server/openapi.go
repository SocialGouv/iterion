package server

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
)

// registerOpenAPIRoutes serves the live route inventory as an OpenAPI 3 document
// (GET /api/openapi.json) and as a flat list (GET /api/routes). Both are
// generated from the recordingMux's captured routes, so they are zero-drift by
// construction. The CLI consumes them (`iterion remote openapi` / `routes`) to
// drive the instance without a hand-maintained client.
//
// Auth: both require a session (requireAuth) — the inventory reveals the API
// shape, not data, but there's no reason to expose it anonymously.
func (s *Server) registerOpenAPIRoutes() {
	s.mux.Handle("GET /api/openapi.json", s.requireAuth(http.HandlerFunc(s.handleOpenAPI)))
	s.mux.Handle("GET /api/routes", s.requireAuth(http.HandlerFunc(s.handleRoutes)))
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	s.writeJSONFor(w, r, map[string]any{"routes": s.mux.Routes()})
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	s.writeJSONFor(w, r, s.buildOpenAPI())
}

var routePathParamRe = regexp.MustCompile(`\{([^}]+)\}`)

// buildOpenAPI assembles a minimal-but-valid OpenAPI 3.1 document from the
// recorded routes. Paths and methods are exact; path params are derived from
// the `{name}` segments; tags group by the first meaningful path element.
// Request/response schemas are intentionally left open ({}) — this v1 is a
// faithful route inventory; richer schemas are an incremental follow-on.
func (s *Server) buildOpenAPI() map[string]any {
	paths := map[string]any{}
	gen := newSchemaGen()
	schemas := routeSchemas()
	for _, rt := range s.mux.Routes() {
		if !strings.HasPrefix(rt.Pattern, "/api/") {
			continue
		}
		oasPath := toOpenAPIPath(rt.Pattern)
		item, _ := paths[oasPath].(map[string]any)
		if item == nil {
			item = map[string]any{}
			if params := pathParams(rt.Pattern); len(params) > 0 {
				item["parameters"] = params
			}
			paths[oasPath] = item
		}
		methods := methodsFor(rt.Method)
		for _, m := range methods {
			op := map[string]any{
				"operationId": operationID(m, rt.Pattern),
				"tags":        []string{tagFor(rt.Pattern)},
				"summary":     m + " " + rt.Pattern,
			}
			// Enrich from the route→types registry when present; otherwise
			// fall back to an open "default" response.
			if rs, ok := schemas[m+" "+rt.Pattern]; ok {
				if rs.request != nil {
					op["requestBody"] = map[string]any{
						"required": !rs.requestOptional,
						"content": map[string]any{
							"application/json": map[string]any{"schema": gen.schema(rs.request)},
						},
					}
				}
				if rs.response != nil {
					op["responses"] = map[string]any{
						"200": map[string]any{
							"description": "OK",
							"content": map[string]any{
								"application/json": map[string]any{"schema": gen.schema(rs.response)},
							},
						},
					}
				}
			}
			if _, ok := op["responses"]; !ok {
				op["responses"] = map[string]any{"default": map[string]any{"description": "Response"}}
			}
			item[strings.ToLower(m)] = op
		}
	}

	version := appinfo.Version
	if appinfo.Commit != "" {
		version += " (" + appinfo.Commit + ")"
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "iterion API",
			"version":     version,
			"description": "Auto-generated route inventory for this iterion instance. Routes and methods are exact; request/response schemas are populated for the typed surface (see components) and enriched route-by-route. Note: the /api/v1/native, /api/v1/dispatcher and /api/v1/mcp/board sub-trees are served but registered on a separate mux, so they are intentionally not catalogued here.",
		},
		"servers": []any{map[string]any{"url": "/"}},
		"paths":   paths,
	}
	if len(gen.components) > 0 {
		doc["components"] = map[string]any{"schemas": gen.components}
	}
	return doc
}

// toOpenAPIPath maps Go ServeMux wildcards to OpenAPI templating. Go's
// `{name}` and OpenAPI's `{name}` already match; trailing `{name...}` becomes
// `{name}` (OpenAPI has no catch-all syntax). Trailing slashes are kept.
func toOpenAPIPath(pattern string) string {
	return strings.ReplaceAll(pattern, "...}", "}")
}

func pathParams(pattern string) []any {
	var out []any
	seen := map[string]bool{}
	for _, m := range routePathParamRe.FindAllStringSubmatch(pattern, -1) {
		name := strings.TrimSuffix(m[1], "...")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, map[string]any{
			"name":     name,
			"in":       "path",
			"required": true,
			"schema":   map[string]any{"type": "string"},
		})
	}
	return out
}

// methodsFor returns the concrete HTTP methods to emit for a recorded route.
// A route registered with no method (Go matches any) is documented as GET+POST
// — the two verbs the SPA actually uses against method-less patterns.
func methodsFor(method string) []string {
	if method == "" {
		return []string{"GET", "POST"}
	}
	return []string{method}
}

// tagFor groups operations under a human bucket: the first path element after
// /api (and after a /v1 version prefix), e.g. /api/orgs/{id} → "orgs",
// /api/v1/triggers → "triggers".
func tagFor(pattern string) string {
	parts := strings.Split(strings.Trim(pattern, "/"), "/")
	// parts[0] == "api"
	i := 1
	if i < len(parts) && (parts[i] == "v1" || parts[i] == "v2") {
		i++
	}
	if i < len(parts) && parts[i] != "" {
		return parts[i]
	}
	return "api"
}

// operationID builds a stable, unique-ish id: method + path elements, with
// `{p}` params rendered as `By<Param>`. e.g. GET /api/orgs/{id} → getOrgsById.
func operationID(method, pattern string) string {
	parts := strings.Split(strings.Trim(pattern, "/"), "/")
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, p := range parts {
		if p == "" || p == "api" {
			continue
		}
		if strings.HasPrefix(p, "{") {
			name := strings.Trim(p, "{}")
			name = strings.TrimSuffix(name, "...")
			b.WriteString("By")
			b.WriteString(title(name))
			continue
		}
		b.WriteString(title(p))
	}
	return b.String()
}

func title(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "-", " ")
	var b strings.Builder
	for _, w := range strings.Fields(s) {
		b.WriteString(strings.ToUpper(w[:1]))
		b.WriteString(w[1:])
	}
	return b.String()
}
