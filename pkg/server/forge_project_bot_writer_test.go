package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// The identity every server-mediated forge write posts with: on a
// project-bot connection, the repository's minted bot — never the person
// whose OAuth grant bootstrapped the connection, who must stay a reviewable
// author while the bot's own echoes are recognised by the guard.
func TestForgeWriterFor_ProjectBotConnectionPostsAsTheBot(t *testing.T) {
	gl := &projectBotGitLab{}
	srv := gl.server()
	defer srv.Close()
	s := newProjectBotTestServer(t)
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"`+srv.URL+`","client_id":"cid","client_secret":"csecret"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	conn := oauthConnectProjectBot(t, s, srv.URL)

	// Before the enable there is no bot to post as, and no fallback to the grant.
	if _, err := s.forgeWriterFor(context.Background(), conn, "group/api"); !errors.Is(err, forge.ErrRepoNotEnabled) {
		t.Fatalf("writer before enable: err = %v, want ErrRepoNotEnabled", err)
	}

	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+conn.ID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	writer, err := s.forgeWriterFor(context.Background(), conn, "group/api")
	if err != nil {
		t.Fatal(err)
	}
	ident, err := writer.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ident.Login != pbBot || ident.Kind != forge.AccountKindBot {
		t.Errorf("the writer authenticates as %+v, want the minted bot @%s", ident, pbBot)
	}
	// The admin client is still the grant: hooks and the mint stay the person's.
	admin, err := s.forgeAdminFor(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if ident, _ := admin.WhoAmI(context.Background()); ident.Login != "alice" {
		t.Errorf("the admin client authenticates as %q, want the grant's user alice", ident.Login)
	}
	// Another repository of the same connection has its own bot or nothing.
	if _, err := s.forgeWriterFor(context.Background(), conn, "group/other"); !errors.Is(err, forge.ErrRepoNotEnabled) {
		t.Errorf("writer for an unenabled repo: err = %v", err)
	}
	if _, err := s.forgeWriterFor(context.Background(), conn, ""); !errors.Is(err, forge.ErrRepoNotEnabled) {
		t.Errorf("writer with no repo: err = %v", err)
	}
	// A connection that posts as itself keeps its own credential.
	pat := forge.Connection{ID: "c-pat", TenantID: "t1", Provider: forge.ProviderGitLab, Kind: forge.KindPAT, ForgeBaseURL: srv.URL, AccountLogin: "alice"}
	sealed, err := forge.SealPAT(s.sealer, pat.ID, pbGrant)
	if err != nil {
		t.Fatal(err)
	}
	pat.SealedPayload = sealed
	if err := s.forgeConnections.Create(context.Background(), pat); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.forgeWriterFor(context.Background(), pat, "group/api")
	if err != nil {
		t.Fatal(err)
	}
	if ident, _ := legacy.WhoAmI(context.Background()); ident.Login != "alice" {
		t.Errorf("legacy writer = %q, want the connection's own account", ident.Login)
	}
}

// The publish lane end to end: Revi's review is posted through
// /api/v1/forge/publish-review with the run's grant, and the note must
// reach GitLab under the minted bot's token.
func TestForgePublishReview_ProjectBotConnectionPostsAsTheBot(t *testing.T) {
	gl := &projectBotGitLab{}
	inner := gl.server()
	defer inner.Close()
	var mu sync.Mutex
	var mrCalls []string
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.EscapedPath(), "/merge_requests/") {
			mu.Lock()
			mrCalls = append(mrCalls, r.Method+" "+r.URL.EscapedPath()+" auth="+gl.bearer(r))
			mu.Unlock()
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"iid": 7, "sha": "abcdef1234567", "diff_refs": map[string]any{"base_sha": "b", "head_sha": "abcdef1234567", "start_sha": "s"}})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer outer.Close()

	s := newProjectBotTestServer(t)
	ctx := superAdminCtx()
	w := httptest.NewRecorder()
	s.handleAdminRegisterForgeOAuthApp(w, forgeReq(ctx, "POST", "/api/admin/forge/oauth-apps",
		`{"provider":"gitlab","forge_base_url":"`+outer.URL+`","client_id":"cid","client_secret":"csecret"}`, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	conn := oauthConnectProjectBot(t, s, outer.URL)
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(ctx, "POST", "/api/teams/t1/forge/repo-bots",
		`{"connection_id":"`+conn.ID+`","repo":"group/api","bot_ids":["review-pr"]}`, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	if s.forgePublishTokens == nil {
		s.forgePublishTokens = NewForgePublishTokenRegistry()
	}
	if err := s.forgePublishTokens.Register("run-tok", ForgePublishGrant{TeamID: "t1", ConnectionID: conn.ID, Repo: "group/api"}); err != nil {
		t.Fatal(err)
	}
	s.forgeReviewClientFor = nil // the real client resolution
	r := httptest.NewRequest(http.MethodPost, "/api/v1/forge/publish-review",
		strings.NewReader(`{"pr_url":"`+outer.URL+`/group/api/-/merge_requests/7","summary":"Revi review summary"}`))
	r.Header.Set("X-Iterion-Run", "run-tok")
	w = httptest.NewRecorder()
	s.handleForgePublishReview(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("publish: code=%d body=%s", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	posted := 0
	for _, c := range mrCalls {
		if !strings.HasPrefix(c, "POST ") {
			continue
		}
		posted++
		if !strings.HasSuffix(c, "auth="+pbMinted) {
			t.Errorf("a write reached GitLab under the wrong credential: %s (want the minted token)", c)
		}
	}
	if posted == 0 {
		t.Fatalf("no write reached GitLab; calls: %v", mrCalls)
	}
}
