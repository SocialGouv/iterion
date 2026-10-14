package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
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
	s.gateSettles = newMemoryGateSettleStore(nil) // wired with the grants, as New does
	s.gateDecisions = newMemoryGateDecisionStore(nil)
	s.gateSweep = defaultGateSweepSettings() // not whatever ITERION_GATE_SWEEP_* the shell exports
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

// A review whose forge answers did not match what the pull request holds
// (a create that errored but persisted, a read-back that failed) reaches the
// caller as inline_diagnostics and the operator's log as one warn line per
// diagnostic — the trail that tells a miscount from a loss once the run is
// gone. Red if the handler drops ReviewResult.Diagnostics on either side.
func TestForgePublishReview_InlineDiagnosticsReachTheResponseAndTheLog(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	logged := warnLogOf(t, s)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	fake.res = forge.ReviewResult{
		URL: "https://gitlab.example/o/r/-/merge_requests/42", CommentsPosted: 2, SuggestionsPosted: 1, Verified: true,
		Diagnostics: []string{
			"inline comment a.go:3: HTTP 500 Internal Server Error — the merge request holds it anyway",
			"inline comment b.go:9: HTTP 500 Internal Server Error — the merge request holds it anyway",
		},
	}

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.InlineDiagnostics) != 2 || resp.InlineDiagnostics[0] != fake.res.Diagnostics[0] || resp.InlineDiagnostics[1] != fake.res.Diagnostics[1] {
		t.Fatalf("inline_diagnostics = %q, want the two forge diagnostics verbatim", resp.InlineDiagnostics)
	}
	if !strings.Contains(w.Body.String(), `"inline_diagnostics"`) {
		t.Fatalf("response body lacks inline_diagnostics: %s", w.Body.String())
	}
	for _, d := range fake.res.Diagnostics {
		want := "forge publish: github o/r#42: " + d
		if n := strings.Count(logged.String(), want); n != 1 {
			t.Fatalf("warn line %q appears %d time(s), want exactly one:\n%s", want, n, logged.String())
		}
	}
}

// A review that landed nowhere answers 502 — and still carries what each
// create answered: the diagnostics reach the response and the warn log on
// the failure path too, the operator's only lead on a publish with no trace
// on the merge request.
func TestForgePublishReview_ReviewFailureStillReportsInlineDiagnostics(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	logged := warnLogOf(t, s)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	fake.err = errors.New("gitlab: create merge request summary note: HTTP 403")
	fake.res = forge.ReviewResult{Diagnostics: []string{`inline comment "a.go":3: HTTP 400 {"message":{"line_code":["can't be blank"]}}`}}

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Published || len(resp.InlineDiagnostics) != 1 || resp.InlineDiagnostics[0] != fake.res.Diagnostics[0] {
		t.Fatalf("inline_diagnostics = %q on the failure path, want the forge diagnostic verbatim", resp.InlineDiagnostics)
	}
	if want := "forge publish: github o/r#42: " + fake.res.Diagnostics[0]; strings.Count(logged.String(), want) != 1 {
		t.Fatalf("log lacks the warn line %q:\n%s", want, logged.String())
	}
	// A caller keeping only the head of the body (review-pr keeps 500
	// bytes) must still read the reason: the diagnostics come LAST.
	if body := w.Body.String(); strings.Index(body, `"skipped_reason"`) > strings.Index(body, `"inline_diagnostics"`) {
		t.Fatalf("inline_diagnostics must follow skipped_reason in the body: %s", body)
	}
}

// warnLogOf makes the server log at warn into a buffer the handler's own
// goroutines may write too, and stubs the reviewer bookkeeping those
// goroutines run (self-assign, withdraw) to "no capability", so the warn
// lines a test reads are the publish's alone.
func warnLogOf(t *testing.T, s *Server) *lockedBuffer {
	t.Helper()
	logged := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelWarn, logged)
	s.forgeReviewerAssignerFor = func(context.Context, forge.Connection) forge.ReviewerAssigner { return nil }
	s.forgeReviewRequestWithdrawerFor = func(context.Context, forge.Connection) forge.ReviewRequestWithdrawer { return nil }
	return logged
}

// A clean publish carries no diagnostics: the field is absent from the body
// and nothing is logged at warn — so an operator grepping the warn line
// sees only the publishes that need a look.
func TestForgePublishReview_CleanPublishHasNoInlineDiagnostics(t *testing.T) {
	s, _ := newForgePublishTestServer(t)
	logged := warnLogOf(t, s)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", validPublishBody))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "inline_diagnostics") {
		t.Fatalf("clean publish must omit inline_diagnostics: %s", w.Body.String())
	}
	if l := logged.String(); strings.Contains(l, "forge publish: github o/r#42: ") {
		t.Fatalf("clean publish must log no diagnostic:\n%s", l)
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
		// A malformed comment is no longer fatal when there is still
		// something to say — see TestForgePublishReview_DropsUnanchorableComments.
		// It stays fatal only when nothing publishable remains.
		{"unanchorable comment and no summary", `{"pr_url": "https://github.com/o/r/pull/42", "comments": [{"path": "", "line": 0, "body": ""}]}`, http.StatusBadRequest},
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
	out, _, err := s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", vars, nil, store.RunTrustDefault)
	if err != nil {
		t.Fatalf("no pr_url: %v", err)
	}
	if _, ok := out[forgePublishVarToken]; ok {
		t.Fatal("no pr_url: nothing must be injected")
	}

	// pr_url on the connection's host → grant minted + vars injected.
	vars = map[string]string{"pr_url": "https://github.com/o/r/pull/42"}
	out, _, err = s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", vars, nil, store.RunTrustDefault)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
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
	out, _, err = s.injectForgePublishVars(context.Background(), "team2", "", "review-pr", map[string]string{"pr_url": "https://github.com/o/r/pull/42"}, nil, store.RunTrustDefault)
	if err != nil {
		t.Fatalf("no matching team connection: %v", err)
	}
	if _, ok := out[forgePublishVarToken]; ok {
		t.Fatal("no matching team connection: nothing must be injected")
	}

	// A PR on a host no connection covers → untouched.
	out, _, err = s.injectForgePublishVars(context.Background(), "team1", "", "review-pr", map[string]string{"pr_url": "https://gitlab.example/g/p/-/merge_requests/1"}, nil, store.RunTrustDefault)
	if err != nil {
		t.Fatalf("host mismatch: %v", err)
	}
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

// A reviewer legitimately produces findings it cannot anchor to a file+line
// (an architectural observation, a cross-file concern). Rejecting the batch
// on one of those used to cost the whole review — and, because the merge gate
// is posted after a successful publish, the required check with it. That is
// how PR #304 ended up with a permanently absent revi/review status.
func TestForgePublishReview_DropsUnanchorableComments(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})

	body := `{
  "pr_url": "https://github.com/o/r/pull/42",
  "summary": "review summary",
  "comments": [
    {"path": "a.go", "line": 3, "body": "anchored finding"},
    {"path": "", "line": 0, "body": "architectural observation with nowhere to hang"},
    {"path": "b.go", "line": 0, "body": "no line"}
  ]
}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var res publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.Published {
		t.Error("published = false: the anchored finding and the summary must still land")
	}
	if len(res.DroppedComments) != 2 {
		t.Errorf("dropped_comments = %v, want 2 — a silent drop hides the caller's broken contract", res.DroppedComments)
	}
	// Only the anchorable comment reaches the forge.
	if len(fake.in.Comments) != 1 || fake.in.Comments[0].Path != "a.go" {
		t.Errorf("forge received %+v, want only the a.go comment", fake.in.Comments)
	}
}

// The gate is a COUNT the bot already computed; it does not depend on the
// comments landing. Coupling them meant one forge hiccup left the PR's
// required check absent — indistinguishable from "never reviewed", and
// unblockable by another review.
func TestForgePublishReview_GatePostedEvenWhenTheReviewFails(t *testing.T) {
	s, fake := newForgePublishTestServer(t)
	registerPublishToken(t, s, "tok1", ForgePublishGrant{TeamID: "team1", ConnectionID: "conn1", Repo: "o/r"})
	fake.err = errors.New("forge exploded")

	gate := &fakeGateClient{headSHA: "deadbeef"}
	s.forgeGateClientFor = func(_ context.Context, _ forge.Connection) (forgeGateClient, error) {
		return gate, nil
	}

	body := `{
  "pr_url": "https://github.com/o/r/pull/42",
  "summary": "review summary",
  "gate": {"enabled": true, "context": "revi/review", "blocking_count": 0, "threshold": "high", "total_findings": 2}
}`
	w := httptest.NewRecorder()
	s.handleForgePublishReview(w, publishReq("tok1", body))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — the publish failure must still be reported", w.Code)
	}
	var res publishReviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Published {
		t.Error("published = true despite a forge error")
	}
	if res.SkippedReason == "" {
		t.Error("skipped_reason empty: a failed publish must say why")
	}
	if !res.GatePosted {
		t.Fatal("gate_posted = false — the required check stays absent and the PR can never merge")
	}
	if gate.setCalls == 0 {
		t.Error("the forge never received a commit status")
	}
}
