package pat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func runStoreSuite(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	plaintext, hash, last4, fp, err := MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		t.Fatalf("plaintext %q missing prefix", plaintext)
	}
	tok := Token{ID: "p1", UserID: "u1", Name: "ci", TokenHash: hash, TokenLast4: last4, Fingerprint: fp, CreatedAt: now}
	if err := s.Create(ctx, tok); err != nil {
		t.Fatalf("Create: %v", err)
	}

	t.Run("lookup by hash + verify", func(t *testing.T) {
		got, err := s.GetByTokenHash(ctx, HashToken(plaintext))
		if err != nil {
			t.Fatalf("GetByTokenHash: %v", err)
		}
		if !VerifyToken(plaintext, got.TokenHash) {
			t.Fatal("VerifyToken = false for the minted plaintext")
		}
		if VerifyToken(plaintext+"x", got.TokenHash) {
			t.Fatal("VerifyToken accepted a tampered token")
		}
	})

	t.Run("list by user", func(t *testing.T) {
		toks, err := s.ListByUser(ctx, "u1")
		if err != nil || len(toks) != 1 {
			t.Fatalf("ListByUser = %v, %v", toks, err)
		}
		other, _ := s.ListByUser(ctx, "u2")
		if len(other) != 0 {
			t.Fatalf("cross-user leak: %v", other)
		}
	})

	t.Run("revoke makes unusable, row kept", func(t *testing.T) {
		if err := s.Revoke(ctx, "p1", now.Add(time.Hour)); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		got, err := s.Get(ctx, "p1")
		if err != nil {
			t.Fatalf("Get after revoke: %v", err)
		}
		if got.Usable(now.Add(2 * time.Hour)) {
			t.Fatal("revoked token still usable")
		}
	})

	t.Run("a revoke re-issued on a revoked token keeps the first instant", func(t *testing.T) {
		if err := s.Revoke(ctx, "p1", now.Add(3*time.Hour)); err != nil {
			t.Fatalf("a re-issued Revoke: %v, want nil: the token is revoked", err)
		}
		got, err := s.Get(ctx, "p1")
		if err != nil {
			t.Fatalf("Get after the re-issued revoke: %v", err)
		}
		if got.RevokedAt == nil || !got.RevokedAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("the re-issued revoke moved the revocation instant to %v, want the first one %v", got.RevokedAt, now.Add(time.Hour))
		}
		if err := s.Revoke(ctx, "ghost", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Revoke(ghost) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("mark used", func(t *testing.T) {
		if err := s.MarkUsed(ctx, "p1", now.Add(time.Minute)); err != nil {
			t.Fatalf("MarkUsed: %v", err)
		}
		got, _ := s.Get(ctx, "p1")
		if got.LastUsedAt == nil {
			t.Fatal("LastUsedAt not set")
		}
	})

	t.Run("not found", func(t *testing.T) {
		if _, err := s.Get(ctx, "ghost"); err != ErrNotFound {
			t.Fatalf("Get(ghost) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("revoking every token of an account takes the live ones and nothing else", func(t *testing.T) {
		for _, tk := range []Token{
			{ID: "ra-1", UserID: "ra", Name: "ci", TokenHash: "h-ra-1", CreatedAt: now},
			{ID: "ra-2", UserID: "ra", Name: "laptop", TokenHash: "h-ra-2", CreatedAt: now},
			{ID: "rb-1", UserID: "rb", Name: "other account", TokenHash: "h-rb-1", CreatedAt: now},
		} {
			if err := s.Create(ctx, tk); err != nil {
				t.Fatalf("Create %s: %v", tk.ID, err)
			}
		}
		earlier := now.Add(time.Minute)
		if err := s.Revoke(ctx, "ra-2", earlier); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		at := now.Add(time.Hour)
		n, err := s.RevokeAllByUser(ctx, "ra", at)
		if err != nil {
			t.Fatalf("RevokeAllByUser: %v", err)
		}
		if n != 1 {
			t.Fatalf("RevokeAllByUser revoked %d token(s), want the one still live", n)
		}
		live, _ := s.Get(ctx, "ra-1")
		if live.RevokedAt == nil || !live.RevokedAt.Equal(at) || live.Usable(at.Add(time.Second)) {
			t.Fatalf("the live token after RevokeAllByUser: %+v", live)
		}
		kept, _ := s.Get(ctx, "ra-2")
		if kept.RevokedAt == nil || !kept.RevokedAt.Equal(earlier) {
			t.Fatalf("RevokeAllByUser moved a revocation instant: %+v, want %v", kept.RevokedAt, earlier)
		}
		other, _ := s.Get(ctx, "rb-1")
		if other.RevokedAt != nil {
			t.Fatal("RevokeAllByUser revoked another account's token")
		}
		if n, err := s.RevokeAllByUser(ctx, "ra", at.Add(time.Hour)); err != nil || n != 0 {
			t.Fatalf("a second RevokeAllByUser revoked %d (err %v), want 0", n, err)
		}
	})

	t.Run("revoke-if-live tells the one call that revoked from the ones that came after", func(t *testing.T) {
		if err := s.Create(ctx, Token{ID: "cas1", UserID: "cu", Name: "ci", TokenHash: "h-cas1", CreatedAt: now}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		first := now.Add(time.Minute)
		revoked, err := s.RevokeIfLive(ctx, "cas1", first)
		if err != nil || !revoked {
			t.Fatalf("RevokeIfLive on a live token = %v, %v, want true, nil", revoked, err)
		}
		revoked, err = s.RevokeIfLive(ctx, "cas1", first.Add(time.Hour))
		if err != nil || revoked {
			t.Fatalf("RevokeIfLive on a revoked token = %v, %v, want false, nil", revoked, err)
		}
		if got, _ := s.Get(ctx, "cas1"); got.RevokedAt == nil || !got.RevokedAt.Equal(first) {
			t.Fatalf("the losing RevokeIfLive moved the instant to %v, want %v", got.RevokedAt, first)
		}
		if _, err := s.RevokeIfLive(ctx, "ghost", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("RevokeIfLive(ghost) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("disable suspends, enable restores, the first disable instant stands", func(t *testing.T) {
		if err := s.Create(ctx, Token{ID: "d1", UserID: "du", Name: "ci", TokenHash: "h-d1", CreatedAt: now}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		first := now.Add(time.Minute)
		if err := s.Disable(ctx, "d1", first); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		got, err := s.Get(ctx, "d1")
		if err != nil {
			t.Fatalf("Get after disable: %v", err)
		}
		if got.DisabledAt == nil || !got.DisabledAt.Equal(first) || got.Usable(first.Add(time.Second)) {
			t.Fatalf("a disabled token reads %+v, want DisabledAt %v and unusable", got, first)
		}
		if err := s.Disable(ctx, "d1", first.Add(time.Hour)); err != nil {
			t.Fatalf("a re-issued Disable: %v, want nil", err)
		}
		if got, _ := s.Get(ctx, "d1"); got.DisabledAt == nil || !got.DisabledAt.Equal(first) {
			t.Fatalf("a re-issued disable moved the instant to %v, want %v", got.DisabledAt, first)
		}
		if err := s.Enable(ctx, "d1"); err != nil {
			t.Fatalf("Enable: %v", err)
		}
		got, _ = s.Get(ctx, "d1")
		if got.DisabledAt != nil || !got.Usable(first.Add(time.Second)) {
			t.Fatalf("an enabled token reads %+v, want no DisabledAt and usable", got)
		}
		if err := s.Enable(ctx, "d1"); err != nil {
			t.Fatalf("Enable on a live token: %v, want nil", err)
		}
		if err := s.Disable(ctx, "ghost", now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Disable(ghost) err = %v, want ErrNotFound", err)
		}
		if err := s.Enable(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Enable(ghost) err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a revoked token is neither disabled nor enabled", func(t *testing.T) {
		if err := s.Create(ctx, Token{ID: "d2", UserID: "du", Name: "laptop", TokenHash: "h-d2", CreatedAt: now}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Disable(ctx, "d2", now.Add(time.Minute)); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if err := s.Revoke(ctx, "d2", now.Add(time.Hour)); err != nil {
			t.Fatalf("Revoke a disabled token: %v", err)
		}
		if err := s.Enable(ctx, "d2"); !errors.Is(err, ErrRevoked) {
			t.Fatalf("Enable on a revoked token: %v, want ErrRevoked", err)
		}
		if got, _ := s.Get(ctx, "d2"); got.DisabledAt == nil || got.Usable(now.Add(2*time.Hour)) {
			t.Fatalf("a refused Enable changed the revoked token: %+v", got)
		}
		if err := s.Disable(ctx, "p1", now); !errors.Is(err, ErrRevoked) {
			t.Fatalf("Disable on a revoked token: %v, want ErrRevoked", err)
		}
	})

	t.Run("purge removes a revoked row only", func(t *testing.T) {
		for _, tk := range []Token{
			{ID: "g-live", UserID: "gu", Name: "live", TokenHash: "h-g-live", CreatedAt: now},
			{ID: "g-off", UserID: "gu", Name: "off", TokenHash: "h-g-off", CreatedAt: now},
			{ID: "g-dead", UserID: "gu", Name: "dead", TokenHash: "h-g-dead", CreatedAt: now},
		} {
			if err := s.Create(ctx, tk); err != nil {
				t.Fatalf("Create %s: %v", tk.ID, err)
			}
		}
		if err := s.Disable(ctx, "g-off", now); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if err := s.Revoke(ctx, "g-dead", now); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		for _, id := range []string{"g-live", "g-off"} {
			if err := s.Purge(ctx, id); !errors.Is(err, ErrNotRevoked) {
				t.Fatalf("Purge(%s) err = %v, want ErrNotRevoked", id, err)
			}
			if _, err := s.Get(ctx, id); err != nil {
				t.Fatalf("a refused purge removed %s: %v", id, err)
			}
		}
		if err := s.Purge(ctx, "g-dead"); err != nil {
			t.Fatalf("Purge of a revoked token: %v", err)
		}
		if _, err := s.Get(ctx, "g-dead"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get after purge err = %v, want ErrNotFound", err)
		}
		if err := s.Purge(ctx, "g-dead"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a re-issued Purge err = %v, want ErrNotFound", err)
		}
		toks, err := s.ListByUser(ctx, "gu")
		if err != nil || len(toks) != 2 {
			t.Fatalf("ListByUser after purge = %d rows (%v), want the 2 not purged", len(toks), err)
		}
	})
}

func TestMemoryStore(t *testing.T) { runStoreSuite(t, NewMemoryStore()) }

func TestUsable(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	cases := []struct {
		name string
		tok  Token
		want bool
	}{
		{"fresh", Token{}, true},
		{"expired", Token{ExpiresAt: &past}, false},
		{"not yet expired", Token{ExpiresAt: &future}, true},
		{"revoked", Token{RevokedAt: &past}, false},
		{"disabled", Token{DisabledAt: &past}, false},
		{"disabled, not yet expired", Token{DisabledAt: &past, ExpiresAt: &future}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tok.Usable(now); got != c.want {
				t.Fatalf("Usable = %v, want %v", got, c.want)
			}
		})
	}
}
