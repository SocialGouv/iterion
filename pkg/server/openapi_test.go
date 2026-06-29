package server

import (
	"net/http"
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
