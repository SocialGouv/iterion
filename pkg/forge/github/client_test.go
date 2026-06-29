package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

var _ forge.Admin = (*AdminClient)(nil)
var _ forge.OAuthExchanger = (*OAuthApp)(nil)

func TestAPIBaseFor(t *testing.T) {
	cases := map[string]string{
		"":                         "https://api.github.com",
		"https://github.com":       "https://api.github.com",
		"https://ghe.example.com":  "https://ghe.example.com/api/v3",
		"https://ghe.example.com/": "https://ghe.example.com/api/v3",
	}
	for in, want := range cases {
		if got := APIBaseFor(in); got != want {
			t.Errorf("APIBaseFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitHubCreateHook_EventsArrayShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/octo/api/hooks") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "active": true, "events": body["events"], "config": body["config"]})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "tok"}
	h, err := c.CreateHook(context.Background(), "octo/api", forge.HookSpec{
		URL: "https://iterion/api/webhooks/github/wh1", Secret: "iwh_s", Events: []string{"pull_request", "issue_comment"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["name"] != "web" {
		t.Errorf("name = %v", body["name"])
	}
	evs, _ := body["events"].([]any)
	if len(evs) != 2 {
		t.Errorf("events = %v", body["events"])
	}
	config, _ := body["config"].(map[string]any)
	if config["url"] != "https://iterion/api/webhooks/github/wh1" || config["secret"] != "iwh_s" || config["content_type"] != "json" {
		t.Errorf("config = %v", config)
	}
	if h.ID != "11" {
		t.Errorf("hook id = %q", h.ID)
	}
}

func TestGitHubListRepos_AdminFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"full_name": "octo/api", "private": true, "default_branch": "main", "permissions": map[string]any{"admin": true}},
			{"full_name": "octo/readonly", "permissions": map[string]any{"admin": false}},
		})
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	repos, err := c.ListRepos(context.Background(), forge.RepoQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("repos = %d", len(repos))
	}
	if !repos[0].CanAdmin || repos[1].CanAdmin {
		t.Errorf("admin flags wrong: %+v", repos)
	}
}

func TestGitHubListHooks_ReturnsAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/repos/o/r/hooks") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "active": true, "events": []string{"push"}, "config": map[string]any{"url": "https://other/hook"}},
			{"id": 2, "active": false, "events": []string{"pull_request"}, "config": map[string]any{"url": "https://iterion/wh"}},
		})
	}))
	defer srv.Close()

	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	hooks, err := c.ListHooks(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 2 {
		t.Fatalf("want all 2 hooks, got %d", len(hooks))
	}
	if hooks[0].ID != "1" || hooks[0].URL != "https://other/hook" || !hooks[0].Active {
		t.Errorf("hook[0] = %+v", hooks[0])
	}
	if hooks[1].ID != "2" || hooks[1].URL != "https://iterion/wh" || hooks[1].Active {
		t.Errorf("hook[1] = %+v", hooks[1])
	}
}

func TestGitHubDeleteHook_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "t"}
	if err := c.DeleteHook(context.Background(), "o/r", "9"); !errors.Is(err, forge.ErrHookNotFound) {
		t.Errorf("delete 404 = %v", err)
	}
}

func TestGitHubOAuth_AuthorizeAndExchange(t *testing.T) {
	a := &OAuthApp{BaseURL: "https://github.com", ClientID: "cid"}
	u := a.AuthorizeURL("https://it/cb", "st", "ignored-pkce", nil)
	if !strings.Contains(u, "/login/oauth/authorize") || strings.Contains(u, "code_challenge") {
		t.Errorf("authorize url wrong (pkce must be absent): %s", u)
	}
	if !strings.Contains(u, "scope=repo+read%3Aorg") {
		t.Errorf("default scopes missing: %s", u)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("accept = %q", r.Header.Get("Accept"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "gho_x", "scope": "repo,read:org", "token_type": "bearer"})
	}))
	defer srv.Close()
	a2 := &OAuthApp{HTTP: srv.Client(), BaseURL: srv.URL, ClientID: "cid", ClientSecret: "sec"}
	tok, err := a2.Exchange(context.Background(), "code", "https://it/cb", "")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "gho_x" || len(tok.Scopes) != 2 {
		t.Errorf("token = %+v", tok)
	}
	if !tok.ExpiresAt.IsZero() {
		t.Error("classic OAuth tokens should have no expiry")
	}
}

func TestGitHubOAuth_ExchangeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "bad_verification_code"})
	}))
	defer srv.Close()
	a := &OAuthApp{HTTP: srv.Client(), BaseURL: srv.URL, ClientID: "c"}
	if _, err := a.Exchange(context.Background(), "x", "y", ""); err == nil || !strings.Contains(err.Error(), "bad_verification_code") {
		t.Errorf("err = %v", err)
	}
}

func TestOrgMembershipRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/user/memberships/orgs/acme"):
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "active", "role": "admin"})
		case strings.HasSuffix(r.URL.Path, "/user/memberships/orgs/beta"):
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "active", "role": "member"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &AdminClient{HTTP: srv.Client(), APIBase: srv.URL, Token: "tok"}
	ctx := context.Background()

	if role, active, err := c.OrgMembershipRole(ctx, "acme"); err != nil || role != "admin" || !active {
		t.Errorf("acme → %q,%v,%v want admin,true,nil", role, active, err)
	}
	if role, active, err := c.OrgMembershipRole(ctx, "beta"); err != nil || role != "member" || !active {
		t.Errorf("beta → %q,%v,%v want member,true,nil", role, active, err)
	}
	// Not a member → ("", false, nil) — a 404 is not an error here.
	if role, active, err := c.OrgMembershipRole(ctx, "stranger"); err != nil || role != "" || active {
		t.Errorf("stranger → %q,%v,%v want '',false,nil", role, active, err)
	}
}
