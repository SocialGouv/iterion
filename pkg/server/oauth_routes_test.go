package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// oauthTestServer builds a local-mode server with a real sealer and an
// in-memory OAuth store, plus a JWT signer so requests carry a genuine
// identity (DisableAuth is deliberately OFF: these endpoints are the BYOK /
// subscription-credential path, and "who is asking" is half their contract).
func oauthTestServer(t *testing.T) (*Server, *httptest.Server, *auth.JWTSigner, *secrets.MemoryOAuthStore) {
	t.Helper()
	sealer, err := secrets.NewAESGCMSealer(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	signer, err := auth.NewJWTSigner(strings.Repeat("a", 43), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	// The routes only mount when the OAuth store, the sealer AND the auth
	// service are all wired (server_routes.go) -- the same gating as BYOK.
	authSvc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	oauthStore := secrets.NewMemoryOAuthStore()
	srv := New(Config{
		OAuthForfait: oauthStore,
		Sealer:       sealer,
		AuthSigner:   signer,
		AuthService:  authSvc,
	}, iterlog.New(iterlog.LevelError, nil))
	hs := httptest.NewServer(srv.handler)
	t.Cleanup(hs.Close)
	return srv, hs, signer, oauthStore
}

func oauthJWT(t *testing.T, signer *auth.JWTSigner, userID string) string {
	t.Helper()
	tok, _, err := signer.IssueAccess(auth.Identity{UserID: userID, Email: userID + "@x"})
	if err != nil {
		t.Fatalf("issue access: %v", err)
	}
	return tok
}

func oauthCall(t *testing.T, hs *httptest.Server, method, path, bearer, body string) (int, string) {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, hs.URL+path, rdr)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	if _, cerr := buf.ReadFrom(resp.Body); cerr != nil {
		t.Fatalf("read body: %v", cerr)
	}
	return resp.StatusCode, buf.String()
}

// TestOAuthConnections_SealsAndNeverEchoesTheCredential locks the /api/me/oauth/*
// surface: the path by which an operator hands a cloud run a subscription
// credential (the BYOK / OAuth-forfait flow of docs/cloud-llm-credentials.md).
//
// Five wired endpoints had NO test at all. The failure that matters here is
// not a 500 — it is a credential leaking back out, or reaching an account that
// is not the one that connected it. Both are asserted against the STORE and
// the response bodies, never against a stub's echo.
func TestOAuthConnections_SealsAndNeverEchoesTheCredential(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")
	bob := oauthJWT(t, signer, "bob")

	// A credentials.json shaped payload carrying a distinctive secret.
	const token = "sk-ant-oat01-THIS-MUST-NEVER-COME-BACK"
	blob := `{"claudeAiOauth":{"accessToken":"` + token + `","refreshToken":"rt-secret-value","expiresAt":` +
		"4102444800000" + `,"scopes":["user:inference"]}}`

	t.Run("anonymous callers are refused", func(t *testing.T) {
		// 401 exactly, not merely "not 200": a 404 would mean the routes
		// never mounted, which would make every case below pass for the
		// wrong reason (measured — it happened while writing this test).
		code, body := oauthCall(t, hs, http.MethodGet, "/api/me/oauth/connections", "", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("anonymous list = %d body=%s, want 401 — these are per-user credentials", code, body)
		}
	})

	t.Run("connecting seals the blob and answers only metadata", func(t *testing.T) {
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, blob)
		if code != http.StatusOK {
			t.Fatalf("upload = %d body=%s", code, body)
		}
		if strings.Contains(body, token) || strings.Contains(body, "rt-secret-value") {
			t.Fatalf("the upload response echoed credential material: %s", body)
		}
		var view oauthConnectionView
		if err := json.Unmarshal([]byte(body), &view); err != nil {
			t.Fatalf("decode view: %v (%s)", err, body)
		}
		if view.Kind != "claude_code" {
			t.Fatalf("view.Kind = %q, want claude_code", view.Kind)
		}

		// The store is the oracle: what landed must be SEALED, i.e. the
		// plaintext token must not appear in the persisted record.
		rec, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		if err != nil {
			t.Fatalf("store Get: %v", err)
		}
		if bytes.Contains(rec.SealedPayload, []byte(token)) {
			t.Fatal("the credential was persisted UNSEALED — the plaintext token is readable in the record")
		}
		if len(rec.SealedPayload) == 0 {
			t.Fatal("nothing was persisted")
		}
	})

	t.Run("listing shows the connection without the secret", func(t *testing.T) {
		code, body := oauthCall(t, hs, http.MethodGet, "/api/me/oauth/connections", alice, "")
		if code != http.StatusOK {
			t.Fatalf("list = %d body=%s", code, body)
		}
		if strings.Contains(body, token) || strings.Contains(body, "rt-secret-value") {
			t.Fatalf("the list response leaked credential material: %s", body)
		}
		if !strings.Contains(body, "claude_code") {
			t.Fatalf("list does not show the connection that was just made: %s", body)
		}
	})

	t.Run("another user neither sees nor can delete the connection", func(t *testing.T) {
		// The owner key is the authenticated user id; a second account must
		// be isolated from the first one's credential.
		code, body := oauthCall(t, hs, http.MethodGet, "/api/me/oauth/connections", bob, "")
		if code != http.StatusOK {
			t.Fatalf("bob list = %d body=%s", code, body)
		}
		if strings.Contains(body, "claude_code") {
			t.Fatalf("bob sees alice's connection: %s", body)
		}

		// Bob deleting "his" claude connection must not touch alice's.
		oauthCall(t, hs, http.MethodDelete, "/api/me/oauth/claude_code", bob, "")
		if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err != nil {
			t.Fatalf("alice's credential disappeared when bob deleted his: %v", err)
		}
	})

	t.Run("an unknown kind is refused", func(t *testing.T) {
		code, _ := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/not-a-provider/credentials", alice, blob)
		if code != http.StatusBadRequest {
			t.Fatalf("unknown kind = %d, want 400", code)
		}
	})

	t.Run("deleting removes it from both the API and the store", func(t *testing.T) {
		if code, body := oauthCall(t, hs, http.MethodDelete, "/api/me/oauth/claude_code", alice, ""); code >= 400 {
			t.Fatalf("delete = %d body=%s", code, body)
		}
		if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err == nil {
			t.Fatal("the record survived its deletion in the store")
		}
		_, body := oauthCall(t, hs, http.MethodGet, "/api/me/oauth/connections", alice, "")
		if strings.Contains(body, "claude_code") {
			t.Fatalf("the deleted connection is still listed: %s", body)
		}
	})
}
