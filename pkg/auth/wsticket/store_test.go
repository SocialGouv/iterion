package wsticket

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
)

func TestMemoryStore_SingleUse(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(time.Minute)
	id := auth.Identity{UserID: "u1", Email: "a@b.io", TeamID: "team-1", IsSuperAdmin: true}

	ticket, err := s.Mint(ctx, id)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := s.Redeem(ctx, ticket)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if got.UserID != "u1" || got.TeamID != "team-1" || !got.IsSuperAdmin {
		t.Errorf("redeemed identity mismatch: %+v", got)
	}
	// Single use.
	if _, err := s.Redeem(ctx, ticket); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("second redeem err = %v, want ErrTicketNotFound", err)
	}
	if _, err := s.Redeem(ctx, "nope"); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("unknown redeem err = %v, want ErrTicketNotFound", err)
	}
}

func TestMemoryStore_Expiry(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(time.Minute)
	s.ttl = -1 * time.Second // force already-expired entries
	ticket, err := s.Mint(ctx, auth.Identity{UserID: "u"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := s.Redeem(ctx, ticket); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("expired redeem err = %v, want ErrTicketNotFound", err)
	}
}
