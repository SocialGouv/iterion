package operatormcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newRemoteStub starts a stub instance and points the env-only remote
// config at it (HOME is isolated so a real ~/.iterion/cli-auth.json is
// never read).
func newRemoteStub(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	isolateHome(t)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("ITERION_REMOTE_URL", srv.URL)
	t.Setenv("ITERION_REMOTE_TOKEN", "iap_test")
	return srv
}

func TestRemoteNotLoggedIn(t *testing.T) {
	isolateHome(t)
	s := newTestServer(t)
	text, isErr := call(t, s, "remote_status", `{}`)
	if !isErr {
		t.Fatalf("remote_status without credentials should error: %s", text)
	}
	if !strings.Contains(text, "iterion remote login") {
		t.Fatalf("error should tell the operator how to log in: %s", text)
	}
}

func TestRemoteStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer iap_test" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"user":{"email":"jo@example.org"},"active_org_id":"org1","active_team_id":"team1"}`))
	})
	newRemoteStub(t, mux)

	s := newTestServer(t)
	text, isErr := call(t, s, "remote_status", `{}`)
	if isErr {
		t.Fatalf("remote_status errored: %s", text)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if got["email"] != "jo@example.org" || got["active_team_id"] != "team1" {
		t.Fatalf("unexpected status: %v", got)
	}
}

func TestRemoteRunsListBuildsQuery(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"runs":[]}`))
	})
	newRemoteStub(t, mux)

	s := newTestServer(t)
	text, isErr := call(t, s, "remote_runs_list", `{"status":"running","limit":5}`)
	if isErr {
		t.Fatalf("remote_runs_list errored: %s", text)
	}
	if !strings.Contains(gotQuery, "status=running") || !strings.Contains(gotQuery, "limit=5") {
		t.Fatalf("query not forwarded: %q", gotQuery)
	}
}

func TestRemoteRunsResumeBody(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/runs/r1/resume", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"status":"resuming"}`))
	})
	newRemoteStub(t, mux)

	s := newTestServer(t)
	text, isErr := call(t, s, "remote_runs_resume", `{"run_id":"r1","answers":{"q":"yes"},"force":true}`)
	if isErr {
		t.Fatalf("resume errored: %s", text)
	}
	if gotBody["force"] != true {
		t.Fatalf("force not forwarded: %v", gotBody)
	}
	answers, _ := gotBody["answers"].(map[string]any)
	if answers["q"] != "yes" {
		t.Fatalf("answers not forwarded: %v", gotBody)
	}
}

func TestRemoteIssueUpdateSendsOnlyProvidedFields(t *testing.T) {
	var gotBody map[string]json.RawMessage
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/v1/native/issues/i1", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	newRemoteStub(t, mux)

	s := newTestServer(t)
	text, isErr := call(t, s, "remote_issue_update", `{"id":"i1","labels":["a","b"]}`)
	if isErr {
		t.Fatalf("update errored: %s", text)
	}
	if len(gotBody) != 1 {
		t.Fatalf("PATCH must carry only the provided fields, got %v", gotBody)
	}
	if _, ok := gotBody["labels"]; !ok {
		t.Fatalf("labels missing from PATCH body: %v", gotBody)
	}

	// No updatable field at all → explicit error, no request.
	text, isErr = call(t, s, "remote_issue_update", `{"id":"i1"}`)
	if !isErr || !strings.Contains(text, "no fields to update") {
		t.Fatalf("want explicit no-fields error, got: %s", text)
	}
}

func TestRemoteAPIEscapeHatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(b)
	})
	mux.HandleFunc("GET /api/broken", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	})
	newRemoteStub(t, mux)

	s := newTestServer(t)
	text, isErr := call(t, s, "remote_api", `{"method":"post","path":"/api/echo","body":{"x":1}}`)
	if isErr {
		t.Fatalf("echo errored: %s", text)
	}
	if !strings.HasPrefix(text, "HTTP 201") || !strings.Contains(text, `"x":1`) {
		t.Fatalf("unexpected escape-hatch result: %s", text)
	}

	// HTTP >= 400 routes as a tool error with the body visible.
	text, isErr = call(t, s, "remote_api", `{"method":"GET","path":"/api/broken"}`)
	if !isErr || !strings.Contains(text, "HTTP 502") || !strings.Contains(text, "boom") {
		t.Fatalf("error response should surface status + body: isErr=%v %s", isErr, text)
	}

	// Bad method and relative path are refused before any request.
	if text, isErr := call(t, s, "remote_api", `{"method":"BREW","path":"/api/echo"}`); !isErr || !strings.Contains(text, "invalid method") {
		t.Fatalf("invalid method should be refused: %s", text)
	}
	if text, isErr := call(t, s, "remote_api", `{"method":"GET","path":"api/echo"}`); !isErr || !strings.Contains(text, "absolute") {
		t.Fatalf("relative path should be refused: %s", text)
	}
}

func TestRemoteAPIReadOnlyAllowsOnlyGET(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/thing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	newRemoteStub(t, mux)

	s := &Server{StoreDir: t.TempDir(), WorkDir: t.TempDir(), ReadOnly: true}
	text, isErr := call(t, s, "remote_api", `{"method":"GET","path":"/api/thing"}`)
	if isErr {
		t.Fatalf("GET should pass in read-only mode: %s", text)
	}
	text, isErr = call(t, s, "remote_api", `{"method":"DELETE","path":"/api/thing"}`)
	if !isErr || !strings.Contains(text, "read-only mode") {
		t.Fatalf("non-GET must be refused in read-only mode: %s", text)
	}
}

func TestRemoteRoutesFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/routes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"routes":[
			{"method":"GET","pattern":"/api/runs"},
			{"method":"POST","pattern":"/api/webhooks/github"},
			{"method":"GET","pattern":"/api/webhooks"}
		]}`))
	})
	newRemoteStub(t, mux)

	s := newTestServer(t)
	text, isErr := call(t, s, "remote_routes", `{"filter":"webhooks"}`)
	if isErr {
		t.Fatalf("routes errored: %s", text)
	}
	var got struct {
		Routes []map[string]string `json:"routes"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if len(got.Routes) != 2 {
		t.Fatalf("filter should keep the two webhook routes: %+v", got.Routes)
	}
}

func TestRemoteOpenAPIPathPrefixFilter(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"openapi":"3.1.0","info":{"title":"iterion"},"paths":{
			"/api/runs":{"get":{}},
			"/api/webhooks/github":{"post":{}}
		}}`))
	})
	newRemoteStub(t, mux)

	s := newTestServer(t)
	text, isErr := call(t, s, "remote_openapi", `{"path_prefix":"/api/webhooks"}`)
	if isErr {
		t.Fatalf("openapi errored: %s", text)
	}
	var got struct {
		Paths map[string]any `json:"paths"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, text)
	}
	if len(got.Paths) != 1 {
		t.Fatalf("prefix filter should keep exactly one path: %v", got.Paths)
	}
	if _, ok := got.Paths["/api/webhooks/github"]; !ok {
		t.Fatalf("wrong path kept: %v", got.Paths)
	}
}
