package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// tokenAnswer is the create/rotate answer GitLab documents for a project
// access token (docs/api/project_access_tokens): the token rides `token`,
// the expiry is a bare date.
func tokenAnswer(id int64, token string, expires any) map[string]any {
	return map[string]any{
		"id": id, "user_id": 166, "name": "iterion-bot", "token": token,
		"scopes": []string{"api"}, "access_level": 40, "active": true, "revoked": false,
		"created_at": "2026-10-21T10:00:00.000Z", "expires_at": expires,
	}
}

func TestMintProjectBot_RequestShapeAndCredential(t *testing.T) {
	var gotBody map[string]any
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tokenAnswer(58, "glpat-minted-token", "2027-10-21"))
	}))
	defer srv.Close()

	cred, err := New(srv.Client(), srv.URL, "oauth-grant").MintProjectBot(context.Background(), "group/sub/api", forge.ProjectBotSpec{
		Name: forge.ProjectBotName, AccessLevel: forge.ProjectBotAccessLevel, Scopes: forge.ProjectBotScopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v4/projects/group%2Fsub%2Fapi/access_tokens" {
		t.Errorf("path = %q, want the namespaced project id", gotPath)
	}
	if gotAuth != "Bearer oauth-grant" {
		t.Errorf("the mint must ride the connection's grant, got Authorization %q", gotAuth)
	}
	if gotBody["name"] != "iterion-bot" {
		t.Errorf("name = %v", gotBody["name"])
	}
	if gotBody["access_level"] != float64(40) {
		t.Errorf("access_level = %v, want 40 (Maintainer)", gotBody["access_level"])
	}
	if scopes, _ := gotBody["scopes"].([]any); len(scopes) != 1 || scopes[0] != "api" {
		t.Errorf("scopes = %v, want [api]", gotBody["scopes"])
	}
	if _, set := gotBody["expires_at"]; set {
		t.Errorf("expires_at = %v: an omitted expiry must stay omitted so GitLab applies its maximum lifetime", gotBody["expires_at"])
	}
	if cred.TokenID != "58" || cred.Token != "glpat-minted-token" || cred.UserID != "166" {
		t.Errorf("credential = %+v", cred)
	}
	if want := time.Date(2027, 10, 21, 0, 0, 0, 0, time.UTC); !cred.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", cred.ExpiresAt, want)
	}
}

func TestMintProjectBot_ExplicitExpiryTravelsAsDate(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(tokenAnswer(1, "glpat-x", nil))
	}))
	defer srv.Close()
	cred, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{
		Name: "b", AccessLevel: 40, Scopes: []string{"api"}, ExpiresAt: time.Date(2027, 1, 2, 15, 4, 5, 0, time.FixedZone("x", 3600)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["expires_at"] != "2027-01-02" {
		t.Errorf("expires_at = %v, want the UTC date", gotBody["expires_at"])
	}
	if !cred.ExpiresAt.IsZero() {
		t.Errorf("a null expires_at must read as no expiry, got %v", cred.ExpiresAt)
	}
}

func TestMintProjectBot_RefusalsAreTyped(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		detail string
	}{
		{"below maintainer or tokens disabled", 400, `{"message":"400 Bad request - User does not have permission to create project access token"}`, forge.ErrProjectBotNotPermitted, "User does not have permission"},
		{"access level above the caller's", 400, `{"message":"Access level of the token contains permissions not held by the creating user"}`, forge.ErrProjectBotNotPermitted, "permissions not held"},
		{"grant rejected", 401, `{"message":"401 Unauthorized"}`, forge.ErrUnauthorized, ""},
		{"project unknown to the grant", 404, `{"message":"404 Project Not Found"}`, forge.ErrNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.detail != "" && (err == nil || !contains(err.Error(), tc.detail)) {
				t.Errorf("err = %v, must carry GitLab's own reason %q", err, tc.detail)
			}
		})
	}
}

func TestMintProjectBot_AnswerWithoutTokenIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "user_id": 9})
	}))
	defer srv.Close()
	if _, err := New(srv.Client(), srv.URL, "tok").MintProjectBot(context.Background(), "g/p", forge.ProjectBotSpec{Name: "b", AccessLevel: 40, Scopes: []string{"api"}}); err == nil {
		t.Fatal("a 2xx without a token must not yield a credential")
	}
}

func TestRotate_SelfAndAdminPaths(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(tokenAnswer(59, "glpat-rotated", "2027-11-04"))
	}))
	defer srv.Close()
	exp := time.Date(2027, 11, 4, 0, 0, 0, 0, time.UTC)

	cred, err := New(srv.Client(), srv.URL, "bot-token").RotateSelf(context.Background(), "g/p", exp)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v4/projects/g%2Fp/access_tokens/self/rotate" {
		t.Errorf("self rotate path = %q", gotPath)
	}
	if gotAuth != "Bearer bot-token" {
		t.Errorf("self rotation must authenticate AS the bot, got %q", gotAuth)
	}
	if gotBody["expires_at"] != "2027-11-04" {
		t.Errorf("expires_at = %v: without it GitLab issues a one-week token", gotBody["expires_at"])
	}
	if cred.TokenID != "59" || cred.Token != "glpat-rotated" || !cred.ExpiresAt.Equal(exp) {
		t.Errorf("credential = %+v", cred)
	}

	if _, err := New(srv.Client(), srv.URL, "oauth-grant").RotateProjectBot(context.Background(), "g/p", "58", exp); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v4/projects/g%2Fp/access_tokens/58/rotate" {
		t.Errorf("admin rotate path = %q", gotPath)
	}
}

func TestRotate_DeadTokenIsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "expired").RotateSelf(context.Background(), "g/p", time.Time{})
	if !errors.Is(err, forge.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized (the worker falls back to the admin grant on it)", err)
	}
}

func TestRotate_RefusedExpiryKeepsGitLabsReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"Expiration date must be before the maximum allowed lifetime"}`))
	}))
	defer srv.Close()
	_, err := New(srv.Client(), srv.URL, "bot").RotateSelf(context.Background(), "g/p", time.Now().Add(400*24*time.Hour))
	if err == nil || errors.Is(err, forge.ErrProjectBotNotPermitted) {
		t.Fatalf("a rotation 400 is not a mint refusal: %v", err)
	}
	if !contains(err.Error(), "maximum allowed lifetime") {
		t.Errorf("err = %v, must carry GitLab's reason", err)
	}
}

func TestRevoke_AdminAndSelf(t *testing.T) {
	var gotPath, gotMethod string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.EscapedPath(), r.Method
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := New(srv.Client(), srv.URL, "tok")

	status = http.StatusNoContent
	if err := c.RevokeProjectBot(context.Background(), "g/p", "58"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v4/projects/g%2Fp/access_tokens/58" {
		t.Errorf("admin revoke = %s %s", gotMethod, gotPath)
	}
	status = http.StatusNotFound
	if err := c.RevokeProjectBot(context.Background(), "g/p", "58"); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("a token already gone must answer the ErrNotFound class, got %v", err)
	}

	status = http.StatusNoContent
	if err := c.RevokeSelf(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v4/personal_access_tokens/self" {
		t.Errorf("self revoke = %s %s", gotMethod, gotPath)
	}
	status = http.StatusUnauthorized
	if err := c.RevokeSelf(context.Background()); !errors.Is(err, forge.ErrUnauthorized) {
		t.Errorf("a dead token revoking itself answers 401, got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
