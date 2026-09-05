package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

func TestWhoAmI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v4/user" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "alice", "email": "a@x.io"})
	}))
	defer srv.Close()

	id, err := New(srv.Client(), srv.URL, "tok-123").WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Login != "alice" || id.ID != "7" || id.Kind != "user" {
		t.Errorf("identity = %+v", id)
	}
}

// TestCreateHook_BooleanBodyShape is the deterministic stand-in for risk #1
// in the plan: GitLab's POST /hooks takes BOOLEAN event fields, not an
// events array. This pins the exact request body so a regression in the
// translation (event_map / events.go) fails here, not silently on a live
// GitLab.
func TestCreateHook_BooleanBodyShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fapi/hooks") {
			t.Errorf("escaped path = %q, want namespaced project id", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 42, "url": gotBody["url"], "merge_requests_events": true, "note_events": true,
		})
	}))
	defer srv.Close()

	h, err := New(srv.Client(), srv.URL, "tok").CreateHook(context.Background(), "group/api", forge.HookSpec{
		URL:    "https://iterion.example.com/api/webhooks/gitlab/wh1",
		Secret: "iwh_secret",
		Events: []string{"merge_request", "note"},
		Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The boolean translation is the crux.
	if gotBody["merge_requests_events"] != true {
		t.Errorf("merge_requests_events = %v, want true", gotBody["merge_requests_events"])
	}
	if gotBody["note_events"] != true {
		t.Errorf("note_events = %v, want true", gotBody["note_events"])
	}
	if gotBody["push_events"] != false {
		t.Errorf("push_events = %v, want false", gotBody["push_events"])
	}
	if gotBody["enable_ssl_verification"] != true {
		t.Errorf("enable_ssl_verification = %v, want true", gotBody["enable_ssl_verification"])
	}
	if gotBody["token"] != "iwh_secret" {
		t.Errorf("token = %v, want the iwh_ secret", gotBody["token"])
	}
	if h.ID != "42" {
		t.Errorf("hook id = %q, want 42", h.ID)
	}
	if len(h.Events) != 2 {
		t.Errorf("returned events = %v", h.Events)
	}
}

func TestCreateHook_SingleEventOmitsOther(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "url": gotBody["url"], "note_events": true})
	}))
	defer srv.Close()

	_, err := New(srv.Client(), srv.URL, "tok").CreateHook(context.Background(), "g/p", forge.HookSpec{
		URL: "u", Secret: "s", Events: []string{"note"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["note_events"] != true || gotBody["merge_requests_events"] != false {
		t.Errorf("single-event body wrong: %v", gotBody)
	}
}

func TestGetHook_MatchByURL(t *testing.T) {
	const wantURL = "https://iterion.example.com/api/webhooks/gitlab/wh1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "url": "https://other/hook", "merge_requests_events": true},
			{"id": 2, "url": wantURL, "merge_requests_events": true, "note_events": true},
		})
	}))
	defer srv.Close()

	c := New(srv.Client(), srv.URL, "tok")
	got, err := c.GetHook(context.Background(), "g/p", wantURL)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != "2" {
		t.Fatalf("got = %+v, want hook id 2", got)
	}

	none, err := c.GetHook(context.Background(), "g/p", "https://nomatch")
	if err != nil || none != nil {
		t.Errorf("expected no match, got %+v err %v", none, err)
	}
}

func TestDeleteHook_404IsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	err := New(srv.Client(), srv.URL, "tok").DeleteHook(context.Background(), "g/p", "9")
	if !errors.Is(err, forge.ErrHookNotFound) {
		t.Errorf("delete 404 = %v, want ErrHookNotFound", err)
	}
}

func TestCreateHook_403IsForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "tok").CreateHook(context.Background(), "g/p", forge.HookSpec{URL: "u", Events: []string{"note"}})
	if !errors.Is(err, forge.ErrForbidden) {
		t.Errorf("create 403 = %v, want ErrForbidden", err)
	}
}

func TestListRepos_FiltersAndMaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("min_access_level") != "40" {
			t.Errorf("min_access_level = %q, want 40", r.URL.Query().Get("min_access_level"))
		}
		if r.URL.Query().Get("membership") != "true" {
			t.Errorf("membership = %q", r.URL.Query().Get("membership"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "path_with_namespace": "group/api", "visibility": "private", "default_branch": "main", "web_url": "https://gl/group/api"},
		})
	}))
	defer srv.Close()

	repos, err := New(srv.Client(), srv.URL, "tok").ListRepos(context.Background(), forge.RepoQuery{Search: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("repos = %d", len(repos))
	}
	r := repos[0]
	if r.FullName != "group/api" || !r.Private || r.DefaultBranch != "main" || !r.CanAdmin {
		t.Errorf("repo mapping wrong: %+v", r)
	}
}

func TestListHooks_ReturnsAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fapi/hooks") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "url": "https://other/hook", "push_events": true},
			{"id": 2, "url": "https://iterion/wh", "merge_requests_events": true, "note_events": true},
		})
	}))
	defer srv.Close()

	hooks, err := New(srv.Client(), srv.URL, "tok").ListHooks(context.Background(), "group/api")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 {
		t.Fatalf("want all 2 hooks, got %d", len(hooks))
	}
	if hooks[0].ID != "1" || hooks[0].URL != "https://other/hook" {
		t.Errorf("hook[0] = %+v", hooks[0])
	}
	if hooks[1].ID != "2" || hooks[1].URL != "https://iterion/wh" || len(hooks[1].Events) != 2 {
		t.Errorf("hook[1] = %+v", hooks[1])
	}
}

func TestGitLabCommentIssue_PostsNote(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fapi/issues/7/notes") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 55, "body": body["body"], "author": map[string]any{"username": "bot"},
		})
	}))
	defer srv.Close()

	got, err := New(srv.Client(), srv.URL, "tok").CommentIssue(context.Background(), "group/api", 7, "looks good")
	if err != nil {
		t.Fatal(err)
	}
	if body["body"] != "looks good" {
		t.Errorf("request body = %v", body)
	}
	if got.ID != "55" || got.Author != "bot" || got.Body != "looks good" {
		t.Errorf("note = %+v", got)
	}
}

// TestGitLabCommentPullRequest_PostsNote pins CommentPullRequest onto the
// merge_requests notes endpoint — SEPARATE from CommentIssue's
// /issues/:iid/notes, since GitLab addresses an MR and an issue as distinct
// resources that can share the same iid in one project. A caller that knows
// its subject is an MR (the /revi approve reply lane) must not go through
// CommentIssue and land on — or 404 against — the wrong resource.
func TestGitLabCommentPullRequest_PostsNote(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fapi/merge_requests/7/notes") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 56, "body": body["body"], "author": map[string]any{"username": "bot"},
		})
	}))
	defer srv.Close()

	got, err := New(srv.Client(), srv.URL, "tok").CommentPullRequest(context.Background(), "group/api", 7, "cannot approve: no gate context pinned")
	if err != nil {
		t.Fatal(err)
	}
	if body["body"] != "cannot approve: no gate context pinned" {
		t.Errorf("request body = %v", body)
	}
	if got.ID != "56" || got.Author != "bot" {
		t.Errorf("note = %+v", got)
	}
}

func TestGitLabCreatePull_MapsBranchesAndDraft(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fapi/merge_requests") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid": 9, "title": body["title"], "state": "opened", "web_url": "https://gl/mr/9",
			"source_branch": body["source_branch"], "target_branch": body["target_branch"],
		})
	}))
	defer srv.Close()

	pr, err := New(srv.Client(), srv.URL, "tok").CreatePull(context.Background(), "group/api", forge.NewPull{
		Title: "feat: x", Body: "details", SourceBranch: "feature", TargetBranch: "main", Draft: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["source_branch"] != "feature" || body["target_branch"] != "main" {
		t.Errorf("branch mapping wrong: %v", body)
	}
	if body["description"] != "details" {
		t.Errorf("description = %v", body["description"])
	}
	// Draft → "Draft: " title prefix.
	if body["title"] != "Draft: feat: x" {
		t.Errorf("draft title prefix = %v want 'Draft: feat: x'", body["title"])
	}
	if pr.Number != 9 || pr.SourceBranch != "feature" || pr.TargetBranch != "main" {
		t.Errorf("created pr = %+v", pr)
	}
}

func TestGitLabUpdatePull_StateEventAndTarget(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fapi/merge_requests/3") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid": 3, "title": "t", "state": "closed", "web_url": "u",
			"target_branch": body["target_branch"],
		})
	}))
	defer srv.Close()

	closed, target := "closed", "develop"
	pr, err := New(srv.Client(), srv.URL, "tok").UpdatePull(context.Background(), "group/api", 3, forge.PullPatch{State: &closed, TargetBranch: &target})
	if err != nil {
		t.Fatal(err)
	}
	// State="closed" → state_event:"close" (GitLab's transition verb).
	if body["state_event"] != "close" {
		t.Errorf("state_event = %v want close", body["state_event"])
	}
	if body["target_branch"] != "develop" {
		t.Errorf("target_branch = %v want develop", body["target_branch"])
	}
	if _, present := body["title"]; present {
		t.Errorf("nil patch field leaked: %v", body)
	}
	if pr.State != "closed" || pr.TargetBranch != "develop" {
		t.Errorf("updated pr = %+v", pr)
	}
}

func TestGitLabMergePull_SquashAndRemoveSource(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.URL.EscapedPath(); !strings.HasSuffix(got, "/projects/group%2Fapi/merge_requests/5/merge") {
			t.Errorf("escaped path = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"iid": 5, "title": "t", "state": "merged", "web_url": "https://gl/mr/5",
		})
	}))
	defer srv.Close()

	pr, err := New(srv.Client(), srv.URL, "tok").MergePull(context.Background(), "group/api", 5, forge.MergeOptions{
		Method: forge.MergeSquash, DeleteBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["squash"] != true {
		t.Errorf("squash = %v want true", body["squash"])
	}
	if body["should_remove_source_branch"] != true {
		t.Errorf("should_remove_source_branch = %v want true", body["should_remove_source_branch"])
	}
	if pr.State != "merged" {
		t.Errorf("merged pr state = %q want merged", pr.State)
	}
}

// compile-time assertion that AdminClient satisfies forge.Admin.
var _ forge.Admin = (*AdminClient)(nil)

// GitLab flags the bot user behind a group/project access token (and service
// accounts) with `bot: true`; that flag is what lets iterion rebrand the
// account unasked, so its reading is pinned.
func TestWhoAmI_BotFlagMarksAccountKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 575, "username": "group_1026_bot_a7c08cc4", "bot": true})
	}))
	defer srv.Close()

	id, err := New(srv.Client(), srv.URL, "tok").WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.Kind != forge.AccountKindBot || id.Login != "group_1026_bot_a7c08cc4" {
		t.Errorf("identity = %+v, want a bot", id)
	}
}

// TestSetAvatar_MultipartShape pins the exact request GitLab's PUT /user/avatar
// takes: a multipart form with ONE file part named `avatar`, typed image/png,
// carrying the bytes verbatim — under the token's Bearer header.
func TestSetAvatar_MultipartShape(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nnot-really-a-png")
	var (
		gotMethod, gotPath, gotAuth, gotField, gotFilename, gotType string
		gotBytes                                                    []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not a multipart request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			t.Errorf("no part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotField, gotFilename, gotType = part.FormName(), part.FileName(), part.Header.Get("Content-Type")
		gotBytes, _ = io.ReadAll(part)
		_ = json.NewEncoder(w).Encode(map[string]any{"avatar_url": "https://gl/uploads/-/system/user/avatar/575/avatar.png"})
	}))
	defer srv.Close()

	url, err := New(srv.Client(), srv.URL, "tok-123").SetAvatar(context.Background(), png)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://gl/uploads/-/system/user/avatar/575/avatar.png" {
		t.Errorf("avatar url = %q", url)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v4/user/avatar" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotField != "avatar" || gotFilename != "iterion-bot.png" || gotType != "image/png" {
		t.Errorf("part = field %q file %q type %q", gotField, gotFilename, gotType)
	}
	if !bytes.Equal(gotBytes, png) {
		t.Errorf("uploaded bytes differ: %d vs %d", len(gotBytes), len(png))
	}
}

// Each refusal keeps its meaning: a 404 is the instance lacking the route (not
// a missing hook), a 400 quotes GitLab's reason, 401/403 map to the sentinels.
func TestSetAvatar_Errors(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		body     string
		wantIs   error
		wantText string
	}{
		{"too big", http.StatusBadRequest, `{"message":{"avatar":["is too big (should be at most 200 KB)"]}}`, nil, "is too big"},
		{"pre-17.0 instance", http.StatusNotFound, `{"error":"404 Not Found"}`, forge.ErrAvatarUnsupported, "17.0"},
		{"revoked token", http.StatusUnauthorized, "", forge.ErrUnauthorized, ""},
		{"forbidden", http.StatusForbidden, "", forge.ErrForbidden, ""},
		{"server error", http.StatusInternalServerError, "<html>boom</html>", nil, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.Client(), srv.URL, "tok").SetAvatar(context.Background(), []byte("png"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.wantIs)
			}
			if tc.wantIs == nil && errors.Is(err, forge.ErrHookNotFound) {
				t.Errorf("err = %v leaked the hook sentinel", err)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("err = %q, want it to mention %q", err, tc.wantText)
			}
		})
	}
}

// A 404 is either the missing route (GitLab < 17.0) or a base URL that
// redirects — the client follows a 301/302 as a body-less GET, which GitLab
// answers 404. The error must keep ErrAvatarUnsupported AND quote the
// instance so the operator can tell the two apart; and any 2xx is a success.
func TestSetAvatar_RedirectAndOther2xx(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.Method == http.MethodPut {
			http.Redirect(w, r, "/api/v4/user/avatar", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"404 Not Found"}`))
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "tok").SetAvatar(context.Background(), []byte("png"))
	if !errors.Is(err, forge.ErrAvatarUnsupported) || !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("err = %v; methods seen %v", err, methods)
	}

	created := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"avatar_url": "https://gl/u.png"})
	}))
	defer created.Close()
	if url, err := New(created.Client(), created.URL, "tok").SetAvatar(context.Background(), []byte("png")); err != nil || url != "https://gl/u.png" {
		t.Errorf("a 201 is a success: url=%q err=%v", url, err)
	}
}
