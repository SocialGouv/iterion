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
