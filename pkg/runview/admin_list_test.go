package runview

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// adminSeed is one run shaped for the cross-tenant listing: an
// explicit tenant (the team id), an optional sovereign pool stamp, and
// a CreatedAt the keyset order can rely on.
type adminSeed struct {
	id, team, pool, status string
	at                     time.Time
}

func seedAdminRuns(t *testing.T, st store.RunStore, seeds []adminSeed) {
	t.Helper()
	for _, s := range seeds {
		r := &store.Run{
			ID:           s.id,
			Name:         "run " + s.id,
			WorkflowName: "wf-" + s.id,
			Status:       store.RunStatus(s.status),
			TenantID:     s.team,
			RunnerPool:   s.pool,
			CreatedAt:    s.at,
			UpdatedAt:    s.at,
		}
		if err := st.SaveRun(context.Background(), r); err != nil {
			t.Fatalf("seed run %s: %v", s.id, err)
		}
	}
}

func newAdminListService(t *testing.T, dir string) *Service {
	t.Helper()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService(%q): %v", dir, err)
	}
	return svc
}

// adminSeeds spreads runs across three teams of two orgs, stamps one
// to a sovereign pool, and TIES two of them on the same instant — the
// keyset must survive the tie through its id break.
func adminSeeds(base time.Time) []adminSeed {
	return []adminSeed{
		{"run-a", "team-a1", "", "finished", base},
		{"run-d", "team-a1", "", "queued", base}, // tied with run-a
		{"run-b", "team-a2", "pool-souverain", "failed", base.Add(time.Second)},
		{"run-c", "team-a1", "", "finished", base.Add(2 * time.Second)},
		{"run-e", "team-b1", "", "finished", base.Add(3 * time.Second)},
	}
}

// TestListAcrossTenantsPaginatesByKeyset walks the listing two rows at
// a time: every seeded run comes back EXACTLY once, the walk ends on
// an empty next token, and the total does not depend on how many pages
// it took. The assertion is set-shaped on purpose — the fact is "all
// seen once", never "the first page held".
func TestListAcrossTenantsPaginatesByKeyset(t *testing.T) {
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	seedAdminRuns(t, st, adminSeeds(base))
	svc := newAdminListService(t, dir)

	seen := map[string]bool{}
	cursor := RunCursor{}
	pages := 0
	for {
		page, next, err := svc.ListAcrossTenants(context.Background(), AdminListFilter{Limit: 2, After: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, su := range page {
			if seen[su.ID] {
				t.Fatalf("run %s seen twice across pages (page %d)", su.ID, pages)
			}
			seen[su.ID] = true
		}
		pages++
		if next == "" {
			break
		}
		if pages > 10 {
			t.Fatalf("pagination never terminates: %d pages", pages)
		}
		cursor, err = DecodeRunCursor(next)
		if err != nil {
			t.Fatalf("decode next cursor: %v", err)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("walked %d unique runs across %d pages, want all 5 (seen: %v)", len(seen), pages, seen)
	}
}

// TestListAcrossTenantsFiltersCombine: team narrowing and the status
// filter COMBINE — every axis applies, a team with no matching run
// answers an empty page (never a fallback), and the sovereign-pool
// stamp rides the row.
func TestListAcrossTenantsFiltersCombine(t *testing.T) {
	base := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	seedAdminRuns(t, st, adminSeeds(base))
	svc := newAdminListService(t, dir)
	ctx := context.Background()

	all, _, err := svc.ListAcrossTenants(ctx, AdminListFilter{})
	if err != nil {
		t.Fatalf("unfiltered list: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("unfiltered list returned %d runs, want the 5 seeded across all tenants", len(all))
	}

	teamA1, _, err := svc.ListAcrossTenants(ctx, AdminListFilter{TeamIDs: []string{"team-a1"}})
	if err != nil {
		t.Fatalf("team filter: %v", err)
	}
	if len(teamA1) != 3 {
		t.Fatalf("team-a1 filter returned %d rows, want its 3 runs", len(teamA1))
	}
	for _, su := range teamA1 {
		if su.TenantID != "team-a1" {
			t.Fatalf("team filter leaked %s (team %s)", su.ID, su.TenantID)
		}
	}

	combined, _, err := svc.ListAcrossTenants(ctx, AdminListFilter{TeamIDs: []string{"team-a2"}, Status: store.RunStatus("failed")})
	if err != nil {
		t.Fatalf("combined filter: %v", err)
	}
	if len(combined) != 1 || combined[0].ID != "run-b" {
		t.Fatalf("team+status combined returned %v, want exactly run-b", idsOf(combined))
	}
	if combined[0].TenantID != "team-a2" || combined[0].RunnerPool != "pool-souverain" {
		t.Fatalf("row identity lost: team %q pool %q", combined[0].TenantID, combined[0].RunnerPool)
	}

	empty, _, err := svc.ListAcrossTenants(ctx, AdminListFilter{TeamIDs: []string{"team-a2"}, Status: store.RunStatus("finished")})
	if err != nil {
		t.Fatalf("empty-combination list: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("team-a2 + finished answered %d rows, want an empty page (no fallback)", len(empty))
	}
}

// TestRunCursorRoundTrip: the opaque token survives itself, the empty
// token is "no position", and a malformed token is an ERROR — never
// silently read as "from the top", which would replay page one
// forever.
func TestRunCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 27, 12, 30, 0, 12345, time.UTC)
	c := RunCursor{CreatedAt: at, ID: "run/with:chars-é"}
	dec, err := DecodeRunCursor(EncodeRunCursor(c))
	if err != nil {
		t.Fatalf("decode encoded cursor: %v", err)
	}
	if !dec.CreatedAt.Equal(at) || dec.ID != c.ID {
		t.Fatalf("round trip lost the position: got %+v want %+v", dec, c)
	}
	if got, err := DecodeRunCursor(""); err != nil || got != (RunCursor{}) {
		t.Fatalf("empty token: got %+v err %v, want the zero cursor", got, err)
	}
	for _, bad := range []string{
		"!!", // not base64 at all
		base64.RawURLEncoding.EncodeToString([]byte("no-separator")),
		base64.RawURLEncoding.EncodeToString([]byte("abc|def")), // separator ok, timestamp garbage
	} {
		if _, err := DecodeRunCursor(bad); err == nil {
			t.Fatalf("malformed cursor %q decoded without error", bad)
		}
	}
}

func idsOf(summaries []RunSummary) []string {
	out := make([]string, 0, len(summaries))
	for _, su := range summaries {
		out = append(out, su.ID)
	}
	return out
}
