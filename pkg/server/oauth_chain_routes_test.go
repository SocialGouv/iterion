package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// chainBlob builds a claude_code credentials.json for a chain test. Without a
// refreshToken the record is NOT refreshable, which RefreshRecord answers
// before it reaches the provider — so these cases make no network call.
func chainBlob(token, refreshToken string) string {
	rt := ""
	if refreshToken != "" {
		rt = `"refreshToken":"` + refreshToken + `",`
	}
	return `{"claudeAiOauth":{"accessToken":"` + token + `",` + rt +
		`"expiresAt":4102444800000,"scopes":["user:inference"]}}`
}

// chainRecord returns the caller's claude_code record at `rank`, read from the
// store rather than from a response: the store is the oracle for everything
// these cases assert.
func chainRecord(t *testing.T, st *secrets.MemoryOAuthStore, user string, rank int) secrets.OAuthRecord {
	t.Helper()
	rec, err := chainRecordOrErr(st, user, rank)
	if err != nil {
		t.Fatalf("claude_code record at rank %d for %s: %v", rank, user, err)
	}
	return rec
}

// chainRecordOrErr is the same lookup for a case that asserts on ABSENCE —
// where a t.Fatalf inside the helper would report "missing" as a failure
// rather than as the answer.
func chainRecordOrErr(st *secrets.MemoryOAuthStore, user string, rank int) (secrets.OAuthRecord, error) {
	recs, err := st.ListByUser(context.Background(), user)
	if err != nil {
		return secrets.OAuthRecord{}, err
	}
	for _, r := range recs {
		if r.Kind == secrets.OAuthKindClaudeCode && r.Rank == rank {
			return r, nil
		}
	}
	return secrets.OAuthRecord{}, secrets.ErrOAuthNotFound
}

// failingDeleteOAuthStore makes exactly one operation fail — the store write
// the pool withdrawal must not outrun.
type failingDeleteOAuthStore struct{ secrets.OAuthStore }

func (failingDeleteOAuthStore) Delete(context.Context, string) error {
	return errors.New("store unavailable")
}

// TestOAuthChainRoutes_AWriteAnswersAboutTheLinkItAddressed is the class sweep
// for `?rank=`: once an owner may hold a CHAIN of forfaits of one kind, every
// handler that selects a record must select the one the request names. A
// handler that still reads (owner, kind) lands on the PRIMARY, and each case
// below is one such site — none of which fails loudly, which is exactly why
// they are pinned here:
//
//   - rename writes the right link and then reports the primary's identity,
//     so the audit line names the wrong credential;
//   - refresh renews the primary whatever rank is asked for — and the provider
//     RETIRES the refresh token it is handed, so trying to renew a dying
//     fallback spends the primary's instead and leaves the fallback dying;
//   - the 409 explaining a refused refresh describes the primary's state;
//   - a re-upload that names no account inherits a label by fingerprint, and
//     comparing against the primary's means a rotated fallback silently loses
//     the name it had.
func TestOAuthChainRoutes_AWriteAnswersAboutTheLinkItAddressed(t *testing.T) {
	t.Run("rename reports the link it renamed, not the primary", func(t *testing.T) {
		_, hs, signer, st := oauthTestServer(t)
		tok := oauthJWT(t, signer, "alice")

		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials?account_label=primary",
			tok, chainBlob("sk-ant-oat01-PRIMARY", "")); code != http.StatusOK {
			t.Fatalf("upload primary = %d %s", code, body)
		}
		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials?rank=1&account_label=fallback",
			tok, chainBlob("sk-ant-oat01-FALLBACK", "")); code != http.StatusOK {
			t.Fatalf("upload fallback = %d %s", code, body)
		}
		wantFP := chainRecord(t, st, "alice", 1).Fingerprint
		primaryFP := chainRecord(t, st, "alice", 0).Fingerprint
		if wantFP == "" || wantFP == primaryFP {
			t.Fatalf("the two links are not distinguishable (rank0=%q rank1=%q) — the case would pass for the wrong reason", primaryFP, wantFP)
		}

		code, body := oauthCall(t, hs, http.MethodPatch, "/api/me/oauth/claude_code?rank=1",
			tok, `{"account_label":"renamed-fallback"}`)
		if code != http.StatusOK {
			t.Fatalf("rename = %d %s", code, body)
		}
		var view oauthConnectionView
		if err := json.Unmarshal([]byte(body), &view); err != nil {
			t.Fatalf("decode view: %v (%s)", err, body)
		}
		// The response is what the audit line is built from, so a view of the
		// wrong record is an audit entry naming the wrong credential.
		if view.Fingerprint != wantFP {
			t.Fatalf("rename ?rank=1 answered about fp=%q, want the rank-1 link fp=%q (primary is %q)",
				view.Fingerprint, wantFP, primaryFP)
		}
		if view.AccountLabel != "renamed-fallback" {
			t.Fatalf("rename ?rank=1 answered account_label=%q, want %q", view.AccountLabel, "renamed-fallback")
		}
		if got := chainRecord(t, st, "alice", 0).AccountLabel; got != "primary" {
			t.Fatalf("renaming rank 1 changed the PRIMARY's label to %q", got)
		}
		if got := chainRecord(t, st, "alice", 1).AccountLabel; got != "renamed-fallback" {
			t.Fatalf("rank 1 label = %q after rename", got)
		}
	})

	t.Run("the listing names each link's rank", func(t *testing.T) {
		// Without it the two links of a chain are indistinguishable in the
		// only listing there is, and `?rank=` — which every write takes — has
		// no value an operator could have read anywhere.
		_, hs, signer, _ := oauthTestServer(t)
		tok := oauthJWT(t, signer, "erin")
		for _, up := range []struct{ path, token string }{
			{"/api/me/oauth/claude_code/credentials", "sk-ant-oat01-PRIMARY"},
			{"/api/me/oauth/claude_code/credentials?rank=1", "sk-ant-oat01-FALLBACK"},
		} {
			if code, body := oauthCall(t, hs, http.MethodPost, up.path, tok, chainBlob(up.token, "")); code != http.StatusOK {
				t.Fatalf("upload %s = %d %s", up.path, code, body)
			}
		}
		code, body := oauthCall(t, hs, http.MethodGet, "/api/me/oauth/connections", tok, "")
		if code != http.StatusOK {
			t.Fatalf("list = %d %s", code, body)
		}
		var envelope struct {
			Connections []oauthConnectionView `json:"connections"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Fatalf("decode listing: %v (%s)", err, body)
		}
		got := envelope.Connections
		if len(got) != 2 {
			t.Fatalf("listing has %d entries, want the 2 links of the chain: %s", len(got), body)
		}
		if got[0].Rank != 0 || got[1].Rank != 1 {
			t.Fatalf("listing ranks = %d,%d — want 0,1 in try order: %s", got[0].Rank, got[1].Rank, body)
		}
	})

	t.Run("refresh acts on the link it was asked for", func(t *testing.T) {
		_, hs, signer, _ := oauthTestServer(t)
		tok := oauthJWT(t, signer, "bob")

		// The two links differ in exactly the property the handler branches on.
		// The primary CAN refresh (and, with no client id configured, fails
		// 502 before any network call); the fallback cannot (409). So the
		// status code alone names which record the handler read.
		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials", tok,
			chainBlob("sk-ant-oat01-PRIMARY", "rt-primary")); code != http.StatusOK {
			t.Fatalf("upload primary = %d %s", code, body)
		}
		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials?rank=1", tok,
			chainBlob("sk-ant-oat01-FALLBACK", "")); code != http.StatusOK {
			t.Fatalf("upload fallback = %d %s", code, body)
		}

		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/refresh?rank=1", tok, "")
		if code == http.StatusBadGateway {
			t.Fatalf("refresh ?rank=1 refreshed the PRIMARY (502 = it reached the refreshable record); "+
				"the rank-1 link has no refresh token and must answer 409. body=%s", body)
		}
		if code != http.StatusConflict {
			t.Fatalf("refresh ?rank=1 = %d %s, want 409 (the addressed link cannot auto-refresh)", code, body)
		}
	})

	t.Run("a refused refresh describes the addressed link's state", func(t *testing.T) {
		_, hs, signer, st := oauthTestServer(t)
		tok := oauthJWT(t, signer, "carol")

		// Both links refreshable; only the FALLBACK is in a refresh cool-down.
		// Reading the primary here answers "a refresh is already in flight",
		// which sends the operator back to a button that will refuse again.
		for _, up := range []struct{ path, token string }{
			{"/api/me/oauth/claude_code/credentials", "sk-ant-oat01-PRIMARY"},
			{"/api/me/oauth/claude_code/credentials?rank=1", "sk-ant-oat01-FALLBACK"},
		} {
			if code, body := oauthCall(t, hs, http.MethodPost, up.path, tok,
				chainBlob(up.token, "rt-"+up.token)); code != http.StatusOK {
				t.Fatalf("upload %s = %d %s", up.path, code, body)
			}
		}
		rec := chainRecord(t, st, "carol", 1)
		until := time.Now().UTC().Add(42 * time.Minute)
		rec.RefreshNotBefore = &until
		if err := st.Upsert(t.Context(), rec); err != nil {
			t.Fatalf("seed cool-down: %v", err)
		}

		code, body := oauthCall(t, hs, http.MethodPost, "/api/me/oauth/claude_code/refresh?rank=1", tok, "")
		if code != http.StatusConflict {
			t.Fatalf("refresh of a cool-down link = %d %s, want 409", code, body)
		}
		if !strings.Contains(body, "cool-down") {
			t.Fatalf("the 409 describes some OTHER record's state (no cool-down mentioned): %s", body)
		}
	})

	t.Run("deleting a fallback leaves the primary's pool pledge alone", func(t *testing.T) {
		// A pledge is keyed on (owner, source, kind) with NO rank, and both
		// verifyLendable and Broker.openCredential resolve it through
		// oauthStore.Get — the PRIMARY. So withdrawing it when the operator
		// deleted a fallback revokes the donor's contribution of a credential
		// that is still connected: no error, no sign of it anywhere but the
		// pool dashboard.
		srv, hs, signer, st := oauthTestServer(t)
		tok := oauthJWT(t, signer, "frank")
		for _, up := range []struct{ path, token string }{
			{"/api/me/oauth/claude_code/credentials", "sk-ant-oat01-PRIMARY"},
			{"/api/me/oauth/claude_code/credentials?rank=1", "sk-ant-oat01-FALLBACK"},
		} {
			if code, body := oauthCall(t, hs, http.MethodPost, up.path, tok, chainBlob(up.token, "")); code != http.StatusOK {
				t.Fatalf("upload %s = %d %s", up.path, code, body)
			}
		}
		pledgeID := credpool.PledgeID("frank", credpool.SourceOAuth, string(secrets.OAuthKindClaudeCode))
		seed := func() {
			t.Helper()
			if err := srv.credPoolPledges.Upsert(t.Context(), credpool.Pledge{
				ID: pledgeID, PoolID: "pool-1", UserID: "frank",
				Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)},
				Enabled:    true, Health: credpool.HealthOK,
			}); err != nil {
				t.Fatalf("seed pledge: %v", err)
			}
		}
		seed()

		if code, body := oauthCall(t, hs, http.MethodDelete, "/api/me/oauth/claude_code?rank=1", tok, ""); code != http.StatusNoContent {
			t.Fatalf("delete rank 1 = %d %s", code, body)
		}
		if _, err := srv.credPoolPledges.Get(t.Context(), pledgeID); err != nil {
			t.Fatalf("deleting the rank-1 fallback withdrew the pledge of the rank-0 primary, which is still connected: %v", err)
		}
		if _, err := chainRecordOrErr(st, "frank", 0); err != nil {
			t.Fatalf("deleting rank 1 removed the primary: %v", err)
		}

		// The primary is the record a pledge actually points at, so deleting
		// IT must still withdraw — the guard is a narrowing, not a removal.
		if code, body := oauthCall(t, hs, http.MethodDelete, "/api/me/oauth/claude_code", tok, ""); code != http.StatusNoContent {
			t.Fatalf("delete primary = %d %s", code, body)
		}
		if _, err := srv.credPoolPledges.Get(t.Context(), pledgeID); !errors.Is(err, credpool.ErrNotFound) {
			t.Fatalf("deleting the PRIMARY left the pledge behind (err=%v); consent for that subscription is gone", err)
		}
	})

	t.Run("a delete that fails leaves the pledge in place", func(t *testing.T) {
		// The withdrawal is irreversible for the donor (only they can pledge
		// again), so it must not run for a credential the store still holds.
		srv, hs, signer, _ := oauthTestServer(t)
		tok := oauthJWT(t, signer, "grace")
		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials", tok, chainBlob("sk-ant-oat01-PRIMARY", "")); code != http.StatusOK {
			t.Fatalf("upload primary = %d %s", code, body)
		}
		pledgeID := credpool.PledgeID("grace", credpool.SourceOAuth, string(secrets.OAuthKindClaudeCode))
		if err := srv.credPoolPledges.Upsert(t.Context(), credpool.Pledge{
			ID: pledgeID, PoolID: "pool-1", UserID: "grace",
			Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindClaudeCode)},
			Enabled:    true, Health: credpool.HealthOK,
		}); err != nil {
			t.Fatalf("seed pledge: %v", err)
		}
		srv.oauthStore = failingDeleteOAuthStore{srv.oauthStore}

		if code, _ := oauthCall(t, hs, http.MethodDelete, "/api/me/oauth/claude_code", tok, ""); code != http.StatusInternalServerError {
			t.Fatalf("delete against a failing store = %d, want 500", code)
		}
		if _, err := srv.credPoolPledges.Get(t.Context(), pledgeID); err != nil {
			t.Fatalf("a FAILED delete withdrew the pledge, stranding it against a credential still in the store: %v", err)
		}
	})

	t.Run("re-uploading a link without a label keeps ITS name", func(t *testing.T) {
		_, hs, signer, st := oauthTestServer(t)
		tok := oauthJWT(t, signer, "dave")
		const fallback = "sk-ant-oat01-FALLBACK"

		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials?account_label=primary",
			tok, chainBlob("sk-ant-oat01-PRIMARY", "")); code != http.StatusOK {
			t.Fatalf("upload primary = %d %s", code, body)
		}
		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials?rank=1&account_label=jothedev",
			tok, chainBlob(fallback, "")); code != http.StatusOK {
			t.Fatalf("upload fallback = %d %s", code, body)
		}
		// The same subscription re-uploaded at the same rank, naming no
		// account — a rotation. The label is kept because the fingerprint
		// proves it is the same subscription; compared against the PRIMARY's
		// fingerprint it never matches, and the name is dropped.
		if code, body := oauthCall(t, hs, http.MethodPost,
			"/api/me/oauth/claude_code/credentials?rank=1",
			tok, chainBlob(fallback, "")); code != http.StatusOK {
			t.Fatalf("re-upload fallback = %d %s", code, body)
		}
		if got := chainRecord(t, st, "dave", 1).AccountLabel; got != "jothedev" {
			t.Fatalf("the rank-1 link lost its name on re-upload: account_label = %q, want %q", got, "jothedev")
		}
	})
}
