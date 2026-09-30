package secrets

import (
	"context"
	"testing"
	"time"
)

// assertConnectTimeMovesOnlyOnAConnect: a lent slot is held to its record's
// connect time (OAuthRecord.CreatedAt), so the store must keep it through
// the refresh worker's writes — a re-stamp of the fingerprint included — and
// take the new one on a re-connect, whose Upsert writes the whole record.
func assertConnectTimeMovesOnlyOnAConnect(t *testing.T, ctx context.Context, s OAuthStore) {
	t.Helper()
	id := OAuthRecordID("alice", OAuthKindClaudeCode, 0)
	connected := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if err := s.Upsert(ctx, OAuthRecord{UserID: "alice", Kind: OAuthKindClaudeCode, SealedPayload: []byte("sealed-v1"), Fingerprint: "fp-subscription", CreatedAt: connected}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := s.UpdateTokens(ctx, id, OAuthTokenUpdate{SealedPayload: []byte("sealed-v2"), Fingerprint: "fp-account"}); err != nil {
		t.Fatalf("the worker's rotation: %v", err)
	}
	got, err := s.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != "fp-account" || !got.CreatedAt.Equal(connected) {
		t.Fatalf("after a re-stamping rotation: fingerprint %q connect time %s, want fp-account and %s kept", got.Fingerprint, got.CreatedAt, connected)
	}
	reconnected := connected.Add(time.Hour)
	if err := s.Upsert(ctx, OAuthRecord{UserID: "alice", Kind: OAuthKindClaudeCode, SealedPayload: []byte("sealed-other"), Fingerprint: "fp-other", CreatedAt: reconnected}); err != nil {
		t.Fatalf("re-connect: %v", err)
	}
	if got, err = s.GetByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(reconnected) {
		t.Fatalf("after a re-connect: connect time %s, want %s", got.CreatedAt, reconnected)
	}
}

func TestMemoryOAuth_TheConnectTimeMovesOnlyOnAConnect(t *testing.T) {
	assertConnectTimeMovesOnlyOnAConnect(t, t.Context(), NewMemoryOAuthStore())
}

func TestMongoOAuth_TheConnectTimeMovesOnlyOnAConnect(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	assertConnectTimeMovesOnlyOnAConnect(t, ctx, s)
}
