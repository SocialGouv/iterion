package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// flakyRevokeStore wraps the in-memory store but can force the
// CAS-revoke to report "already revoked" (driving the token-reuse
// branch) and make the record of an account or a lineage revocation fail,
// to prove a revocation that fails is answered and logged as a failure
// rather than swallowed.
type flakyRevokeStore struct {
	*MemorySessionStore
	forceCASLost       bool
	revokeUserErr      error
	revokeUserCalls    int
	revokeLineageErr   error
	revokeLineageCalls int
}

func (s *flakyRevokeStore) RecordLineageRevocation(ctx context.Context, userID, lineageID string, at time.Time) error {
	s.revokeLineageCalls++
	if s.revokeLineageErr != nil {
		return s.revokeLineageErr
	}
	return s.MemorySessionStore.RecordLineageRevocation(ctx, userID, lineageID, at)
}

func (s *flakyRevokeStore) RevokeSessionIfNotRevoked(ctx context.Context, id string, at time.Time) (bool, error) {
	if s.forceCASLost {
		return false, nil
	}
	return s.MemorySessionStore.RevokeSessionIfNotRevoked(ctx, id, at)
}

func (s *flakyRevokeStore) RecordAccountRevocation(ctx context.Context, userID string, at time.Time) (int64, error) {
	s.revokeUserCalls++
	if s.revokeUserErr != nil {
		return 0, s.revokeUserErr
	}
	return s.MemorySessionStore.RecordAccountRevocation(ctx, userID, at)
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
	tok, sess, err := IssueSession(context.Background(), store, NewAccountRoot(), "u-1", "ua", "127.0.0.1", time.Hour)
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

// Service.Refresh logs (does not swallow) a failed revocation on the reuse
// branch: the lineage's, for a session that names one, and the account's,
// for a row written before lineages were recorded.
// TestServiceRefresh_AReuseWhoseRevocationFailsIsAFailure: reuse detection
// whose revocation fails must not answer the refusal it would answer once the
// lineage (or, for a row written before lineages, the account) is revoked —
// the presenter would drop a token whose family is still live, and nothing
// would retry the revocation. It answers a failure, logged, and the next
// presentation of the same token, once the store answers, is reuse again and
// revokes.
func TestServiceRefresh_AReuseWhoseRevocationFailsIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		legacy bool
	}{
		{"a session of a lineage", false},
		{"a row written before lineages", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &flakyRevokeStore{
				MemorySessionStore: NewMemorySessionStore(),
				revokeUserErr:      errors.New("mongo down"),
				revokeLineageErr:   errors.New("mongo down"),
			}
			ctx := context.Background()
			tok, sess, err := IssueSession(ctx, store, NewAccountRoot(), "u-1", "", "", time.Hour)
			if err != nil {
				t.Fatalf("issue: %v", err)
			}
			// Pre-revoke the session so Refresh hits the "RevokedAt != nil"
			// reuse branch.
			at := time.Now().UTC()
			sess.RevokedAt = &at
			if tc.legacy {
				sess.LineageID = ""
			}
			store.mu.Lock()
			store.byID[sess.ID] = sess
			store.mu.Unlock()

			buf := &bytes.Buffer{}
			svc := &Service{
				store:    identity.NewMemoryStore(),
				sessions: store,
				now:      func() time.Time { return time.Now().UTC() },
				logger:   iterlog.New(iterlog.LevelError, buf),
			}
			_, err = svc.Refresh(ctx, tok, "", "")
			if err == nil || errors.Is(err, ErrSessionRevoked) {
				t.Fatalf("a reuse whose revocation failed: got %v, want the store's failure, not the refusal", err)
			}
			wantUser, wantLineage := 0, 1
			if tc.legacy {
				wantUser, wantLineage = 1, 0
			}
			if store.revokeUserCalls != wantUser || store.revokeLineageCalls != wantLineage {
				t.Fatalf("RecordAccountRevocation calls = %d, RecordLineageRevocation calls = %d; want %d, %d", store.revokeUserCalls, store.revokeLineageCalls, wantUser, wantLineage)
			}
			if !strings.Contains(buf.String(), "mongo down") {
				t.Fatalf("revoke failure not logged; log = %q", buf.String())
			}
			store.revokeUserErr, store.revokeLineageErr = nil, nil
			if _, err := svc.Refresh(ctx, tok, "", ""); !errors.Is(err, ErrSessionRevoked) {
				t.Fatalf("the same token presented once the store answers: got %v, want ErrSessionRevoked (the revocation retried)", err)
			}
			if revoked, err := store.SessionRevoked(ctx, sess); err != nil || !revoked {
				t.Fatalf("after the retried revocation the session's revocation reads revoked=%t err=%v, want true", revoked, err)
			}
		})
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

func TestMemorySessionStore_RevokeAccountSessions_SkipsAlreadyRevoked(t *testing.T) {
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

	generation, err := store.RecordAccountRevocation(ctx, "u", now)
	if err != nil {
		t.Fatalf("record the account revocation: %v", err)
	}
	if _, err := store.RevokeAccountSessions(ctx, "u", generation, now); err != nil {
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
	tok, _, err := IssueSession(context.Background(), store, NewAccountRoot(), "u", "", "", time.Hour)
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
