package desktopsso

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

func TestMemoryStore_SingleUse(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(time.Minute)
	res := auth.LoginResult{
		AccessToken:  "acc-1",
		RefreshToken: "ref-1",
		User:         identity.User{ID: "u1", Email: "a@b.io"},
		ActiveTeamID: "team-1",
	}

	ticket, err := s.Mint(ctx, res)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ticket == "" {
		t.Fatal("empty ticket")
	}

	got, err := s.Redeem(ctx, ticket)
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if got.AccessToken != "acc-1" || got.User.ID != "u1" || got.ActiveTeamID != "team-1" {
		t.Errorf("redeemed result mismatch: %+v", got)
	}

	// Second redeem must fail — single use.
	if _, err := s.Redeem(ctx, ticket); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("second redeem err = %v, want ErrTicketNotFound", err)
	}
	// Unknown ticket.
	if _, err := s.Redeem(ctx, "nope"); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("unknown redeem err = %v, want ErrTicketNotFound", err)
	}
}

func TestMemoryStore_Expiry(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(-1 * time.Second) // NewMemoryStore floors <=0 to 2m…
	// …so force an already-expired entry directly to exercise the TTL check.
	s.ttl = -1 * time.Second
	ticket, err := s.Mint(ctx, auth.LoginResult{AccessToken: "x"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := s.Redeem(ctx, ticket); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("expired redeem err = %v, want ErrTicketNotFound", err)
	}
}

func TestMemoryStore_DistinctTickets(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(time.Minute)
	a, _ := s.Mint(ctx, auth.LoginResult{AccessToken: "a"})
	b, _ := s.Mint(ctx, auth.LoginResult{AccessToken: "b"})
	if a == b {
		t.Fatal("two mints produced the same ticket")
	}
	ra, _ := s.Redeem(ctx, a)
	rb, _ := s.Redeem(ctx, b)
	if ra.AccessToken != "a" || rb.AccessToken != "b" {
		t.Errorf("tickets crossed: a=%q b=%q", ra.AccessToken, rb.AccessToken)
	}
}
