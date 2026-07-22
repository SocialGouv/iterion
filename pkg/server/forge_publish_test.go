package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// fakeReviewClient records the CreatePullReview call and returns a canned
// result — the seam the handler tests use instead of a live forge.
type fakeReviewClient struct {
	repo   string
	number int
	in     forge.NewReview
	res    forge.ReviewResult
	err    error
	calls  int
}

func (f *fakeReviewClient) CreatePullReview(_ context.Context, repo string, number int, in forge.NewReview) (forge.ReviewResult, error) {
	f.calls++
	f.repo, f.number, f.in = repo, number, in
	return f.res, f.err
}

func newForgePublishTestServer(t *testing.T) (*Server, *fakeReviewClient) {
	t.Helper()
	s := New(Config{}, iterlog.New(iterlog.LevelError, nil))
	s.forgeConnections = forge.NewMemoryConnectionStore()
	s.forgePublishTokens = NewForgePublishTokenRegistry()
	if err := s.forgeConnections.Create(context.Background(), forge.Connection{
		ID: "conn1", TenantID: "team1", Provider: forge.ProviderGitHub,
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeReviewClient{res: forge.ReviewResult{URL: "https://github.com/o/r/pull/42#review-1", CommentsPosted: 2, SuggestionsPosted: 1, Verified: true}}
	s.forgeReviewClientFor = func(_ context.Context, conn forge.Connection) (forge.ReviewClient, error) {
		return fake, nil
	}
	return s, fake
}

func registerPublishToken(t *testing.T, s *Server, token string, g ForgePublishGrant) {
	t.Helper()
	if err := s.forgePublishTokens.Register(token, g); err != nil {
		t.Fatal(err)
	}
}

func publishReq(token, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/publish-review", strings.NewReader(body))
	if token != "" {
		r.Header.Set("X-Iterion-Run", token)
	}
	return r
}

const validPublishBody = `{
  "pr_url": "https://github.com/o/r/pull/42",
  "summary": "review summary",
  "comments": [
    {"path": "a.go", "line": 3, "line_end": 5, "body": "finding", "suggestion": "fix()"},
    {"path": "b.go", "line": 9, "body": "plain"}
  ]
}`

func TestForgePublishReview_HappyPath(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Published || resp.Provider != "github" || resp.CommentsPosted != 2 || resp.SuggestionsPosted != 1 || !resp.Verified {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if fake.calls != 1 || fake.repo != "o/r" || fake.number != 42 {
		t.Fatalf("forge client called wrong: calls=%d repo=%q number=%d", fake.calls, fake.repo, fake.number)
	}
	if len(fake.in.Comments) != 2 || fake.in.Comments[0].LineEnd != 5 || fake.in.Comments[0].Suggestion != "fix()" {
		t.Fatalf("comments not mapped: %+v", fake.in.Comments)
	}
	if fake.in.Body != "review summary" {
		t.Fatalf("summary not mapped: %q", fake.in.Body)
	}
}

func TestForgePublishReview_AuthFailures(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

	// Missing token.
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("", validPublishBody))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: status = %d", w.Code)
	}
	// Unknown token.
	w = httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("nope", validPublishBody))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: status = %d", w.Code)
	}
	// Revoked token.
	s.forgePublishTokens.Revoke("tok1")
	w = httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: status = %d", w.Code)
	}
	if fake.calls != 0 {
		t.Fatalf("forge client must never be reached on auth failure (calls=%d)", fake.calls)
	}
}

func TestForgePublishReview_ValidationFailures(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

	cases := []struct {
		name string
		body string
		code int
	}{
		{"bad json", "{", http.StatusBadRequest},
		{"bad pr url", `{"pr_url": "https://github.com/o/r/issues/42", "summary": "x"}`, http.StatusBadRequest},
		{"repo mismatch", `{"pr_url": "https://github.com/other/repo/pull/42", "summary": "x"}`, http.StatusForbidden},
		{"bad mode", `{"pr_url": "https://github.com/o/r/pull/42", "summary": "x", "mode": "yolo"}`, http.StatusBadRequest},
		{"empty payload", `{"pr_url": "https://github.com/o/r/pull/42"}`, http.StatusBadRequest},
		{"bad comment", `{"pr_url": "https://github.com/o/r/pull/42", "summary": "x", "comments": [{"path": "", "line": 0, "body": ""}]}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		s.handleForgePublishReview(w, publishReq("tok1", tc.body))
		if w.Code != tc.code {
			t.Errorf("%s: status = %d, want %d (body: %s)", tc.name, w.Code, tc.code, w.Body.String())
		}
	}
	if fake.calls != 0 {
		t.Fatalf("forge client must never be reached on validation failure (calls=%d)", fake.calls)
	}
}

func TestForgePublishReview_ConnectionAndProviderFailures(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	// Grant pointing at a connection of ANOTHER team → 404 (non-enumeration).
	registerPublishToken(t, s, "tokX", ForgePublishGrant{TeamID: "team2", ConnectionID: "conn1", Repo: "o/r"})
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tokX", validPublishBody))
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-team grant: status = %d", w.Code)
	}

	// Provider without a review client → 501, explicit.
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	s.forgeReviewClientFor = func(context.Context, forge.Connection) (forge.ReviewClient, error) { return nil, nil }
	w = httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("no review client: status = %d", w.Code)
	}
}

func TestForgePublishReview_ForgeErrorIs502(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	fake.err = forge.StatusErr("github", "create pull review", 401)
	fake.res = forge.ReviewResult{}
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("forge failure must surface as 502, got %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "create pull review") {
		t.Fatalf("error body must carry the forge failure: %s", w.Body.String())
	}
}

func TestForgePublishReview_SummaryModeFoldsComments(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	body := `{"pr_url": "https://github.com/o/r/pull/42", "summary": "sum", "mode": "summary",
	          "comments": [{"path": "a.go", "line": 3, "body": "finding"}]}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(fake.in.Comments) != 0 {
		t.Fatalf("summary mode must not post inline comments: %+v", fake.in.Comments)
	}
	if !strings.Contains(fake.in.Body, "a.go:3") {
		t.Fatalf("summary mode must fold findings into the body: %q", fake.in.Body)
	}
}

func TestForgePublishTokenRegistry_TTLAndRevoke(t *testing.T) {
	r := NewForgePublishTokenRegistry()
	now := time.Now()
	r.now = func() time.Time { return now }
	if err := r.Register("t1", ForgePublishGrant{TeamID: "a", ConnectionID: "c", Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	if g, ok := r.lookup("t1"); !ok || g.Repo != "o/r" {
		t.Fatalf("lookup after register: ok=%v g=%+v", ok, g)
	}
	r.Revoke("t1")
	if _, ok := r.lookup("t1"); ok {
		t.Fatal("revoked token must not authorize")
	}
	if err := r.Register("t2", ForgePublishGrant{TeamID: "a", ConnectionID: "c", Repo: "o/r"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(forgePublishDefaultTTL + time.Minute)
	if _, ok := r.lookup("t2"); ok {
		t.Fatal("expired token must not authorize")
	}
}

func TestInjectForgePublishVars(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	s.cfg.PublicURL = "https://iterion.example"

	// No pr_url → untouched.
	vars := map[string]string{"base_ref": "main"}
	out := s.injectForgePublishVars(context.Background(), "team1", "", vars, nil)
	if _, ok := out[forgePublishVarToken]; ok {
		t.Fatal("no pr_url: nothing must be injected")
	}

	// pr_url on the connection's host → grant minted + vars injected.
	vars = map[string]string{"pr_url": "https://github.com/o/r/pull/42"}
	out = s.injectForgePublishVars(context.Background(), "team1", "", vars, nil)
	if out[forgePublishVarURL] != "https://iterion.example/api/v1/forge/publish-review" {
		t.Fatalf("url var = %q", out[forgePublishVarURL])
	}
	tok := out[forgePublishVarToken]
	if tok == "" {
		t.Fatal("token var not injected")
	}
	g, ok := s.forgePublishTokens.lookup(tok)
	if !ok || g.TeamID != "team1" || g.ConnectionID != "conn1" || g.Repo != "o/r" {
		t.Fatalf("grant wrong: ok=%v g=%+v", ok, g)
	}

	// Another team without a matching connection → untouched.
	out = s.injectForgePublishVars(context.Background(), "team2", "", map[string]string{"pr_url": "https://github.com/o/r/pull/42"}, nil)
	if _, ok := out[forgePublishVarToken]; ok {
		t.Fatal("no matching team connection: nothing must be injected")
	}

	// A PR on a host no connection covers → untouched.
	out = s.injectForgePublishVars(context.Background(), "team1", "", map[string]string{"pr_url": "https://gitlab.example/g/p/-/merge_requests/1"}, nil)
	if _, ok := out[forgePublishVarToken]; ok {
		t.Fatal("host mismatch: nothing must be injected")
	}
}

func TestForgePublishReview_HostMismatchRejected(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	// Grant repo matches, but the connection is a github.com connection and
	// the pr_url points at another forge host.
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	body := `{"pr_url": "https://ghe.internal/o/r/pull/42", "summary": "x"}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("host mismatch: status = %d", w.Code)
	}
	if fake.calls != 0 {
		t.Fatal("forge client must not be reached on host mismatch")
	}
}
