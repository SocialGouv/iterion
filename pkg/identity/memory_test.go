package identity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMemoryStore_UserCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	now := time.Now().UTC()
	u, err := s.CreateUser(ctx, User{
		ID:        "u1",
		Email:     "  Alice@Example.COM  ",
		Status:    UserStatusActive,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if u.Email != "alice@example.com" {
		t.Fatalf("email not normalized: %q", u.Email)
	}

	if _, err := s.CreateUser(ctx, User{ID: "u2", Email: "alice@example.com"}); !errors.Is(err, ErrEmailAlreadyTaken) {
		t.Fatalf("expected ErrEmailAlreadyTaken, got %v", err)
	}

	got, err := s.GetUserByEmail(ctx, "ALICE@example.com")
	if err != nil || got.ID != "u1" {
		t.Fatalf("GetUserByEmail mismatch: %v %s", err, got.ID)
	}

	got.Name = "Alice"
	if err := s.UpdateUser(ctx, got); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	got2, _ := s.GetUser(ctx, "u1")
	if got2.Name != "Alice" {
		t.Fatalf("update lost: %v", got2)
	}

	// Email rename collision detection.
	_, _ = s.CreateUser(ctx, User{ID: "u3", Email: "bob@example.com"})
	got2.Email = "bob@example.com"
	if err := s.UpdateUser(ctx, got2); !errors.Is(err, ErrEmailAlreadyTaken) {
		t.Fatalf("expected collision on rename, got %v", err)
	}
}

func TestMemoryStore_TeamSlugUnique(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if _, err := s.CreateTeam(ctx, Team{ID: "t1", Slug: "acme"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateTeam(ctx, Team{ID: "t2", Slug: "acme"}); !errors.Is(err, ErrSlugAlreadyTaken) {
		t.Fatalf("expected ErrSlugAlreadyTaken, got %v", err)
	}
}

func TestMemoryStore_Memberships(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.UpsertMembership(ctx, Membership{UserID: "u1", TeamID: "t1", Role: RoleAdmin, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertMembership(ctx, Membership{UserID: "u1", TeamID: "t2", Role: RoleViewer, JoinedAt: time.Now().Add(time.Second)}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, _ := s.ListMembershipsByUser(ctx, "u1")
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}

	// Role-rejection on invalid value.
	if err := s.UpsertMembership(ctx, Membership{UserID: "x", TeamID: "y", Role: "wat"}); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("expected ErrInvalidRole, got %v", err)
	}

	// Promote on second upsert.
	if err := s.UpsertMembership(ctx, Membership{UserID: "u1", TeamID: "t1", Role: RoleOwner, JoinedAt: time.Now()}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	mb, _ := s.GetMembership(ctx, "u1", "t1")
	if mb.Role != RoleOwner {
		t.Fatalf("promotion lost: %v", mb.Role)
	}
}

func TestRoleAtLeast(t *testing.T) {
	cases := []struct {
		have Role
		want Role
		ok   bool
	}{
		{RoleOwner, RoleAdmin, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleMember, RoleAdmin, false},
		{RoleViewer, RoleViewer, true},
		{Role("wat"), RoleViewer, false},
	}
	for _, c := range cases {
		if got := c.have.AtLeast(c.want); got != c.ok {
			t.Errorf("Role(%q).AtLeast(%q) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestSlugifyTeamName(t *testing.T) {
	cases := map[string]string{
		"Acme Corp":           "acme-corp",
		"  My Personal Team ": "my-personal-team",
		"Hello/World!":        "helloworld",
		"---weird---":         "weird",
		"":                    "",
	}
	for in, want := range cases {
		if got := SlugifyTeamName(in); got != want {
			t.Errorf("SlugifyTeamName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The one-team-per-pool invariant at the store level: the mapping write
// itself refuses when another team holds the pool — the backstop that
// closes the route's check-then-act window (D13, R81a941).
func TestPatchTeam_RunnerPoolHeldByAnotherTeam(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateTeam(ctx, Team{ID: "t1", Name: "t1", Slug: "t1", OrgID: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTeam(ctx, Team{ID: "t2", Name: "t2", Slug: "t2", OrgID: "o1"}); err != nil {
		t.Fatal(err)
	}
	pool := "honorabilite"
	if _, err := m.PatchTeam(ctx, "t1", TeamPatch{RunnerPool: &pool}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PatchTeam(ctx, "t2", TeamPatch{RunnerPool: &pool}); !errors.Is(err, ErrRunnerPoolHeld) {
		t.Fatalf("second team onto a held pool: err = %v, want ErrRunnerPoolHeld", err)
	}
	// Re-stamping the holder itself passes (idempotent re-map).
	if _, err := m.PatchTeam(ctx, "t1", TeamPatch{RunnerPool: &pool}); err != nil {
		t.Fatalf("holder re-map refused: %v", err)
	}
}

// CreateTeam enforces the same invariant — a team cannot be BORN
// holding a pool another team already holds.
func TestCreateTeam_RunnerPoolHeldByAnotherTeam(t *testing.T) {
	m := NewMemoryStore()
	ctx := context.Background()
	if _, err := m.CreateTeam(ctx, Team{ID: "t1", Name: "t1", Slug: "t1", OrgID: "o1", RunnerPool: "honorabilite"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTeam(ctx, Team{ID: "t2", Name: "t2", Slug: "t2", OrgID: "o1", RunnerPool: "honorabilite"}); !errors.Is(err, ErrRunnerPoolHeld) {
		t.Fatalf("err = %v, want ErrRunnerPoolHeld", err)
	}
}

// The index pre-flight's grouping (Rce7ebd): duplicates are named, a
// clean fleet passes.
func TestDuplicatePoolHolders(t *testing.T) {
	err := duplicatePoolHolders([]Team{
		{ID: "a", Slug: "alpha", RunnerPool: "p"},
		{ID: "b", Slug: "beta", RunnerPool: "p"},
		{ID: "c", Slug: "gamma", RunnerPool: "q"},
	})
	if err == nil || !strings.Contains(err.Error(), `"p"`) || !strings.Contains(err.Error(), "a/alpha") {
		t.Fatalf("duplicate holders undiagnosed: %v", err)
	}
	if err := duplicatePoolHolders([]Team{
		{ID: "a", Slug: "alpha", RunnerPool: "p"},
		{ID: "c", Slug: "gamma", RunnerPool: "q"},
	}); err != nil {
		t.Fatalf("clean fleet refused: %v", err)
	}
}
