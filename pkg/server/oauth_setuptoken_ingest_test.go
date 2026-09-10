package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// What an operator holds when provisioning a team or the platform tier is
// almost never a credentials.json — nobody logs a shared account into a
// local CLI just to export its file. It is the bare `sk-ant-oat…` that
// `claude setup-token` prints. Refusing it did not remove the wrapping; it
// moved it into every operator's shell, where the expiry convention had to
// be re-guessed each time and a wrong guess is stored happily and never
// serves a run.
//
// The oracle is the STORE and the SEALED payload, not the response code: a
// 200 over a payload a runner cannot parse would be the same failure one
// layer down.
func TestOAuthCredentialIngestion_AcceptsABareSetupToken(t *testing.T) {
	srv, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")
	const token = "sk-ant-oat01-" + "AAAABBBBCCCCDDDDEEEEFFFF"

	before := time.Now().UTC()
	code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, token)
	if code != http.StatusOK {
		t.Fatalf("upload of a bare setup token = %d body=%s, want 200", code, body)
	}

	rec, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	// The two fields whose absence the CLI reads as "Not logged in".
	if len(rec.Scopes) == 0 {
		t.Fatal("stored record carries no scope — the CLI would read it as 'Not logged in'")
	}
	if rec.AccessTokenExpiresAt == nil {
		t.Fatal("stored record carries no expiry — the CLI would read it as 'Not logged in'")
	}
	if !rec.NotRefreshable {
		t.Fatal("a setup token cannot be renewed; the record must say so or the refresh worker will try")
	}
	if got := rec.AccessTokenExpiresAt.Sub(before); got < secrets.SetupTokenAssumedLifetime-time.Minute {
		t.Fatalf("expiry is %s away, want ≈%s", got, secrets.SetupTokenAssumedLifetime)
	}

	// A runner receives the sealed payload and parses it as credentials.json.
	payload, err := secrets.OpenOAuthPayload(srv.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	v, err := secrets.ParseAnthropicView(payload)
	if err != nil {
		t.Fatalf("the sealed payload is not a credentials.json a run can use: %v", err)
	}
	if v.ClaudeAIOauth.AccessToken != token {
		t.Fatalf("sealed accessToken = %q, want the token that was uploaded", v.ClaudeAIOauth.AccessToken)
	}
}

// Re-uploading the SAME setup token must land on the same fingerprint: the
// fingerprint is the subscription's audit identity and the usage meter's
// key, and it is what decides whether an unnamed re-connect keeps its
// account label. Wrapping stamps a clock-derived expiry into the payload,
// so this only holds because the identity is taken over the token.
func TestOAuthCredentialIngestion_SetupTokenKeepsItsIdentityAcrossUploads(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")
	const token = "sk-ant-oat01-" + "AAAABBBBCCCCDDDDEEEEFFFF"

	code, body := oauthCall(t, hs, http.MethodPost,
		"/api/me/oauth/claude_code/credentials?account_label=devthejo@proton.me", alice, token)
	if code != http.StatusOK {
		t.Fatalf("first upload = %d body=%s", code, body)
	}
	first, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}

	// Same token, read from a file this time (trailing newline), and with no
	// account_label — the shape a routine re-provisioning takes.
	code, body = oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, token+"\n")
	if code != http.StatusOK {
		t.Fatalf("second upload = %d body=%s", code, body)
	}
	second, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode)
	if err != nil {
		t.Fatalf("store Get: %v", err)
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatalf("fingerprint moved on a re-upload of the same token: %s → %s — a fresh usage meter, and the account name dropped",
			first.Fingerprint, second.Fingerprint)
	}
	// Equality between the two uploads is necessary but not sufficient: both
	// happen in the same millisecond, so a fingerprint taken over the WRAPPER
	// would match here too. Pin what it is actually taken over.
	if want := secrets.SubscriptionFingerprint(secrets.OAuthKindClaudeCode, []byte(token)); first.Fingerprint != want {
		t.Fatalf("fingerprint = %s, want %s (the token's) — taken over the wrapper, it would move with the clock on the next upload",
			first.Fingerprint, want)
	}
	if second.AccountLabel != "devthejo@proton.me" {
		t.Fatalf("account label = %q, want it inherited — the re-connect provably names the same subscription", second.AccountLabel)
	}
}

// The new path must not swallow the refusals the old one earned. A pasted
// terminal transcript is not a token, and it must still be refused with the
// message that says so.
func TestOAuthCredentialIngestion_SetupTokenPathKeepsTheTypedRefusals(t *testing.T) {
	_, hs, signer, oauthStore := oauthTestServer(t)
	alice := oauthJWT(t, signer, "alice")

	for _, tc := range []struct{ name, blob, wantInErr string }{
		{"a transcript that happens to contain a token", "Welcome to Claude Code\n> /login\nsk-ant-oat01-x", "is not a JSON object"},
		{"a token with an inner space", "sk-ant-oat01-aaa bbb", "space"},
		{"an API key, not an OAuth token", "sk-ant-api03-whatever", "is not a JSON object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = oauthStore.Delete(t.Context(), secrets.OAuthRecordID("alice", secrets.OAuthKindClaudeCode, 0))
			code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/credentials", alice, tc.blob)
			if code != http.StatusBadRequest {
				t.Fatalf("upload = %d body=%s, want 400", code, body)
			}
			if !strings.Contains(body, tc.wantInErr) {
				t.Fatalf("error body = %q, want it to mention %q", body, tc.wantInErr)
			}
			if _, err := oauthStore.Get(t.Context(), "alice", secrets.OAuthKindClaudeCode); err == nil {
				t.Fatal("a refused credential was stored anyway")
			}
		})
	}
}
