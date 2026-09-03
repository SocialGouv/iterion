package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
)

func testKeyPEM(t *testing.T) (string, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(pemBytes), &key.PublicKey
}

func TestSignAppJWT(t *testing.T) {
	pemStr, pub := testKeyPEM(t)
	now := time.Unix(1700000000, 0).UTC()
	tok, err := signAppJWT(42, pemStr, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt should have 3 parts, got %d", len(parts))
	}
	// verify the RS256 signature.
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
	// claims carry iss = app id.
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(cb, &claims)
	if claims["iss"] != "42" {
		t.Errorf("iss = %v, want 42", claims["iss"])
	}
}

func TestMintInstallationToken(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/app/installations/99/access_tokens") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Errorf("expected a JWT bearer, got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_inst", "expires_at": "2099-01-01T00:00:00Z"})
	}))
	defer srv.Close()
	cfg := AppConfig{AppID: 42, PrivateKeyPEM: pemStr, AppSlug: "iterion"}
	tok, exp, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_inst" || exp.Year() != 2099 {
		t.Errorf("token=%q exp=%v", tok, exp)
	}
}

// A least-privilege mint sends a body scoping the token to the given repos +
// permission subset (the whole-installation default sends no body).
func TestMintInstallationToken_NarrowsScope(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_scoped", "expires_at": "2099-01-01T00:00:00Z"})
	}))
	defer srv.Close()
	cfg := AppConfig{AppID: 42, PrivateKeyPEM: pemStr}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0),
		&InstallationTokenOptions{Repositories: []string{"api"}, Permissions: RuntimeInstallationPermissions()})
	if err != nil {
		t.Fatal(err)
	}
	repos, _ := gotBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "api" {
		t.Errorf("repositories body = %v, want [api]", gotBody["repositories"])
	}
	perms, _ := gotBody["permissions"].(map[string]any)
	if perms["repository_hooks"] != "write" || perms["administration"] != nil {
		t.Errorf("permissions body = %v, want minimal set without administration", perms)
	}
}

// A 422 (the observed prod failure) must surface GitHub's own explanation, not
// a bare "HTTP 422" — the mint call scopes to specific repos/permissions and
// GitHub says exactly which one is wrong.
func TestMintInstallationToken_422SurfacesGitHubMessage(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "There is at least one repository that does not exist or is not accessible to the owner.",
			"errors":  []map[string]any{{"code": "invalid", "field": "repositories"}},
		})
	}))
	defer srv.Close()
	cfg := AppConfig{AppID: 42, PrivateKeyPEM: pemStr}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0),
		&InstallationTokenOptions{Repositories: []string{"gone"}, Permissions: RuntimeInstallationPermissions()})
	if err == nil {
		t.Fatal("expected error on 422")
	}
	if !strings.Contains(err.Error(), "does not exist or is not accessible") {
		t.Errorf("error must surface GitHub's message, got: %v", err)
	}
}

// A 422 whose body reports the requested permissions are NOT GRANTED is a
// PERMANENT config mismatch (the install was approved with a narrower
// permission set than iterion now requests). It must classify as the terminal
// forge.ErrPermissionsNotGranted — so the refresh worker marks the connection
// degraded instead of re-minting it every tick — while still surfacing GitHub's
// actionable message.
func TestMintInstallationToken_422PermissionsNotGranted(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "The permissions requested are not granted to this installation.",
		})
	}))
	defer srv.Close()
	cfg := AppConfig{AppID: 42, PrivateKeyPEM: pemStr}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0),
		&InstallationTokenOptions{Permissions: RuntimeInstallationPermissions()})
	if err == nil {
		t.Fatal("expected error on 422")
	}
	if !errors.Is(err, forge.ErrPermissionsNotGranted) {
		t.Errorf("422 permissions-not-granted → %v, want ErrPermissionsNotGranted", err)
	}
	if errors.Is(err, forge.ErrUnauthorized) {
		t.Errorf("must NOT classify as ErrUnauthorized (that path markRevokes): %v", err)
	}
	// GitHub's actionable message is preserved in the wrapped error.
	if !strings.Contains(err.Error(), "not granted to this installation") {
		t.Errorf("error must surface GitHub's message, got: %v", err)
	}
}

// A 422 for a DIFFERENT reason (repository not accessible) must NOT be
// misclassified as the terminal permission mismatch — it stays a plain error.
func TestMintInstallationToken_422OtherStaysPlain(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "There is at least one repository that does not exist or is not accessible to the owner.",
		})
	}))
	defer srv.Close()
	cfg := AppConfig{AppID: 42, PrivateKeyPEM: pemStr}
	_, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, cfg, 99, time.Unix(1700000000, 0),
		&InstallationTokenOptions{Repositories: []string{"gone"}})
	if err == nil {
		t.Fatal("expected error on 422")
	}
	if errors.Is(err, forge.ErrPermissionsNotGranted) {
		t.Errorf("a non-permission 422 must stay a plain error, got ErrPermissionsNotGranted: %v", err)
	}
}

func TestAppClient_CreateHookUsesInstallationToken(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	var hookAuth string
	mints := 0
	mux := http.NewServeMux()
	// APIBaseFor maps a non-github.com web base to <base>/api/v3 — register
	// the mux under that prefix so AppClient's own calls match.
	mux.HandleFunc("/api/v3/app/installations/99/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mints++
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_inst", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("/api/v3/repos/octo/api/hooks", func(w http.ResponseWriter, r *http.Request) {
		hookAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "active": true})
	})
	mux.HandleFunc("/api/v3/installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []map[string]any{
			{"full_name": "octo/api", "private": true},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	app := &AppClient{
		HTTP: srv.Client(), WebBaseURL: srv.URL,
		Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr, AppSlug: "iterion"}, InstallationID: 99,
	}

	id, _ := app.WhoAmI(context.Background())
	if id.Login != "iterion[bot]" || id.Kind != "bot" {
		t.Errorf("app identity = %+v", id)
	}

	repos, err := app.ListRepos(context.Background(), forge.RepoQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].FullName != "octo/api" || !repos[0].CanAdmin {
		t.Errorf("installation repos = %+v", repos)
	}

	if _, err := app.CreateHook(context.Background(), "octo/api", forge.HookSpec{URL: "u", Secret: "s", Events: []string{"pull_request"}}); err != nil {
		t.Fatal(err)
	}
	if hookAuth != "Bearer ghs_inst" {
		t.Errorf("hook used auth %q, want the installation token", hookAuth)
	}
	// the token is cached — list + create reused one mint.
	if mints != 1 {
		t.Errorf("expected 1 token mint (cached), got %d", mints)
	}
}

// AppClient.rest requests the OPTIONAL statuses:write (the merge-gate commit
// status) on top of the core baseline, and — when the installation did not
// grant it (422 permissions-not-granted) — falls back to the core set so every
// other capability keeps working (the gate then advises, non-fatal). This is
// the multi-tenant safety guarantee: a third-party install without statuses is
// never broken by the gate feature.
func TestAppClientRest_StatusesFallback(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	var reqPerms []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		perms, _ := body["permissions"].(map[string]any)
		reqPerms = append(reqPerms, perms)
		if _, wantsStatuses := perms["statuses"]; wantsStatuses {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "The permissions requested are not granted to this installation."})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_core", "expires_at": "2099-01-01T00:00:00Z"})
	}))
	defer srv.Close()
	a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}, InstallationID: 99, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	rc, err := a.rest(context.Background())
	if err != nil {
		t.Fatalf("rest fell over instead of falling back: %v", err)
	}
	if rc == nil || rc.Token != "ghs_core" {
		t.Fatalf("expected the core-set token after fallback, got %+v", rc)
	}
	if len(reqPerms) != 2 {
		t.Fatalf("expected 2 mint attempts (statuses, then core), got %d", len(reqPerms))
	}
	if _, ok := reqPerms[0]["statuses"]; !ok {
		t.Errorf("first attempt must request statuses:write, got %v", reqPerms[0])
	}
	if _, ok := reqPerms[1]["statuses"]; ok {
		t.Errorf("fallback attempt must DROP statuses, got %v", reqPerms[1])
	}
	if reqPerms[1]["pull_requests"] != "write" {
		t.Errorf("fallback must keep the core baseline, got %v", reqPerms[1])
	}
}

// When the installation DOES grant statuses:write, the first mint succeeds and
// the token carries it — the gate can post for real (no fallback, one request).
func TestAppClientRest_StatusesGranted(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_full", "expires_at": "2099-01-01T00:00:00Z"})
	}))
	defer srv.Close()
	a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}, InstallationID: 99, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	rc, err := a.rest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rc.Token != "ghs_full" || attempts != 1 {
		t.Fatalf("granted statuses → one successful mint, got token=%q attempts=%d", rc.Token, attempts)
	}
}

// The App client must be able to COMMENT, not only to set a status: the
// merge gate's parked-review pause notice resolves its client by type
// assertion off forgeAdminFor, which on the production GitHub path returns
// *AppClient. Without this forwarder the assertion failed and the notice was
// silently dropped. Round-trip: mint (issues:write is already in the
// baseline) → POST /repos/{repo}/issues/{n}/comments with the minted token.
func TestAppClientCommentIssue_RoundTrip(t *testing.T) {
	pemStr, _ := testKeyPEM(t)
	var (
		body    map[string]any
		gotAuth string
		gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			perms, _ := in["permissions"].(map[string]any)
			if perms["issues"] != "write" {
				t.Errorf("the minted token must carry issues:write (the comments endpoint), got %v", perms)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_app", "expires_at": "2099-01-01T00:00:00Z"})
			return
		}
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":       321,
			"html_url": "https://github.com/o/r/issues/7#issuecomment-321",
			"body":     body["body"],
			"user":     map[string]any{"login": "iterion[bot]"},
		})
	}))
	defer srv.Close()

	a := &AppClient{HTTP: srv.Client(), WebBaseURL: srv.URL, Cfg: AppConfig{AppID: 42, PrivateKeyPEM: pemStr}, InstallationID: 99, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	got, err := a.CommentIssue(context.Background(), "o/r", 7, "⏸️ Review paused")
	if err != nil {
		t.Fatalf("the App path must be able to comment: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/repos/o/r/issues/7/comments") {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer ghs_app" {
		t.Errorf("the comment must ride the minted installation token, got %q", gotAuth)
	}
	if body["body"] != "⏸️ Review paused" {
		t.Errorf("request body = %v", body)
	}
	if got.ID != "321" || got.Author != "iterion[bot]" {
		t.Errorf("comment = %+v", got)
	}
}
