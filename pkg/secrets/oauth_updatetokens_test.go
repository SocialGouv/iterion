package secrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A refresh must write only what a refresh owns. The lost update this
// closes: the worker reads a record, spends a round trip at the provider,
// and writes the whole record back — reverting a rename an operator
// committed in between to the label the worker had read.
func TestMemoryOAuthStore_UpdateTokensKeepsAConcurrentRename(t *testing.T) {
	s := NewMemoryOAuthStore()
	ctx := context.Background()
	if err := s.Upsert(ctx, OAuthRecord{
		UserID: "alice", Kind: OAuthKindClaudeCode,
		SealedPayload: []byte("sealed-v1"), Fingerprint: "fp-1", AccountLabel: "old name",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// What a refresh site holds: the record as it was BEFORE the rename.
	stale, err := s.Get(ctx, "alice", OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindClaudeCode, "jothedev"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// …then the refresh lands.
	exp := time.Now().Add(time.Hour).UTC()
	last := time.Now().UTC()
	stale.SealedPayload = []byte("sealed-v2")
	stale.AccessTokenExpiresAt = &exp
	stale.LastRefreshedAt = &last
	if err := s.UpdateTokens(ctx, "alice", OAuthKindClaudeCode, OAuthTokenUpdateFrom(stale)); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "jothedev" {
		t.Fatalf("label after a refresh = %q, want the rename to survive", got.AccountLabel)
	}
	if string(got.SealedPayload) != "sealed-v2" {
		t.Fatalf("payload = %q, want the refreshed one", got.SealedPayload)
	}
	if got.AccessTokenExpiresAt == nil || !got.AccessTokenExpiresAt.Equal(exp) {
		t.Fatalf("expiry = %v, want %v", got.AccessTokenExpiresAt, exp)
	}
}

// The self-heal flips one flag on a record it never re-sealed: a nil
// payload must leave the sealed blob alone rather than blank it.
func TestMemoryOAuthStore_UpdateTokensNilPayloadKeepsTheBlob(t *testing.T) {
	s := NewMemoryOAuthStore()
	ctx := context.Background()
	if err := s.Upsert(ctx, OAuthRecord{
		UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed"), Fingerprint: "fp-1",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpdateTokens(ctx, "alice", OAuthKindCodex, OAuthTokenUpdate{NotRefreshable: true}); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NotRefreshable {
		t.Fatal("not_refreshable was not set")
	}
	if string(got.SealedPayload) != "sealed" || got.Fingerprint != "fp-1" {
		t.Fatalf("self-heal disturbed the credential: payload=%q fp=%q", got.SealedPayload, got.Fingerprint)
	}
}

func TestMemoryOAuthStore_UpdateTokensMissingRecord(t *testing.T) {
	s := NewMemoryOAuthStore()
	if err := s.UpdateTokens(context.Background(), "nobody", OAuthKindClaudeCode, OAuthTokenUpdate{}); !errors.Is(err, ErrOAuthNotFound) {
		t.Fatalf("UpdateTokens on a missing record = %v, want ErrOAuthNotFound", err)
	}
}

// The end-to-end interleaving, at the site that pays for it: the rename
// lands WHILE the worker is waiting on the provider. Before the partial
// write, RunOnce's whole-record Upsert reverted the label to the one it
// had read a round trip earlier.
func TestOAuthRefreshWorker_RenameDuringTheRoundTripSurvives(t *testing.T) {
	freshRetrySchedule(t)
	sealer, err := NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	st := NewMemoryOAuthStore()
	seedRecord(t, st, sealer, "alice", OAuthKindClaudeCode, time.Now().Add(5*time.Minute))
	if err := st.SetAccountLabel(context.Background(), "alice", OAuthKindClaudeCode, "old name"); err != nil {
		t.Fatalf("seed label: %v", err)
	}

	// The provider hop is where the operator renames the connection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := st.SetAccountLabel(context.Background(), "alice", OAuthKindClaudeCode, "jothedev"); err != nil {
			t.Errorf("concurrent rename: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-fresh1234567890abcdef","refresh_token":"rf-fresh","expires_in":28800}`))
	}))
	defer srv.Close()

	wk := &OAuthRefreshWorker{
		Store:             st,
		Sealer:            sealer,
		HTTP:              redirectingClient(srv.URL),
		AnthropicClientID: "client-xyz",
		Lead:              30 * time.Minute,
	}
	if n, err := wk.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v; want 1, nil", n, err)
	}
	rec, err := st.Get(context.Background(), "alice", OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if rec.AccountLabel != "jothedev" {
		t.Fatalf("account label = %q, want %q — the refresh reverted a rename it never read", rec.AccountLabel, "jothedev")
	}
	blob, err := OpenOAuthPayload(sealer, "alice", OAuthKindClaudeCode, rec.SealedPayload)
	if err != nil {
		t.Fatalf("unseal: %v", err)
	}
	v, _ := ParseAnthropicView(blob)
	if v.ClaudeAIOauth.AccessToken != "sk-ant-fresh1234567890abcdef" {
		t.Fatalf("token not rotated: %q", v.ClaudeAIOauth.AccessToken)
	}
}
