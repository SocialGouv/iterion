package server

import (
	"net/http"
	"regexp"
	"strconv"
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
				// Query parameters are per-OPERATION: the DELETE of a bundle
				// file reads an if-match token the PUT takes in its body, so
				// they cannot ride the path-level list.
				if len(rs.query) > 0 {
					params := make([]any, 0, len(rs.query))
					for _, q := range rs.query {
						params = append(params, map[string]any{
							"name":        q.name,
							"in":          "query",
							"required":    false,
							"description": q.description,
							"schema":      map[string]any{"type": q.schemaType},
						})
					}
					op["parameters"] = params
				}
				if rs.request != nil {
					op["requestBody"] = map[string]any{
						"required": !rs.requestOptional,
						"content": map[string]any{
							"application/json": map[string]any{"schema": gen.schema(rs.request)},
						},
					}
				}
				if rs.description != "" {
					op["description"] = rs.description
				}
				if rs.response != nil {
					status := rs.responseStatus
					if status == 0 {
						status = http.StatusOK
					}
					op["responses"] = map[string]any{
						strconv.Itoa(status): map[string]any{
							"description": http.StatusText(status),
							"content": map[string]any{
								"application/json": map[string]any{"schema": gen.schema(rs.response)},
							},
						},
					}
				}
				if len(rs.errors) > 0 {
					responses, _ := op["responses"].(map[string]any)
					if responses == nil {
						responses = map[string]any{}
					}
					for code, description := range rs.errors {
						responses[strconv.Itoa(code)] = map[string]any{"description": description}
					}
					op["responses"] = responses
				}
			}
			// The credential-derivation chokepoint's refusal statuses: the
			// routes that mint a credential, grant or exercise platform
			// authority, or change or remove one answer the refusals before
			// their handler runs, and the spec carries them (the generator
			// has no other source for error responses).
			if responses := chokepointResponses(m, rt.Pattern); responses != nil {
				resp, _ := op["responses"].(map[string]any)
				if resp == nil {
					resp = map[string]any{}
				}
				for code, r := range responses {
					resp[code] = r
				}
				op["responses"] = resp
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
			"description": "Auto-generated route inventory for this iterion instance. Routes and methods are exact; request/response schemas are populated for the typed surface (see components) and enriched route-by-route. Note: the /api/v1/native, /api/v1/dispatcher and /api/v1/mcp/{board,runs} sub-trees are served but registered on a separate mux, so they are intentionally not catalogued here.",
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

// chokepointRefusalDescriptions are the shared refusal bodies: what the
// credential-derivation chokepoint (auth.Service.CheckDerivation, and the
// super-admin re-derivation at the identity) refuses, per status.
var chokepointRefusalDescriptions = map[int]string{
	http.StatusUnauthorized:       "The credential presented is refused: the access token's session is revoked (signed out everywhere, refresh reuse, a password change), the account is disabled or pending a password change, the token is expired, or a personal access token's row is revoked, expired or gone.",
	http.StatusForbidden:          "The credential presented may not do this: a personal access token asked for a credential of its own account (a switch, a token, an SSO link, a password) or for a team token that would outlive it (a webhook, share-link or invitation token), an access token naming no session asked for an SSO link, or the account is not a platform super-admin on a platform-admin route.",
	http.StatusConflict:           "The change conflicts with the account's state (see the error code in the body).",
	http.StatusTooManyRequests:    "The proof is throttled like the sign-in it stands for; Retry-After names the wait.",
	http.StatusServiceUnavailable: "The request's record landed but a later step failed; sign in again and repeat the request (every step is idempotent).",
}

// gatedRouteChokepointCodes lists the refusal statuses of every route the
// chokepoint gates, beyond the platform-admin family (every /api/admin/
// route and the non-GET plugin routes, which all answer 401 and 403). Keyed
// "METHOD pattern" as the mux registers them; keep in step with the
// gateDerivation / requireSuperAdmin registrations.
var gatedRouteChokepointCodes = map[string][]int{
	"POST /api/auth/me/team/{team_id}":                   {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/auth/me/org/{org_id}":                     {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/me/tokens":                                {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/ws/ticket":                                {http.StatusUnauthorized, http.StatusForbidden},
	"GET /api/me/sso/{provider}/link/start":              {http.StatusUnauthorized, http.StatusForbidden},
	"DELETE /api/me/sso/links/{provider}/{subject}":      {http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict},
	"DELETE /api/me/tokens/{token_id}":                   {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/me/password":                              {http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests},
	"POST /api/me/sessions/revoke-all":                   {http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable},
	"POST /api/admin/users/{id}/reset-password":          {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/orgs/{id}/sso/providers":                  {http.StatusUnauthorized, http.StatusForbidden},
	"PATCH /api/orgs/{id}/sso/providers/{provider_id}":   {http.StatusUnauthorized, http.StatusForbidden},
	"DELETE /api/orgs/{id}/sso/providers/{provider_id}":  {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/orgs/{id}/sso/domains":                    {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/orgs/{id}/sso/domains/{domain_id}/verify": {http.StatusUnauthorized, http.StatusForbidden},
	"DELETE /api/orgs/{id}/sso/domains/{domain_id}":      {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/teams/{id}/webhooks":                      {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/teams/{id}/webhooks/{webhook_id}/rotate":  {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/teams/{id}/config-shares":                 {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/teams/{id}/config-shares/{sid}/rotate":    {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/teams/{id}/invitations":                   {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/orgs/{id}/invitations":                    {http.StatusUnauthorized, http.StatusForbidden},
	// The repo-bot provisioning mints the repo's webhook token through the
	// orchestrator (forge.Orchestrator.Provision → webhooks.MintToken), and
	// the org approval replays that exact request.
	"POST /api/teams/{id}/forge/repo-bots":                          {http.StatusUnauthorized, http.StatusForbidden},
	"PATCH /api/teams/{id}/forge/repo-bots/{integration_id}":        {http.StatusUnauthorized, http.StatusForbidden},
	"POST /api/orgs/{id}/provision-approvals/{approval_id}/approve": {http.StatusUnauthorized, http.StatusForbidden},
}

// chokepointResponses answers the refusal response objects a route carries:
// the platform-admin family (every /api/admin/ route, the non-GET plugin
// routes) always, the explicit table's codes where the route is gated, nil
// for a route the chokepoint does not gate.
func chokepointResponses(method, pattern string) map[string]any {
	codes, ok := gatedRouteChokepointCodes[method+" "+pattern]
	if !ok {
		isPlatformAdmin := strings.HasPrefix(pattern, "/api/admin/") || pattern == "/api/admin"
		isPluginWrite := strings.HasPrefix(pattern, "/api/v1/plugins") && method != http.MethodGet
		if !isPlatformAdmin && !isPluginWrite {
			return nil
		}
		codes = []int{http.StatusUnauthorized, http.StatusForbidden}
	}
	out := make(map[string]any, len(codes))
	for _, code := range codes {
		out[strconv.Itoa(code)] = map[string]any{"description": chokepointRefusalDescriptions[code]}
	}
	return out
}
