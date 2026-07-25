package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// flakyRevokeStore wraps the in-memory store but can force the
// CAS-revoke to report "already revoked" (driving the token-reuse
// branch) and make RevokeUserSessions fail, to prove the cleanup error
// is surfaced rather than swallowed.
type flakyRevokeStore struct {
	*MemorySessionStore
	forceCASLost    bool
	revokeUserErr   error
	revokeUserCalls int
}

func (s *flakyRevokeStore) RevokeSessionIfNotRevoked(ctx context.Context, id string, at time.Time) (bool, error) {
	if s.forceCASLost {
		return false, nil
	}
	return s.MemorySessionStore.RevokeSessionIfNotRevoked(ctx, id, at)
}

func (s *flakyRevokeStore) RevokeUserSessions(ctx context.Context, userID string, at time.Time) error {
	s.revokeUserCalls++
	return s.revokeUserErr
}

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

// Service.Refresh logs (does not swallow) a failed sibling-revoke on the
// reuse branch.
func TestServiceRefresh_RevokeFailureLogged(t *testing.T) {
	store := &flakyRevokeStore{
		MemorySessionStore: NewMemorySessionStore(),
		revokeUserErr:      errors.New("mongo down"),
	}
	ctx := context.Background()
	tok, sess, err := IssueSession(ctx, store, "u-1", "", "", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Pre-revoke the session so Refresh hits the "RevokedAt != nil"
	// reuse branch.
	if err := store.RevokeSession(ctx, sess.ID, time.Now().UTC()); err != nil {
		t.Fatalf("pre-revoke: %v", err)
	}

	buf := &bytes.Buffer{}
	svc := &Service{
		sessions: store,
		now:      func() time.Time { return time.Now().UTC() },
		logger:   iterlog.New(iterlog.LevelError, buf),
	}
	if _, err := svc.Refresh(ctx, tok, "", ""); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("want ErrSessionRevoked, got %v", err)
	}
	if store.revokeUserCalls != 1 {
		t.Fatalf("RevokeUserSessions calls = %d; want 1", store.revokeUserCalls)
	}
	if !strings.Contains(buf.String(), "mongo down") {
		t.Fatalf("revoke failure not logged; log = %q", buf.String())
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
