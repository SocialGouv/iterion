package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// TestAllTeams_walksEveryPage: a walk over every team crosses page
// boundaries — none lost, none twice.
func TestAllTeams_walksEveryPage(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 2*allTeamsPageSize + 3
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("team-%04d", i)
		if _, err := m.CreateTeam(ctx, Team{ID: slug, Name: slug, Slug: slug, CreatedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	teams, err := AllTeams(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tm := range teams {
		if seen[tm.ID] {
			t.Fatalf("team %s walked twice", tm.ID)
		}
		seen[tm.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("walked %d teams, want %d", len(seen), n)
	}
}

// deletingTeamLister serves ListTeams from a MemoryStore and deletes one
// team once the first page is served — a team removed while a sweep or the
// pool DELETE guard walks.
type deletingTeamLister struct {
	m      *MemoryStore
	once   sync.Once
	victim string
}

func (d *deletingTeamLister) ListTeams(ctx context.Context, p Page) ([]Team, error) {
	page, err := d.m.ListTeams(ctx, p)
	if p.Offset == 0 {
		d.once.Do(func() {
			if derr := d.m.DeleteTeam(ctx, d.victim); derr != nil {
				panic(derr)
			}
		})
	}
	return page, err
}

// TestAllTeams_aDeletionMidWalkMissesNoLiveTeam: an earlier team deleted
// between two page reads shifts every later team back one row; the
// overlapping pages still read the team that sat on the boundary.
func TestAllTeams_aDeletionMidWalkMissesNoLiveTeam(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = allTeamsPageSize + 5
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("team-%04d", i)
		if _, err := m.CreateTeam(ctx, Team{ID: slug, Name: slug, Slug: slug, CreatedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	teams, err := AllTeams(ctx, &deletingTeamLister{m: m, victim: "team-0000"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tm := range teams {
		seen[tm.ID] = true
	}
	live, _ := m.ListTeams(ctx, Page{Limit: 10 * n})
	for _, tm := range live {
		if !seen[tm.ID] {
			t.Errorf("AllTeams missed the live team %s (walked %d, live %d)", tm.ID, len(teams), len(live))
		}
	}
}

// TestAllTeams_teamsTiedOnCreatedAt: a created_at tie straddling a page
// boundary — the id breaks it, so every walk reads each team once.
func TestAllTeams_teamsTiedOnCreatedAt(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = allTeamsPageSize + 1
	for i := 0; i < n; i++ {
		slug := fmt.Sprintf("team-%04d", i)
		if _, err := m.CreateTeam(ctx, Team{ID: slug, Name: slug, Slug: slug, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for round := 0; round < 20; round++ {
		// One page at a time, the way AllTeams reads them — the ties must
		// sit at the same rows in every query.
		var walked []string
		for offset := 0; offset < n; offset += allTeamsPageSize {
			page, err := m.ListTeams(ctx, Page{Offset: offset, Limit: allTeamsPageSize})
			if err != nil {
				t.Fatal(err)
			}
			for _, tm := range page {
				walked = append(walked, tm.ID)
			}
		}
		seen := map[string]bool{}
		for _, id := range walked {
			seen[id] = true
		}
		if len(walked) != n || len(seen) != n {
			t.Fatalf("round %d: plain pages over %d tied teams read %d rows, %d distinct", round, n, len(walked), len(seen))
		}
	}
}
