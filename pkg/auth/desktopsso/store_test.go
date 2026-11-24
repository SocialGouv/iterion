package desktopsso

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
)

func pendingLogin(userID, lineage, team string) auth.DesktopLogin {
	return auth.DesktopLogin{
		UserID:          userID,
		Root:            auth.SessionRoot{ID: lineage, Generation: 2},
		PreferredTeamID: team,
	}
}

func TestMemoryStore_SingleUse(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore(time.Minute)
	d := pendingLogin("u1", "lineage-1", "team-1")

	ticket, err := s.Mint(ctx, d)
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
	if got.UserID != "u1" || got.Root.ID != "lineage-1" || got.Root.Generation != 2 || got.PreferredTeamID != "team-1" {
		t.Errorf("redeemed login mismatch: %+v", got)
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
	ticket, err := s.Mint(ctx, pendingLogin("u1", "lineage-1", ""))
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
	a, _ := s.Mint(ctx, pendingLogin("u1", "lineage-a", ""))
	b, _ := s.Mint(ctx, pendingLogin("u2", "lineage-b", ""))
	if a == b {
		t.Fatal("two mints produced the same ticket")
	}
	ra, _ := s.Redeem(ctx, a)
	rb, _ := s.Redeem(ctx, b)
	if ra.Root.ID != "lineage-a" || rb.Root.ID != "lineage-b" {
		t.Errorf("tickets crossed: a=%+v b=%+v", ra, rb)
	}
}

// The witness #2635 asks for: what rests in the store is the ticket's HASH
// and the deferred login — the ticket itself is not in it, and no stored
// entry carries a credential (DesktopLogin has no token field; the marshal
// proves none rides along).
func TestMemoryStore_NothingRedeemableRestsInIt(t *testing.T) {
	s := NewMemoryStore(time.Minute)
	ticket, err := s.Mint(context.Background(), pendingLogin("u1", "lineage-1", "team-1"))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) != 1 {
		t.Fatalf("stored entries = %d, want 1", len(s.m))
	}
	for key, e := range s.m {
		if key == ticket || strings.Contains(key, ticket) {
			t.Errorf("the ticket itself is stored as the key: %q", key)
		}
		if key != ticketKey(ticket) {
			t.Errorf("stored key = %q, want the ticket's SHA-256 %q", key, ticketKey(ticket))
		}
		raw, err := json.Marshal(e.pending)
		if err != nil {
			t.Fatalf("marshal the stored entry: %v", err)
		}
		if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "secret") {
			t.Errorf("the stored entry carries a credential-shaped field: %s", raw)
		}
		if strings.Contains(string(raw), ticket) {
			t.Errorf("the stored entry holds the ticket itself: %s", raw)
		}
	}
}
