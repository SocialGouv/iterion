package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestRecordingMuxRecordsRoutes(t *testing.T) {
	m := newRecordingMux()
	m.Handle("GET /api/orgs/{id}", http.NotFoundHandler())
	m.HandleFunc("POST /api/orgs", func(http.ResponseWriter, *http.Request) {})
	m.Handle("/api/health", http.NotFoundHandler()) // no method

	routes := m.Routes()
	if len(routes) != 3 {
		t.Fatalf("want 3 routes, got %d: %+v", len(routes), routes)
	}
	// Sorted by pattern then method.
	want := []RouteInfo{
		{Method: "", Pattern: "/api/health"},
		{Method: "POST", Pattern: "/api/orgs"},
		{Method: "GET", Pattern: "/api/orgs/{id}"},
	}
	for i, w := range want {
		if routes[i] != w {
			t.Errorf("route[%d] = %+v, want %+v", i, routes[i], w)
		}
	}
}

func TestBuildOpenAPIShape(t *testing.T) {
	s := &Server{mux: newRecordingMux()}
	s.mux.Handle("GET /api/orgs/{id}", http.NotFoundHandler())
	s.mux.Handle("POST /api/orgs", http.NotFoundHandler())
	s.mux.Handle("GET /api/v1/triggers/{id}", http.NotFoundHandler())
	s.mux.Handle("GET /healthz", http.NotFoundHandler()) // non-/api → excluded

	doc := s.buildOpenAPI()
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi version = %v", doc["openapi"])
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths not a map: %T", doc["paths"])
	}
	if _, ok := paths["/healthz"]; ok {
		t.Errorf("non-/api route leaked into spec")
	}

	// /api/orgs/{id} → has a get op, an id path param, tag "orgs".
	item, ok := paths["/api/orgs/{id}"].(map[string]any)
	if !ok {
		t.Fatalf("missing /api/orgs/{id}: %+v", paths)
	}
	params, ok := item["parameters"].([]any)
	if !ok || len(params) != 1 {
		t.Fatalf("want 1 path param, got %+v", item["parameters"])
	}
	p0 := params[0].(map[string]any)
	if p0["name"] != "id" || p0["in"] != "path" {
		t.Errorf("bad path param: %+v", p0)
	}
	get, ok := item["get"].(map[string]any)
	if !ok {
		t.Fatalf("missing get op on /api/orgs/{id}")
	}
	if get["operationId"] != "getOrgsById" {
		t.Errorf("operationId = %v, want getOrgsById", get["operationId"])
	}
	if tags, _ := get["tags"].([]string); len(tags) != 1 || tags[0] != "orgs" {
		t.Errorf("tags = %v, want [orgs]", get["tags"])
	}

	// /api/v1/triggers/{id} → tag strips the v1 prefix.
	tItem := paths["/api/v1/triggers/{id}"].(map[string]any)
	tGet := tItem["get"].(map[string]any)
	if tags, _ := tGet["tags"].([]string); len(tags) != 1 || tags[0] != "triggers" {
		t.Errorf("v1 tag = %v, want [triggers]", tGet["tags"])
	}
}

func TestOpenAPITypedSchemas(t *testing.T) {
	s := &Server{mux: newRecordingMux()}
	// Routes whose types are registered in routeSchemas().
	s.mux.Handle("POST /api/auth/login", http.NotFoundHandler())
	s.mux.Handle("POST /api/admin/orgs", http.NotFoundHandler())

	doc := s.buildOpenAPI()
	paths := doc["paths"].(map[string]any)

	// login POST → requestBody schema referencing loginReq fields.
	login := paths["/api/auth/login"].(map[string]any)["post"].(map[string]any)
	rb, ok := login["requestBody"].(map[string]any)
	if !ok {
		t.Fatalf("login has no requestBody: %+v", login)
	}
	schema := rb["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if ref, _ := schema["$ref"].(string); ref != "#/components/schemas/loginReq" {
		t.Errorf("login requestBody $ref = %q, want loginReq", ref)
	}
	// 200 response schema present (AuthSessionResponse).
	resp200, ok := login["responses"].(map[string]any)["200"].(map[string]any)
	if !ok {
		t.Fatalf("login has no 200 response: %+v", login["responses"])
	}
	if resp200["content"] == nil {
		t.Errorf("login 200 has no content schema")
	}

	// components/schemas was populated and includes the referenced types.
	comps, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("no components emitted")
	}
	schemas := comps["schemas"].(map[string]any)
	for _, want := range []string{"loginReq", "AuthSessionResponse", "createOrgReq", "orgView"} {
		if _, ok := schemas[want]; !ok {
			t.Errorf("components.schemas missing %q", want)
		}
	}
	// loginReq must require email + password (no omitempty).
	lr := schemas["loginReq"].(map[string]any)
	req, _ := lr["required"].([]string)
	if !containsAll(req, "email", "password") {
		t.Errorf("loginReq required = %v, want email+password", req)
	}
}

func containsAll(have []string, want ...string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestRouteSchemasKeysAreRegistered closes the one silent-drift hole the CI
// generate-and-diff guard doesn't cover: a routeSchemas() key that no longer
// matches a real route (e.g. after a rename) would quietly stop enriching the
// spec with zero signal. Assert every key resolves to a recorded route+method.
func TestRouteSchemasKeysAreRegistered(t *testing.T) {
	doc, err := BuildOpenAPISpec()
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	for key := range routeSchemas() {
		method, pat, ok := strings.Cut(key, " ")
		if !ok {
			t.Errorf("routeSchemas key %q is not \"METHOD /pattern\"", key)
			continue
		}
		item, ok := paths[toOpenAPIPath(pat)].(map[string]any)
		if !ok {
			t.Errorf("routeSchemas key %q: path %q is not a registered route (renamed/removed?)", key, pat)
			continue
		}
		if _, ok := item[strings.ToLower(method)]; !ok {
			t.Errorf("routeSchemas key %q: method %s not registered on path %q", key, method, pat)
		}
	}
}

func TestMethodlessRouteGetsGetAndPost(t *testing.T) {
	s := &Server{mux: newRecordingMux()}
	s.mux.Handle("/api/memory", http.NotFoundHandler())
	paths := s.buildOpenAPI()["paths"].(map[string]any)
	item := paths["/api/memory"].(map[string]any)
	if _, ok := item["get"]; !ok {
		t.Error("method-less route missing get")
	}
	if _, ok := item["post"]; !ok {
		t.Error("method-less route missing post")
	}
}

// TestTheSpecCarriesTheChokepointRefusals: the routes the credential
// derivation chokepoint gates carry their refusal statuses in the spec —
// 401 for a revoked session's access token, 403 for a credential that may
// not do the route's act, and the route-specific conflicts — and a route
// the chokepoint does not gate carries none of them.
func TestTheSpecCarriesTheChokepointRefusals(t *testing.T) {
	s := &Server{mux: newRecordingMux()}
	s.mux.Handle("POST /api/me/password", http.NotFoundHandler())
	s.mux.Handle("POST /api/me/sessions/revoke-all", http.NotFoundHandler())
	s.mux.Handle("DELETE /api/me/sso/links/{provider}/{subject}", http.NotFoundHandler())
	s.mux.Handle("GET /api/admin/users", http.NotFoundHandler())
	s.mux.Handle("POST /api/v1/plugins/thing", http.NotFoundHandler())
	s.mux.Handle("GET /api/me/sso/links", http.NotFoundHandler()) // not gated

	doc := s.buildOpenAPI()
	paths := doc["paths"].(map[string]any)
	codesOf := func(path, method string) map[string]any {
		t.Helper()
		item, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("%s is not in the spec", path)
		}
		op, ok := item[method].(map[string]any)
		if !ok {
			t.Fatalf("%s %s is not in the spec", method, path)
		}
		resp, _ := op["responses"].(map[string]any)
		return resp
	}
	for _, p := range []struct{ path, method string }{
		{"/api/me/password", "post"},
		{"/api/me/sessions/revoke-all", "post"},
		{"/api/me/sso/links/{provider}/{subject}", "delete"},
		{"/api/admin/users", "get"},
		{"/api/v1/plugins/thing", "post"},
	} {
		if _, ok := codesOf(p.path, p.method)["401"]; !ok {
			t.Fatalf("%s %s carries no 401 refusal", p.method, p.path)
		}
	}
	if _, ok := codesOf("/api/me/sso/links/{provider}/{subject}", "delete")["409"]; !ok {
		t.Fatal("the unlink carries no 409 (last_sign_in_method)")
	}
	if _, ok := codesOf("/api/me/password", "post")["429"]; !ok {
		t.Fatal("the password change carries no 429 (the proof's bucket)")
	}
	if _, ok := codesOf("/api/me/sessions/revoke-all", "post")["503"]; !ok {
		t.Fatal("sign out everywhere carries no 503 (cleanup incomplete)")
	}
	if resp := codesOf("/api/me/sso/links", "get"); len(resp) > 1 {
		t.Fatalf("a route the chokepoint does not gate gained refusal codes: %v", resp)
	}
}

// TestTheSpecTypesTheSSOLinkList: GET /api/me/sso/links answers a typed
// response — the SSO link rows with session_linked, the marker that names
// which identity "sign out everywhere" keeps — not the untyped default no
// client could read the field from.
func TestTheSpecTypesTheSSOLinkList(t *testing.T) {
	s := &Server{mux: newRecordingMux()}
	s.mux.Handle("GET /api/me/sso/links", http.NotFoundHandler())
	doc := s.buildOpenAPI()
	paths := doc["paths"].(map[string]any)
	get := paths["/api/me/sso/links"].(map[string]any)["get"].(map[string]any)
	resp200, ok := get["responses"].(map[string]any)["200"].(map[string]any)
	if !ok {
		t.Fatalf("the link list carries no 200 response: %+v", get["responses"])
	}
	schema := resp200["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	links, ok := props["links"].(map[string]any)
	if !ok {
		t.Fatalf("the response schema names no links array: %+v", schema)
	}
	items, _ := links["items"].(map[string]any)
	if ref, _ := items["$ref"].(string); ref != "#/components/schemas/ssoLinkView" {
		t.Fatalf("the link rows $ref = %q, want ssoLinkView", ref)
	}
	comps := doc["components"].(map[string]any)["schemas"].(map[string]any)
	view := comps["ssoLinkView"].(map[string]any)
	vprops, _ := view["properties"].(map[string]any)
	sl, ok := vprops["session_linked"].(map[string]any)
	if !ok || sl["type"] != "boolean" {
		t.Fatalf("ssoLinkView.session_linked = %+v, want a boolean property", vprops["session_linked"])
	}
}
