package pat

import (
	"context"
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tok.Usable(now); got != c.want {
				t.Fatalf("Usable = %v, want %v", got, c.want)
			}
		})
	}
}
