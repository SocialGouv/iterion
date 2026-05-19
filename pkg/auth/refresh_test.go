package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHashRefreshToken_Deterministic(t *testing.T) {
	a := HashRefreshToken("hello-world")
	b := HashRefreshToken("hello-world")
	if a != b {
		t.Errorf("hash should be deterministic: %s != %s", a, b)
	}
	c := HashRefreshToken("HELLO-WORLD")
	if a == c {
		t.Error("hash collides across case variants — bad")
	}
}

func TestIssueSession_StoresHashedOnly(t *testing.T) {
	store := NewMemorySessionStore()
	tok, sess, err := IssueSession(context.Background(), store, "u-1", "ua", "127.0.0.1", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	if sess.TokenHash == "" || sess.TokenHash == tok {
		t.Errorf("expected hashed token, got %q (raw=%q)", sess.TokenHash, tok)
	}
	if sess.UserID != "u-1" || sess.UserAgent != "ua" || sess.IP != "127.0.0.1" {
		t.Errorf("session fields: %+v", sess)
	}
	if sess.ExpiresAt.Before(sess.IssuedAt) {
		t.Errorf("exp before issued: %v vs %v", sess.ExpiresAt, sess.IssuedAt)
	}
}

func TestRotateSession_HappyPath(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	tok, _, err := IssueSession(ctx, store, "u-1", "", "", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	newTok, newSess, prev, err := RotateSession(ctx, store, tok, "ua2", "1.1.1.1", time.Hour)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if newTok == "" || newTok == tok {
		t.Errorf("expected new distinct token, got %q (old=%q)", newTok, tok)
	}
	if newSess.RotatedFromID != prev.ID {
		t.Errorf("rotated_from not set: %+v", newSess)
	}
	if newSess.UserAgent != "ua2" || newSess.IP != "1.1.1.1" {
		t.Errorf("new session metadata not applied: %+v", newSess)
	}
	// Previous session must be revoked now.
	prevReloaded, err := store.GetSessionByTokenHash(ctx, HashRefreshToken(tok))
	if err != nil {
		t.Fatalf("get prev: %v", err)
	}
	if prevReloaded.RevokedAt == nil {
		t.Error("previous session not revoked after rotation")
	}
}

func TestRotateSession_RejectsUnknownToken(t *testing.T) {
	store := NewMemorySessionStore()
	_, _, _, err := RotateSession(context.Background(), store, "not-a-real-token", "", "", time.Hour)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound, got %v", err)
	}
}

func TestRotateSession_RejectsRevokedToken(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	tok, sess, _ := IssueSession(ctx, store, "u-1", "", "", time.Hour)
	if err := store.RevokeSession(ctx, sess.ID, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, _, _, err := RotateSession(ctx, store, tok, "", "", time.Hour)
	if !errors.Is(err, ErrSessionRevoked) {
		t.Errorf("expected ErrSessionRevoked, got %v", err)
	}
}

func TestRotateSession_RejectsExpiredToken(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	// Issue a session with a negative TTL so it's born expired.
	tok, _, err := IssueSession(ctx, store, "u-1", "", "", -time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	_, _, _, err = RotateSession(ctx, store, tok, "", "", time.Hour)
	if !errors.Is(err, ErrSessionExpired) {
		t.Errorf("expected ErrSessionExpired, got %v", err)
	}
}

func TestRotateSession_ConcurrentRotationsExactlyOneWins(t *testing.T) {
	// Core safety guarantee: N parallel RotateSession calls on the same
	// token must produce exactly one winner. All losers must see
	// ErrSessionRevoked — either because the CAS-revoke caught them
	// (which also triggers the user-wide lockdown defense at
	// refresh.go:122) or because the early-exit "RevokedAt != nil"
	// check did. The user-wide lockdown is best-effort and only fires
	// on the CAS path, so the test doesn't assert it — exercising
	// that branch reliably would require timing injection.
	store := NewMemorySessionStore()
	ctx := context.Background()
	tok, _, _ := IssueSession(ctx, store, "u-multi", "", "", time.Hour)

	const N = 8
	var wg sync.WaitGroup
	results := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, _, _, err := RotateSession(ctx, store, tok, "", "", time.Hour)
			results[i] = err
		}()
	}
	wg.Wait()

	var wins, losses int
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrSessionRevoked):
			losses++
		default:
			t.Errorf("unexpected err: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("expected exactly one rotation to win; got wins=%d losses=%d", wins, losses)
	}
	if losses != N-1 {
		t.Errorf("expected N-1 losses with ErrSessionRevoked; got %d", losses)
	}
}

func TestMemorySessionStore_RejectsDuplicateHash(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	sess := Session{
		ID:        "id-1",
		UserID:    "u",
		TokenHash: "h",
		IssuedAt:  time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	dup := sess
	dup.ID = "id-2"
	err := store.CreateSession(ctx, dup)
	if err == nil || !strings.Contains(err.Error(), "collision") {
		t.Errorf("expected hash collision err, got %v", err)
	}
}

func TestMemorySessionStore_DeleteExpired(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	now := time.Now()
	fresh := Session{
		ID:        "fresh",
		UserID:    "u",
		TokenHash: "h-fresh",
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	}
	stale := Session{
		ID:        "stale",
		UserID:    "u",
		TokenHash: "h-stale",
		IssuedAt:  now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}
	_ = store.CreateSession(ctx, fresh)
	_ = store.CreateSession(ctx, stale)

	n, err := store.DeleteExpired(ctx, now)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 expired, got %d", n)
	}
	if _, err := store.GetSessionByTokenHash(ctx, "h-stale"); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("stale should be gone, got %v", err)
	}
	if _, err := store.GetSessionByTokenHash(ctx, "h-fresh"); err != nil {
		t.Errorf("fresh should still exist, got %v", err)
	}
}

func TestMemorySessionStore_RevokeUserSessions_SkipsAlreadyRevoked(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	now := time.Now()
	earlier := now.Add(-time.Hour)
	revokedSess := Session{
		ID:        "rv",
		UserID:    "u",
		TokenHash: "h-rv",
		IssuedAt:  earlier,
		ExpiresAt: now.Add(time.Hour),
		RevokedAt: &earlier,
	}
	freshSess := Session{
		ID:        "fr",
		UserID:    "u",
		TokenHash: "h-fr",
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	}
	_ = store.CreateSession(ctx, revokedSess)
	_ = store.CreateSession(ctx, freshSess)

	if err := store.RevokeUserSessions(ctx, "u", now); err != nil {
		t.Fatalf("revoke user: %v", err)
	}
	rv, _ := store.GetSessionByTokenHash(ctx, "h-rv")
	if rv.RevokedAt == nil || !rv.RevokedAt.Equal(earlier) {
		t.Errorf("already-revoked timestamp should not be overwritten: %+v", rv.RevokedAt)
	}
	fr, _ := store.GetSessionByTokenHash(ctx, "h-fr")
	if fr.RevokedAt == nil {
		t.Error("fresh session should now be revoked")
	}
}

func TestMemorySessionStore_RevokeSessionIfNotRevoked_CAS(t *testing.T) {
	store := NewMemorySessionStore()
	ctx := context.Background()
	now := time.Now()
	sess := Session{
		ID:        "s",
		UserID:    "u",
		TokenHash: "h",
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	}
	_ = store.CreateSession(ctx, sess)

	ok, err := store.RevokeSessionIfNotRevoked(ctx, "s", now)
	if err != nil || !ok {
		t.Fatalf("first cas: ok=%v err=%v", ok, err)
	}
	ok, err = store.RevokeSessionIfNotRevoked(ctx, "s", now.Add(time.Second))
	if err != nil {
		t.Fatalf("second cas err: %v", err)
	}
	if ok {
		t.Error("second cas should NOT report revoked=true (already revoked)")
	}

	// Unknown id surfaces ErrSessionNotFound.
	_, err = store.RevokeSessionIfNotRevoked(ctx, "missing", now)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected ErrSessionNotFound for unknown id, got %v", err)
	}
}

func TestGenerateRandomTokenRoundtripsViaIssue(t *testing.T) {
	// Sanity: GenerateRandomToken is called via IssueSession; verify
	// the produced token has reasonable length and is URL-safe so it
	// can flow through a cookie / HTTP header without encoding gymnastics.
	store := NewMemorySessionStore()
	tok, _, err := IssueSession(context.Background(), store, "u", "", "", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(tok) < 32 {
		t.Errorf("token too short: %d bytes", len(tok))
	}
	for _, r := range tok {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			t.Errorf("token contains non-URL-safe char %q", r)
		}
	}
}
