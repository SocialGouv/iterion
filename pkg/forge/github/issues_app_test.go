package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The App client is the PRODUCTION shape of a GitHub connection (the studio's
// connect wizard creates github_app connections by default), so every
// capability the server type-asserts must exist on it too. A capability
// implemented on *AdminClient alone is invisible to `admin.(forge.IssueClient)`
// and the board sync fails at the assertion, not at a call it could explain.
var (
	_ forge.IssueClient = (*AdminClient)(nil)
	_ forge.IssueClient = (*AppClient)(nil)
)

// issueMintRecorder is a fake GitHub serving the installation-token mint and
// the issues endpoints, recording the permission set of every mint and the
// bearer that reached each REST call.
type issueMintRecorder struct {
	mu      sync.Mutex
	mints   []map[string]string
	bearers []string
	paths   []string
	srv     *httptest.Server
}

func newIssueMintRecorder(t *testing.T) *issueMintRecorder {
	t.Helper()
	r := &issueMintRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/access_tokens") {
			var body struct {
				Permissions map[string]string `json:"permissions"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			r.mu.Lock()
			r.mints = append(r.mints, body.Permissions)
			n := len(r.mints)
			r.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_scoped_" + string(rune('a'+n-1)),
				"expires_at": "2099-01-01T00:00:00Z",
			})
			return
		}
		r.mu.Lock()
		r.bearers = append(r.bearers, req.Header.Get("Authorization"))
		// The full request URI, query included: "the same endpoint" means the
		// same filters too, not just the same path.
		r.paths = append(r.paths, req.Method+" "+req.URL.RequestURI())
		r.mu.Unlock()
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242, "body": "hello", "html_url": "https://gh/c/4242"})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/issues"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"number": 7, "title": "boom", "state": "open", "html_url": "https://gh/x/7"},
			})
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/issues"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 8, "title": "made", "state": "open"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "title": "boom", "state": "open"})
		}
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *issueMintRecorder) snapshot() ([]map[string]string, []string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]string(nil), r.mints...),
		append([]string(nil), r.bearers...),
		append([]string(nil), r.paths...)
}

func (r *issueMintRecorder) appClient(t *testing.T) *AppClient {
	t.Helper()
	return &AppClient{
		HTTP: r.srv.Client(), WebBaseURL: r.srv.URL,
		Cfg: AppConfig{AppID: 42, PrivateKeyPEM: testKeyPEMOnce(t), AppSlug: "iterion"}, InstallationID: 99,
	}
}

// testKeyPEMOnce caches one generated key for the file's tests: RSA keygen
// dominates their runtime otherwise.
var (
	testKeyOnce sync.Once
	testKeyPEMv string
)

func testKeyPEMOnce(t *testing.T) string {
	t.Helper()
	testKeyOnce.Do(func() { testKeyPEMv, _ = testKeyPEM(t) })
	return testKeyPEMv
}

// A read through the App client mints a READ-ONLY issues token and calls the
// exact endpoint the PAT client calls. Widening it to issues:write — which the
// cached runtime token already carries — would hand a listing the permission to
// rewrite every issue in the installation.
func TestAppClientListIssuesMintsAReadScopedToken(t *testing.T) {
	r := newIssueMintRecorder(t)
	opts := forge.IssueListOptions{State: "all", PerPage: 50, Page: 1}

	// The PAT client first: its recorded request IS the reference the App
	// client's delegation has to reproduce.
	pat := &AdminClient{HTTP: r.srv.Client(), APIBase: APIBaseFor(r.srv.URL), Token: "ghp_pat"}
	if _, err := pat.ListIssues(context.Background(), "SocialGouv/iterion", opts); err != nil {
		t.Fatalf("PAT ListIssues: %v", err)
	}

	issues, err := r.appClient(t).ListIssues(context.Background(), "SocialGouv/iterion", opts)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 7 {
		t.Fatalf("issues = %+v, want the one issue the forge served", issues)
	}
	mints, bearers, paths := r.snapshot()
	if len(mints) != 1 {
		t.Fatalf("mints = %d, want exactly 1 (the PAT call mints nothing)", len(mints))
	}
	if mints[0]["issues"] != "read" {
		t.Errorf("mint permissions = %v, want issues:read", mints[0])
	}
	if mints[0]["metadata"] != "read" {
		t.Errorf("mint permissions = %v, want the mandatory metadata baseline", mints[0])
	}
	for _, extra := range []string{"contents", "pull_requests", "repository_hooks", "statuses"} {
		if _, ok := mints[0][extra]; ok {
			t.Errorf("a read token must not carry %s, got %v", extra, mints[0])
		}
	}
	if len(paths) != 2 || paths[0] != paths[1] {
		t.Errorf("paths = %v, want the App client to hit the PAT client's own endpoint", paths)
	}
	if len(bearers) != 2 || bearers[0] != "Bearer ghp_pat" || !strings.HasPrefix(bearers[1], "Bearer ghs_scoped_") {
		t.Errorf("bearers = %v, want [the PAT, the minted installation token]", bearers)
	}
}

// A sync pass walks hundreds of issues; a token minted per call would cost a
// mint per issue and burn the App's rate budget. The scoped token is cached
// per permission set, like the runtime one.
func TestAppClientIssueReadsReuseTheCachedToken(t *testing.T) {
	r := newIssueMintRecorder(t)
	a := r.appClient(t)
	ctx := context.Background()
	if _, err := a.ListIssues(ctx, "o/r", forge.IssueListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ListIssues(ctx, "o/r", forge.IssueListOptions{Page: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetIssue(ctx, "o/r", 7); err != nil {
		t.Fatal(err)
	}
	mints, bearers, _ := r.snapshot()
	if len(mints) != 1 {
		t.Fatalf("mints = %d over 3 reads, want 1 (the scoped token is cached)", len(mints))
	}
	if len(bearers) != 3 {
		t.Fatalf("bearers = %d, want one per REST call", len(bearers))
	}
	for _, b := range bearers[1:] {
		if b != bearers[0] {
			t.Errorf("every read must reuse the same cached token, got %v", bearers)
		}
	}
}

// A write mints its OWN issues:write token: the read token cannot serve it,
// and the read must not be widened to make one token do both.
func TestAppClientIssueWritesMintTheirOwnScopedToken(t *testing.T) {
	r := newIssueMintRecorder(t)
	a := r.appClient(t)
	ctx := context.Background()
	if _, err := a.ListIssues(ctx, "o/r", forge.IssueListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateIssue(ctx, "o/r", forge.NewIssue{Title: "made"}); err != nil {
		t.Fatal(err)
	}
	title := "renamed"
	if _, err := a.UpdateIssue(ctx, "o/r", 7, forge.IssuePatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	mints, bearers, _ := r.snapshot()
	if len(mints) != 2 {
		t.Fatalf("mints = %d, want 2 (one read scope, one write scope, both cached)", len(mints))
	}
	if mints[0]["issues"] != "read" || mints[1]["issues"] != "write" {
		t.Fatalf("mint scopes = %v, want [issues:read, issues:write]", mints)
	}
	if len(bearers) != 3 || bearers[1] != bearers[2] {
		t.Errorf("the two writes must share one cached write token, got %v", bearers)
	}
	if bearers[0] == bearers[1] {
		t.Error("the read and the write must not share a token: that is the widening this test forbids")
	}
}

// A comment is the one issue call whose permission depends on WHAT it targets:
// GitHub serves PR comments from the issues endpoint but gates them on
// `pull_requests`, and every AppClient.CommentIssue caller in pkg/server
// targets a pull request (the gate pause notice, the DLQ notice, the
// approve-rejection reply). All three swallow the error at Debug, so a token
// short of that grant would turn every notice into a silent 403.
func TestAppClientCommentIssueMintsAPullRequestScopedToken(t *testing.T) {
	r := newIssueMintRecorder(t)

	// The PAT client first: its recorded request is the reference the App
	// client's delegation has to reproduce.
	pat := &AdminClient{HTTP: r.srv.Client(), APIBase: APIBaseFor(r.srv.URL), Token: "ghp_pat"}
	if _, err := pat.CommentIssue(context.Background(), "o/r", 12, "hello"); err != nil {
		t.Fatalf("PAT CommentIssue: %v", err)
	}

	ref, err := r.appClient(t).CommentIssue(context.Background(), "o/r", 12, "hello")
	if err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}
	if ref.ID != "4242" {
		t.Errorf("comment ref = %+v, want the comment the forge created", ref)
	}
	mints, bearers, paths := r.snapshot()
	if len(mints) != 1 {
		t.Fatalf("mints = %d, want exactly 1 (the PAT call mints nothing)", len(mints))
	}
	if mints[0]["pull_requests"] != "write" {
		t.Errorf("mint permissions = %v, want pull_requests:write — a comment on a PR number is "+
			"refused without it (403 \"Resource not accessible by integration\")", mints[0])
	}
	if mints[0]["issues"] != "write" {
		t.Errorf("mint permissions = %v, want issues:write — the same call comments on a real issue too", mints[0])
	}
	if mints[0]["metadata"] != "read" {
		t.Errorf("mint permissions = %v, want the mandatory metadata baseline", mints[0])
	}
	for _, extra := range []string{"contents", "repository_hooks", "statuses"} {
		if _, ok := mints[0][extra]; ok {
			t.Errorf("a comment token must not carry %s (that is the runtime token's grant), got %v", extra, mints[0])
		}
	}
	if len(paths) != 2 || paths[0] != paths[1] {
		t.Errorf("paths = %v, want the App client to hit the PAT client's own comment endpoint", paths)
	}
	if len(bearers) != 2 || bearers[0] != "Bearer ghp_pat" || !strings.HasPrefix(bearers[1], "Bearer ghs_scoped_") {
		t.Errorf("bearers = %v, want [the PAT, the minted installation token]", bearers)
	}
}

// The comment scope is its OWN cached set: a read must not inherit
// pull_requests:write by sharing a token with it.
func TestAppClientCommentAndReadDoNotShareAToken(t *testing.T) {
	r := newIssueMintRecorder(t)
	a := r.appClient(t)
	ctx := context.Background()
	if _, err := a.ListIssues(ctx, "o/r", forge.IssueListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CommentIssue(ctx, "o/r", 12, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CommentIssue(ctx, "o/r", 13, "again"); err != nil {
		t.Fatal(err)
	}
	mints, bearers, _ := r.snapshot()
	if len(mints) != 2 {
		t.Fatalf("mints = %d, want 2 (one read scope, one comment scope, both cached)", len(mints))
	}
	if _, ok := mints[0]["pull_requests"]; ok {
		t.Errorf("the read token must not carry pull_requests, got %v", mints[0])
	}
	if mints[1]["pull_requests"] != "write" {
		t.Errorf("the comment token = %v, want pull_requests:write (its callers all target a PR)", mints[1])
	}
	if bearers[0] == bearers[1] {
		t.Error("the read and the comment must not share a token")
	}
	if bearers[1] != bearers[2] {
		t.Errorf("the two comments must share one cached token, got %v", bearers)
	}
}

// The scoped profiles are their own, and never leak into the runtime baseline
// the cached management token carries.
func TestIssuePermissionProfiles(t *testing.T) {
	read := IssuesReadInstallationPermissions()
	if read["issues"] != "read" || read["metadata"] != "read" || len(read) != 2 {
		t.Errorf("read profile = %v, want exactly issues:read + metadata:read", read)
	}
	write := IssuesWriteInstallationPermissions()
	if write["issues"] != "write" || write["metadata"] != "read" || len(write) != 2 {
		t.Errorf("write profile = %v, want exactly issues:write + metadata:read", write)
	}
	comment := IssueCommentInstallationPermissions()
	if comment["issues"] != "write" || comment["pull_requests"] != "write" || comment["metadata"] != "read" || len(comment) != 3 {
		t.Errorf("comment profile = %v, want exactly issues:write + pull_requests:write + metadata:read", comment)
	}
	// The three stay below the runtime baseline: none of them may acquire a
	// grant the management token holds for other work.
	base := RuntimeInstallationPermissions()
	for _, profile := range []map[string]string{read, write, comment} {
		for name := range profile {
			if _, ok := base[name]; !ok {
				t.Errorf("profile %v carries %s, which is not even in the runtime baseline", profile, name)
			}
		}
	}
}
