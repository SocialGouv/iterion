package server

import (
	"strings"
	"testing"
)

// The enumeration the by-id epic (#1849) owes: every route served under an
// id wildcard, outside the families governed elsewhere, is either in the
// ADR-103 by-id class or listed here with the rule that governs it instead.
// A future id-addressed route reddens here until it takes the class (one
// resource per change) or states its reason — an id route that silently
// follows the active team is the exact thing the epic exists to retire.

// idRouteExemptions names the prefix families whose id-addressing is
// governed by something other than the active-team/by-id question, with
// the governing rule. First matching prefix wins.
var idRouteExemptions = []struct{ prefix, reason string }{
	{"/api/runs", "the by-id run class (ADR-103; witnessed by TestEveryRunWildcardRouteIsInTheClass)"},
	{"/api/ws/runs", "the by-id run class (ADR-103)"},
	{"/api/teams/", "path-team scoped: the {id} of the path IS the scope (canViewTeam/canManageTeam)"},
	{"/api/admin/", "super-admin surfaces: requireSuperAdmin over the platform sentinel tenant"},
	{"/api/auth/", "identity + OIDC: public or user-scoped, never tenant-addressed"},
	{"/api/me/", "user-scoped: handlers key on the user id, not a team"},
	{"/api/orgs/", "org-scoped by path"},
	{"/api/v1/marketplace/", "public marketplace family: anonymous reads, installs pack from repo coordinates"},
	{"/api/v1/mcp/", "per-run X-Iterion-Run token over single-tenant store surfaces (the C082 invariant)"},
	{"/api/webhooks/", "signature-authenticated deliveries; the retry route is super-admin over the delivery's own tenant"},
	{"/api/examples/", "static example programs from the deployment's ExamplesDir or the embedded recipes: the {name} is a sanitized file path in a deployment-shared tree, not a tenant-addressed resource"},
	{"/api/local/", "local-mode skill library over the server's own StoreDir (global|project scope): no tenant dimension — the {name} addresses the local library, not a tenant resource"},
	{"/api/projects/", "local-mode projects registry (refused outright in cloud mode): the {id} addresses an entry of the operator's local registry, not a tenant resource"},
	{"/api/v1/pipeline-board/workspace-images/", "workspace-file serving from the server's own workdir (safePath containment + image extension allowlist): the {path} is a file path in the deployment's workdir tree, not a tenant-addressed resource"},
	{"/api/v1/plugins/", "the deployment's plugin registry (builtins + the local plugin tree), mutations super-admin-gated: the {name} addresses a plugin of the shared tree, not a tenant resource"},
	{"/brand/", "public static brand assets (stable URLs, ETag): the {file} is an asset name, not a tenant-addressed resource"},
}

// idRouteReasons names the id-wildcard families that today resolve through
// the active team or a tiered design, WITH the ticket or decision that owns
// that state. The pending #1849 issues slice (L) turns the board entries
// into by-id class members.
var idRouteReasons = []struct{ prefix, reason string }{
	{"/api/v1/native/issues/", "board issue by id: the store resolves per ACTIVE team (cloud) or the one local store; a cross-team id answers 404 emergently — the ADR-103 by-id rule for issues is the pending #1849 slice"},
	{"/api/v1/pipeline-board/tasks/", "pipeline board cards over the SAME native store: same active-team resolution, same pending #1849 slice"},
	{"/api/v1/native/labels/", "board label vocabulary of the active team's store"},
	{"/api/v1/native/board/", "board config (states/fields/views) of the active team's store"},
	{"/api/v1/bots/", "the dispatch catalog: a tiered namespace by design (team > platform > baked over the shared FS catalog), not an authored-resource id"},
	{"/api/assistant-watches/", "fixed compatibility sibling of the run-addressed DELETE /api/runs/{id}/assistant-watches/{watchID} (ADR-103 decision 4): the stop resolves the watch's TARGET RUN under the caller's active team, so it answers 404 across teams like the board class; the run-addressed spelling is the governed one"},
}

// hasWildcardSegment reports whether the pattern carries any {id} segment —
// derived from the pattern, never from a production table (a witness that
// reads the table it witnesses is mute on a table change).
func hasWildcardSegment(pattern string) bool {
	if i := strings.Index(pattern, " "); i >= 0 && !strings.HasPrefix(pattern, "/") {
		pattern = pattern[i+1:]
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			return true
		}
	}
	return false
}

// idRouteClassification returns the rule governing an id-wildcard pattern,
// or "" when the pattern is UNCLASSIFIED — the state this witness refuses.
func idRouteClassification(pattern string) string {
	for _, ex := range idRouteExemptions {
		if strings.HasPrefix(pattern, ex.prefix) {
			return ex.reason
		}
	}
	for _, rr := range idRouteReasons {
		if strings.HasPrefix(pattern, rr.prefix) {
			return rr.reason
		}
	}
	return ""
}

func TestEveryIDWildcardRouteIsClassifiedOrReasoned(t *testing.T) {
	srv, _ := newTestServer(t)
	// The board registrars mount on the raw *http.ServeMux — the recording
	// mux never sees them — so their table is derived from the registrar
	// source, exactly as the write-rung witness derives it.
	table := srv.mux.Routes()
	table = append(table, nativeBoardAPIRoutes(t)...)
	table = append(table, boardRegistrarRoutes(t)...)
	seen := 0
	for _, route := range table {
		if !hasWildcardSegment(route.Pattern) {
			continue
		}
		if reason := idRouteClassification(route.Pattern); reason == "" {
			t.Fatalf("%s %s: an id-addressed route is neither in the by-id class nor reasoned — classify it (ADR-103, one resource per change) or extend this enumeration with the rule that governs it", route.Method, route.Pattern)
		}
		seen++
	}
	if seen < 10 {
		t.Fatalf("only %d id-wildcard routes enumerated — the union table collapsed and this witness covers almost nothing", seen)
	}
}
