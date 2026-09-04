package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
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

// Nothing on an OAuth record says WHOSE account it is: the payload is
// sealed, and the runtime logs print only a fingerprint — so answering
// "whose subscription served that run?" meant grepping server logs and
// correlating hex by hand (measured, 2026-09-03). The label closes that,
// and the fingerprint is exposed beside it because it is the join key
// the logs actually print.
func TestOAuthAccountLabel(t *testing.T) {
	srv, hs, signer, store := oauthTestServer(t)
	jo := oauthJWT(t, signer, "jo")
	blob := func(tok string) string {
		// Include a scope: sealOAuthRecord's ingestion gate refuses a
		// claude_code record with no scope (the shape the CLI reads as
		// "Not logged in").
		return `{"claudeAiOauth":{"accessToken":"` + tok + `","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`
	}
	codexBlob := func(tok, account string) string {
		return `{"tokens":{"access_token":"` + tok + `","refresh_token":"rt","account_id":"` + account + `"},"auth_mode":"chatgpt"}`
	}
	view := func(body string) map[string]any {
		t.Helper()
		var v map[string]any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		return v
	}

	t.Run("connecting names the account, and the view exposes the join key", func(t *testing.T) {
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials?account_label=jothedev", jo, blob("sk-ant-oat01-A"))
		if code != http.StatusOK {
			t.Fatalf("upload = %d body=%s", code, body)
		}
		v := view(body)
		if v["account_label"] != "jothedev" {
			t.Fatalf("account_label = %v, want jothedev", v["account_label"])
		}
		fp, _ := v["fingerprint"].(string)
		if fp == "" {
			t.Fatal("fingerprint must be exposed — it is what the runtime logs print when picking a credential")
		}
		if strings.Contains(body, "sk-ant-oat01-A") {
			t.Fatal("the credential itself must never come back")
		}
	})

	t.Run("an unnamed re-connect of the same credential keeps the name", func(t *testing.T) {
		// Same blob → same fingerprint → provably the same subscription:
		// re-pasting a token is not renaming the account.
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", jo, blob("sk-ant-oat01-A"))
		if code != http.StatusOK {
			t.Fatalf("re-upload = %d body=%s", code, body)
		}
		if v := view(body); v["account_label"] != "jothedev" {
			t.Fatalf("account_label = %v after re-pasting the same blob, want it preserved", v["account_label"])
		}
	})

	t.Run("an unnamed re-connect with a different fingerprint drops the name rather than inherit it", func(t *testing.T) {
		// A claude_code blob carries no account id, so a different blob may
		// be a rotation OR another person's forfait on the same owner key —
		// the swap measured live on 2026-09-03. Inheriting the old name
		// would answer "whose subscription paid?" with the wrong person;
		// an absent name is a visible gap the operator can fill.
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", jo, blob("sk-ant-oat01-B"))
		if code != http.StatusOK {
			t.Fatalf("re-upload = %d body=%s", code, body)
		}
		if v := view(body); v["account_label"] != nil {
			t.Fatalf("account_label = %v after a re-connect with a new fingerprint, want it dropped", v["account_label"])
		}
		rec, err := store.Get(t.Context(), "jo", secrets.OAuthKindClaudeCode)
		if err != nil {
			t.Fatal(err)
		}
		if rec.AccountLabel != "" {
			t.Fatalf("store label = %q, want empty", rec.AccountLabel)
		}
	})

	t.Run("codex: the name follows the account id across token rotations", func(t *testing.T) {
		// Codex fingerprints derive from tokens.account_id, so a fresh
		// auth.json of the SAME ChatGPT account keeps its name, and one
		// from a different account provably is not it.
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials?account_label=jo%40openai", jo, codexBlob("tok-1", "acc-1"))
		if code != http.StatusOK {
			t.Fatalf("connect codex = %d body=%s", code, body)
		}
		code, body = oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials", jo, codexBlob("tok-2", "acc-1"))
		if code != http.StatusOK {
			t.Fatalf("rotate codex = %d body=%s", code, body)
		}
		if v := view(body); v["account_label"] != "jo@openai" {
			t.Fatalf("account_label = %v after rotating tokens of the same account, want it preserved", v["account_label"])
		}
		code, body = oauthCall(t, hs, http.MethodPost, "/api/me/oauth/codex/credentials", jo, codexBlob("tok-3", "acc-2"))
		if code != http.StatusOK {
			t.Fatalf("swap codex = %d body=%s", code, body)
		}
		if v := view(body); v["account_label"] != nil {
			t.Fatalf("account_label = %v after connecting a different account, want it dropped", v["account_label"])
		}
	})

	t.Run("rename backfills a record connected before labels existed, without rewriting it", func(t *testing.T) {
		// The rename must be a metadata write at the store, never a
		// Get → Upsert of the whole record: that would carry the sealed
		// payload this handler read back over whatever a concurrent refresh
		// committed in between — a refresh token the provider may already
		// have rotated out, which no later sweep can heal.
		spy := &oauthUpsertSpy{OAuthStore: store}
		srv.oauthStore = spy
		t.Cleanup(func() { srv.oauthStore = store })
		code, body := oauthCall(t, hs, http.MethodPatch, "/api/me/oauth/claude_code", jo, `{"account_label":"jo perso"}`)
		if code != http.StatusOK {
			t.Fatalf("rename = %d body=%s", code, body)
		}
		if v := view(body); v["account_label"] != "jo perso" {
			t.Fatalf("account_label = %v", v["account_label"])
		}
		if spy.upserts != 0 {
			t.Fatalf("rename issued %d Upsert(s) — it must not rewrite the sealed record", spy.upserts)
		}
		rec, err := store.Get(t.Context(), "jo", secrets.OAuthKindClaudeCode)
		if err != nil {
			t.Fatal(err)
		}
		if rec.AccountLabel != "jo perso" || len(rec.SealedPayload) == 0 || rec.Fingerprint == "" {
			t.Fatalf("after rename: label=%q payload=%d bytes fp=%q", rec.AccountLabel, len(rec.SealedPayload), rec.Fingerprint)
		}
	})

	t.Run("rename with an empty label clears it", func(t *testing.T) {
		code, body := oauthCall(t, hs, http.MethodPatch, "/api/me/oauth/claude_code", jo, `{"account_label":""}`)
		if code != http.StatusOK {
			t.Fatalf("clear = %d body=%s", code, body)
		}
		if v := view(body); v["account_label"] != nil {
			t.Fatalf("account_label = %v after clearing, want absent", v["account_label"])
		}
		if rec, _ := store.Get(t.Context(), "jo", secrets.OAuthKindClaudeCode); rec.AccountLabel != "" {
			t.Fatalf("store label = %q after clearing", rec.AccountLabel)
		}
	})

	t.Run("a label past the cap is refused on every path that accepts one", func(t *testing.T) {
		long := strings.Repeat("é", maxOAuthAccountLabel+1)
		if code, body := oauthCall(t, hs, http.MethodPatch, "/api/me/oauth/claude_code", jo, `{"account_label":"`+long+`"}`); code != http.StatusBadRequest {
			t.Fatalf("rename with a %d-rune label = %d body=%s, want 400", maxOAuthAccountLabel+1, code, body)
		}
		if code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials?account_label="+url.QueryEscape(long), jo, blob("sk-ant-oat01-B")); code != http.StatusBadRequest {
			t.Fatalf("connect with a %d-rune label = %d body=%s, want 400", maxOAuthAccountLabel+1, code, body)
		}
		// Exactly at the cap is fine — the bound is on length, not on
		// bytes, so a French name is not cut shorter than an ASCII one.
		atCap := strings.Repeat("é", maxOAuthAccountLabel)
		if code, body := oauthCall(t, hs, http.MethodPatch, "/api/me/oauth/claude_code", jo, `{"account_label":"`+atCap+`"}`); code != http.StatusOK {
			t.Fatalf("rename with a %d-rune label = %d body=%s, want 200", maxOAuthAccountLabel, code, body)
		}
	})

	t.Run("rename refuses an absent field rather than blanking the name", func(t *testing.T) {
		code, _ := oauthCall(t, hs, http.MethodPatch, "/api/me/oauth/claude_code", jo, `{}`)
		if code != http.StatusBadRequest {
			t.Fatalf("rename with no account_label = %d, want 400 (an omitted field must not clear the label)", code)
		}
	})

	t.Run("rename of a kind that is not connected is 404", func(t *testing.T) {
		nobody := oauthJWT(t, signer, "nobody")
		code, _ := oauthCall(t, hs, http.MethodPatch, "/api/me/oauth/claude_code", nobody, `{"account_label":"x"}`)
		if code != http.StatusNotFound {
			t.Fatalf("rename with no connection = %d, want 404", code)
		}
	})

	t.Run("team scope: a team admin names the forfait, a viewer cannot", func(t *testing.T) {
		seedTeam(t, srv, "t1", "acme")
		ctx := context.Background()
		adm := seedTeamMember(t, srv, ctx, "adm", identity.RoleAdmin)
		vie := seedTeamMember(t, srv, ctx, "vie", identity.RoleViewer)
		admTok, _, err := signer.IssueAccess(adm)
		if err != nil {
			t.Fatal(err)
		}
		vieTok, _, err := signer.IssueAccess(vie)
		if err != nil {
			t.Fatal(err)
		}
		code, body := oauthCall(t, hs, http.MethodPost, "/api/teams/t1/oauth/claude_code/credentials?account_label=team%20forfait", admTok, blob("sk-ant-oat01-T"))
		if code != http.StatusOK {
			t.Fatalf("team connect = %d body=%s", code, body)
		}
		if code, _ := oauthCall(t, hs, http.MethodPatch, "/api/teams/t1/oauth/claude_code", vieTok, `{"account_label":"mine now"}`); code != http.StatusForbidden {
			t.Fatalf("viewer rename = %d, want 403", code)
		}
		code, body = oauthCall(t, hs, http.MethodPatch, "/api/teams/t1/oauth/claude_code", admTok, `{"account_label":"SocialGouv Revi"}`)
		if code != http.StatusOK {
			t.Fatalf("admin rename = %d body=%s", code, body)
		}
		code, body = oauthCall(t, hs, http.MethodGet, "/api/teams/t1/oauth/connections", vieTok, "")
		if code != http.StatusOK || !strings.Contains(body, `"account_label":"SocialGouv Revi"`) || !strings.Contains(body, `"fingerprint":"`) {
			t.Fatalf("team listing = %d body=%s, want the new name beside the fingerprint", code, body)
		}
	})
}

// oauthUpsertSpy counts whole-record writes, so a test can prove a code path
// that promises "metadata only" never rewrites the sealed payload.
type oauthUpsertSpy struct {
	secrets.OAuthStore
	upserts int
}

func (s *oauthUpsertSpy) Upsert(ctx context.Context, rec secrets.OAuthRecord) error {
	s.upserts++
	return s.OAuthStore.Upsert(ctx, rec)
}

// TestOAuthCredentialIngestionValidatesShape locks the paid failures behind
// the /api/me/oauth/{kind}/credentials paste path (#627):
//   - a claude_code accessToken with an embedded newline (terminal-transcript
//     paste, the shape the prod incident reported): every downstream call
//     would die on `Bearer <transcript>` — refused at ingestion.
//   - a claude_code record with no scopes and/or no expiresAt: the shape the
//     CLI reads as "Not logged in" — refused at ingestion so the operator
//     sees it at paste time, not on a fleet of dead-on-arrival runs.
//   - a claude_code record with an ALREADY-EXPIRED accessToken.
//   - a healthy full-shape blob still passes and lands sealed.
//
// The oracle is the STORE (no record must have been written), not just the
// response status — a 400 with a silent write would still leave the garbage
// on disk for the runs to pick up.
func TestOAuthCredentialIngestionValidatesShape(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")

	claudeBlob := func(tok string, expiresAtMs int64, scopes string) string {
		exp := ""
		if expiresAtMs > 0 {
			exp = fmt.Sprintf(`,"expiresAt":%d`, expiresAtMs)
		}
		sc := ""
		if scopes != "" {
			sc = `,"scopes":` + scopes
		}
		return `{"claudeAiOauth":{"accessToken":"` + tok + `","refreshToken":"rt"` + exp + sc + `}}`
	}

	for _, tc := range []struct {
		name, blob, wantInErr string
	}{
		{
			name:      "accessToken carrying an embedded newline",
			blob:      `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-good\nWelcome to claude","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`,
			wantInErr: "newline",
		},
		{
			name:      "accessToken carrying a tab",
			blob:      `{"claudeAiOauth":{"accessToken":"\tsk-ant-oat01-good","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`,
			wantInErr: "tab",
		},
		{
			name:      "accessToken carrying a space",
			blob:      `{"claudeAiOauth":{"accessToken":"sk-ant-oat01 good","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`,
			wantInErr: "space",
		},
		{
			name:      "accessToken carrying an ANSI escape (terminal transcript)",
			blob:      `{"claudeAiOauth":{"accessToken":"\u001b[32msk-ant-oat01-good\u001b[0m","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`,
			wantInErr: "control character",
		},
		{
			name:      "claude_code with no scopes",
			blob:      claudeBlob("sk-ant-oat01-good", 4102444800000, ""),
			wantInErr: "scopes",
		},
		{
			name:      "claude_code with no expiresAt",
			blob:      claudeBlob("sk-ant-oat01-good", 0, `["user:inference"]`),
			wantInErr: "expiresAt",
		},
		{
			// With a refreshToken an expired record is accepted (the refresh
			// worker renews it); only the unrenewable shape is refused.
			name:      "claude_code with an already-expired accessToken and no refreshToken",
			blob:      fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-good","expiresAt":%d,"scopes":["user:inference"]}}`, time.Now().Add(-time.Hour).UnixMilli()),
			wantInErr: "expired",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Wipe first so a residue from an earlier subtest cannot mask a
			// silent write here.
			_ = oauthStore.Delete(t.Context(), "alice", secrets.OAuthKindClaudeCode)

			code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, tc.blob)
			if code != http.StatusBadRequest {
				t.Fatalf("upload = %d body=%s, want 400 (garbage credential rejected at ingestion)", code, body)
			}
			if !strings.Contains(body, tc.wantInErr) {
				t.Fatalf("error body = %q, want it to mention %q", body, tc.wantInErr)
			}
			// The store is the oracle: nothing must have been persisted.
			if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err == nil {
				t.Fatal("a rejected credential was silently stored — every downstream call would now die on a Bearer transcript")
			}
		})
	}

	t.Run("healthy full-shape blob still passes", func(t *testing.T) {
		_ = oauthStore.Delete(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-good","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, blob)
		if code != http.StatusOK {
			t.Fatalf("upload = %d body=%s, want 200 for a well-shaped credential", code, body)
		}
		rec, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		if err != nil {
			t.Fatalf("store Get: %v", err)
		}
		if len(rec.SealedPayload) == 0 || len(rec.Scopes) == 0 || rec.AccessTokenExpiresAt == nil {
			t.Fatalf("healthy blob landed with wrong shape: sealed=%d scopes=%v expiresAt=%v", len(rec.SealedPayload), rec.Scopes, rec.AccessTokenExpiresAt)
		}
	})
}

// An expired access token is only dead when nothing can renew it. The
// refresh worker heals exactly the expired-but-refreshable record
// (ExpiringBefore lists expired ones; RunOnce refreshes every refreshable
// one), and every producer funnels through sealOAuthRecord — so refusing
// that shape at paste time left a stale export from a LOGGED-IN machine
// unconnectable, with `claude login` as the wrong remedy (#627 round 1).
func TestOAuthCredentialIngestion_ExpiredIsRefusedOnlyWithoutARefreshToken(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")
	expired := time.Now().Add(-time.Hour).UnixMilli()

	t.Run("expired + refreshToken → accepted, refreshable", func(t *testing.T) {
		_ = oauthStore.Delete(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		blob := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-stale","refreshToken":"rt","expiresAt":%d,"scopes":["user:inference"]}}`, expired)
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, blob)
		if code != http.StatusOK {
			t.Fatalf("upload = %d body=%s, want 200 (the refresh worker renews it)", code, body)
		}
		var view oauthConnectionView
		if err := json.Unmarshal([]byte(body), &view); err != nil {
			t.Fatalf("decode view: %v", err)
		}
		if !view.Refreshable {
			t.Fatalf("view.refreshable = false, want true: %s", body)
		}
		rec, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		if err != nil {
			t.Fatalf("stored record: %v", err)
		}
		if rec.NotRefreshable || rec.AccessTokenExpiresAt == nil || rec.AccessTokenExpiresAt.After(time.Now()) {
			t.Fatalf("stored record = %+v, want refreshable with its past expiry kept (ExpiringBefore must list it)", rec)
		}
	})

	t.Run("expired + no refreshToken → refused, remedy names the refreshToken", func(t *testing.T) {
		_ = oauthStore.Delete(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		blob := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-stale","expiresAt":%d,"scopes":["user:inference"]}}`, expired)
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, blob)
		if code != http.StatusBadRequest {
			t.Fatalf("upload = %d body=%s, want 400", code, body)
		}
		if !strings.Contains(body, "expired") || !strings.Contains(body, "refreshToken") || strings.Contains(body, "claude login") {
			t.Fatalf("remedy must say the record cannot be renewed and what to paste instead, never `claude login`: %s", body)
		}
		if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err == nil {
			t.Fatal("a refused credential was silently stored")
		}
	})

	t.Run("missing expiresAt → refused", func(t *testing.T) {
		_ = oauthStore.Delete(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-good","refreshToken":"rt","scopes":["user:inference"]}}`
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, blob)
		if code != http.StatusBadRequest || !strings.Contains(body, "expiresAt") {
			t.Fatalf("upload = %d body=%s, want 400 naming expiresAt", code, body)
		}
	})
}

// The ASCII-only gate let a NO-BREAK SPACE glued to the token through the
// chokepoint and SEALED it. End to end through the paste path: refused,
// naming the rune, nothing stored.
func TestOAuthCredentialIngestion_RefusesUnicodeWhitespaceEndToEnd(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-good\u00a0tail","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`
	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, blob)
	if code != http.StatusBadRequest || !strings.Contains(body, "U+00A0") {
		t.Fatalf("upload = %d body=%s, want 400 naming U+00A0", code, body)
	}
	if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err == nil {
		t.Fatal("an NBSP-glued token was sealed through the chokepoint")
	}
}

// cannedTokenTransport answers the Anthropic token exchange in-process.
type cannedTokenTransport struct {
	status int
	body   string
	seen   *http.Request
}

func (c *cannedTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.seen = r
	return &http.Response{
		StatusCode: c.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(c.body)),
		Request:    r,
	}, nil
}

// seedPending puts a browser-flow pending authorization for owner+kind
// and returns its state.
func seedPending(t *testing.T, srv *Server, ownerKey string) string {
	t.Helper()
	sealedVerifier, err := secrets.SealOAuthVerifier(srv.sealer, ownerKey, secrets.OAuthKindClaudeCode, "verifier-x")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := srv.oauthPending.Put(t.Context(), secrets.OAuthPending{
		OwnerKey: ownerKey, Kind: secrets.OAuthKindClaudeCode, SealedVerifier: sealedVerifier,
		State: "state-x", RedirectURI: "https://example.invalid/cb", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	return "state-x"
}

// The browser flow builds the blob itself: BuildAnthropicCredentials
// omits expiresAt when the exchange returns no expires_in and scopes when
// it returns no scope (the repo's refresh tests model scope-less
// responses). The paste-path presence rules do not apply to it — and
// with the pending authorization already consumed, a refusal costs the
// operator a restarted connect, so the server-built blob gets the
// token-shape check only. A shape refusal there is a 400, not a 500.
func TestOAuthBrowserCompletion_ServerBuiltBlobIsNotHeldToPasteRules(t *testing.T) {
	srv, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")
	srv.oauthPending = secrets.NewMemoryOAuthPendingStore()
	srv.cfg.AnthropicOAuthClientID = "client-x"

	t.Run("scope-less, expiry-less exchange → stored", func(t *testing.T) {
		_ = oauthStore.Delete(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		state := seedPending(t, srv, "alice")
		rt := &cannedTokenTransport{status: 200, body: `{"access_token":"sk-ant-oat01-browser-token","refresh_token":"rt-browser","token_type":"Bearer"}`}
		srv.httpClient = &http.Client{Transport: rt}
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/authorize/complete", alice, `{"code":"code-x","state":"`+state+`"}`)
		if code != http.StatusOK {
			t.Fatalf("complete = %d body=%s, want 200", code, body)
		}
		if rt.seen == nil {
			t.Fatal("the exchange never reached the token endpoint")
		}
		rec, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		if err != nil {
			t.Fatalf("stored record: %v", err)
		}
		if rec.NotRefreshable || rec.AccessTokenExpiresAt != nil || len(rec.Scopes) != 0 {
			t.Fatalf("stored record = %+v, want refreshable with no expiry and no scopes (as the exchange answered)", rec)
		}
	})

	t.Run("exchange answers a malformed token → 400, nothing stored", func(t *testing.T) {
		_ = oauthStore.Delete(t.Context(), "alice", secrets.OAuthKindClaudeCode)
		state := seedPending(t, srv, "alice")
		srv.httpClient = &http.Client{Transport: &cannedTokenTransport{status: 200, body: `{"access_token":"sk-ant-oat01-browser\ntoken","refresh_token":"rt"}`}}
		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/authorize/complete", alice, `{"code":"code-x","state":"`+state+`"}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "newline") {
			t.Fatalf("complete = %d body=%s, want 400 naming the newline", code, body)
		}
		if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err == nil {
			t.Fatal("a refused credential was stored")
		}
	})
}

// A refusal leaves a trace: a Warn and, on an audited owner (a team's
// org credential), an audit event naming the field and the reason —
// never the value. Before this only the success path wrote anything.
func TestOAuthCredentialIngestion_RefusalLeavesATrace(t *testing.T) {
	srv, hs, signer, oauthStore := oauthTestServer(t)
	var logs bytes.Buffer
	srv.logger = iterlog.New(iterlog.LevelWarn, &logs)
	auditStore := audit.NewMemoryStore()
	srv.auditStore = auditStore
	tok, _, err := signer.IssueAccess(auth.Identity{UserID: "root", Email: "root@x", IsSuperAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	blob := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-good\nWelcome","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`
	code, body := oauthCall(t, hs, http.MethodPost, "/api/teams/team-x/oauth/claude_code/credentials", tok, blob)
	if code != http.StatusBadRequest {
		t.Fatalf("upload = %d body=%s, want 400", code, body)
	}
	if _, err := oauthStore.Get(t.Context(), secrets.OrgOwnerKey("team-x"), secrets.OAuthKindClaudeCode); err == nil {
		t.Fatal("a refused credential was stored")
	}
	if l := logs.String(); !strings.Contains(l, "REFUSED at ingestion") || !strings.Contains(l, "claudeAiOauth.accessToken") || strings.Contains(l, "sk-ant-oat01-good") {
		t.Fatalf("want a Warn naming the field and never the value; got:\n%s", l)
	}
	// The audit insert is detached; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		events, err := auditStore.ListByTenant(t.Context(), "team-x", audit.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.Action == "oauth.org.refused" {
				if e.Meta["field"] != "claudeAiOauth.accessToken" || e.Meta["reason"] == nil || strings.Contains(fmt.Sprint(e.Meta), "sk-ant-oat01-good") {
					t.Fatalf("audit event = %+v, want field + reason and never the value", e)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no oauth.org.refused audit event; got %+v", events)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A whole terminal transcript pasted into the box is the shape #627 was
// filed on. It fails to PARSE, which is not a ShapeError — so the refusal
// branch let it through with no Warn and no audit entry, exactly the
// invisible failure the ticket exists to end. The 400 alone is not enough:
// the operator who pasted it is not the one who reads the run's 401s hours
// later.
func TestOAuthCredentialIngestion_TranscriptPasteLeavesATrace(t *testing.T) {
	srv, hs, signer, oauthStore := oauthTestServer(t)
	var buf bytes.Buffer
	srv.logger = iterlog.New(iterlog.LevelInfo, &buf)
	alice := oauthJWT(t, signer, "alice")

	blob := "\x1b[32mWelcome to Claude Code\x1b[0m\n" +
		`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-secret","refreshToken":"rt","expiresAt":4102444800000,"scopes":["user:inference"]}}`

	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, blob)
	if code != http.StatusBadRequest {
		t.Fatalf("upload = %d body=%s, want 400 for a transcript paste", code, body)
	}
	if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err == nil {
		t.Fatal("a transcript paste was stored")
	}
	log := buf.String()
	if !strings.Contains(log, "REFUSED at ingestion") || !strings.Contains(log, "credentials.json") {
		t.Fatalf("a refused paste must leave a Warn naming the file it could not read; got:\n%s", log)
	}
	// The trace names the shape, never the material.
	if strings.Contains(log, "sk-ant-oat01-secret") || strings.Contains(body, "sk-ant-oat01-secret") {
		t.Fatalf("the refusal echoed the token; log:\n%s\nbody: %s", log, body)
	}
}
