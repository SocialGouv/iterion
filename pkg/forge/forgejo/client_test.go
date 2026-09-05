package forgejo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
)

var _ forge.Admin = (*AdminClient)(nil)

func TestForgejoCreateHook_TypeAndConfig(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if !strings.HasSuffix(r.URL.Path, "/api/v1/repos/org/api/hooks") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "active": true, "events": body["events"], "config": body["config"]})
	}))
	defer srv.Close()

	c := New(srv.Client(), srv.URL, "tok")
	h, err := c.CreateHook(context.Background(), "org/api", forge.HookSpec{
		URL: "https://it/api/webhooks/forgejo/wh1", Secret: "iwh_s", Events: []string{"pull_request", "issue_comment"}, Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["type"] != "gitea" {
		t.Errorf("type = %v, want gitea", body["type"])
	}
	config, _ := body["config"].(map[string]any)
	if config["url"] != "https://it/api/webhooks/forgejo/wh1" || config["secret"] != "iwh_s" || config["content_type"] != "json" {
		t.Errorf("config = %v", config)
	}
	if h.ID != "5" {
		t.Errorf("hook id = %q", h.ID)
	}
}

func TestForgejoListHooks_ReturnsAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/v1/repos/o/r/hooks") {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "type": "gitea", "active": true, "events": []string{"push"}, "config": map[string]any{"url": "https://other/hook"}},
			{"id": 2, "type": "gitea", "active": false, "events": []string{"pull_request"}, "config": map[string]any{"url": "https://iterion/wh"}},
		})
	}))
	defer srv.Close()

	hooks, err := New(srv.Client(), srv.URL, "t").ListHooks(context.Background(), "o/r")
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

func TestForgejoDeleteHook_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if err := New(srv.Client(), srv.URL, "t").DeleteHook(context.Background(), "o/r", "9"); !errors.Is(err, forge.ErrHookNotFound) {
		t.Errorf("delete 404 = %v", err)
	}
}

func TestForgejoOAuth_AuthorizeWithPKCEAndRefresh(t *testing.T) {
	a := &OAuthApp{BaseURL: "https://codeberg.org", ClientID: "cid"}
	u := a.AuthorizeURL("https://it/cb", "st", "chal", nil)
	if !strings.Contains(u, "/login/oauth/authorize") || !strings.Contains(u, "code_challenge=chal") {
		t.Errorf("authorize url wrong: %s", u)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at2", "refresh_token": "rt2", "expires_in": 3600})
	}))
	defer srv.Close()
	a2 := &OAuthApp{HTTP: srv.Client(), BaseURL: srv.URL, ClientID: "c", ClientSecret: "s"}
	tok, err := a2.Refresh(context.Background(), forge.Connection{}, "rt1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at2" || tok.RefreshToken != "rt2" || tok.ExpiresAt.IsZero() {
		t.Errorf("refreshed token = %+v", tok)
	}
}

// TestSetAvatar_JSONBase64 pins Gitea/Forgejo's POST /user/avatar shape: a
// JSON body whose `image` is the raw bytes base64-encoded (standard alphabet,
// no data-URL prefix), answered 204 with no URL to report.
func TestSetAvatar_JSONBase64(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 250}
	var gotMethod, gotPath, gotAuth string
	var got struct {
		Image string `json:"image"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	url, err := New(srv.Client(), srv.URL, "tok").SetAvatar(context.Background(), png)
	if err != nil {
		t.Fatal(err)
	}
	if url != "" {
		t.Errorf("forgejo reports no avatar url, got %q", url)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/user/avatar" || gotAuth != "token tok" {
		t.Errorf("request = %s %s auth %q", gotMethod, gotPath, gotAuth)
	}
	dec, err := base64.StdEncoding.DecodeString(got.Image)
	if err != nil || !bytes.Equal(dec, png) {
		t.Errorf("image field = %q (decode err %v), want the png bytes base64-encoded", got.Image, err)
	}
}

func TestSetAvatar_UnsupportedInstance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "tok").SetAvatar(context.Background(), []byte("png"))
	if !errors.Is(err, forge.ErrAvatarUnsupported) {
		t.Errorf("err = %v, want ErrAvatarUnsupported (a 404 is the missing route, not a missing hook)", err)
	}
}
