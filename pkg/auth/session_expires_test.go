package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestSessionExpiresAt_NamesTheSessionsBoundNotTheAccessTokensExp: the bound
// the service reads is the lineage's live session row's expires_at — the
// refresh session's clock (the refresh TTL), never the access token's own
// exp (the access TTL). The two TTLs are set far apart on purpose, so a
// method that leaked the access token's exp cannot pass.
func TestSessionExpiresAt_NamesTheSessionsBoundNotTheAccessTokensExp(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// Access TTL 10 minutes, refresh TTL 30 days: the two clocks the answer
	// must tell apart are hours apart.
	signer, err := NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 10*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := NewService(Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: SignupOpen,
		RefreshTTL: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	reg, err := svc.Register(ctx, "bound@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess, err := svc.sessions.GetSessionByTokenHash(ctx, HashRefreshToken(reg.RefreshToken))
	if err != nil {
		t.Fatalf("read the session of the refresh token: %v", err)
	}
	bound, found, err := svc.SessionExpiresAt(ctx, sess.UserID, sess.LineageID)
	if err != nil || !found {
		t.Fatalf("SessionExpiresAt: found=%t err=%v, want the login's live session", found, err)
	}
	if !bound.Equal(sess.ExpiresAt) {
		t.Fatalf("the bound read %v, want the live row's expires_at %v", bound, sess.ExpiresAt)
	}
	until := time.Until(bound)
	if until < 24*time.Hour {
		t.Fatalf("the bound is %s away, want the refresh TTL's neighborhood (the access TTL is 10m)", until)
	}
}

// TestSessionExpiresAt_AfterASignOutTheLineageHoldsNoLiveSession: once the
// sign-out has landed, the lineage holds no live row — found is false with a
// nil error, never a bound nobody holds.
func TestSessionExpiresAt_AfterASignOutTheLineageHoldsNoLiveSession(t *testing.T) {
	ctx := context.Background()
	svc := newTestServiceOn(t, identity.NewMemoryStore(), NewMemorySessionStore())
	reg, err := svc.Register(ctx, "signedout@example.org", "correct-horse-9", "", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess, err := svc.sessions.GetSessionByTokenHash(ctx, HashRefreshToken(reg.RefreshToken))
	if err != nil {
		t.Fatalf("read the session of the refresh token: %v", err)
	}
	signedOut, err := svc.LogoutOf(ctx, reg.RefreshToken, sess.LineageID)
	if err != nil || !signedOut {
		t.Fatalf("LogoutOf: signedOut=%t err=%v, want signed out", signedOut, err)
	}
	svc.WaitRevocationPasses()
	bound, found, err := svc.SessionExpiresAt(ctx, sess.UserID, sess.LineageID)
	if err != nil || found {
		t.Fatalf("after the sign-out: found=%t err=%v, want (false, nil)", found, err)
	}
	if !bound.IsZero() {
		t.Fatalf("after the sign-out the bound reads %v, want the zero time", bound)
	}
}
