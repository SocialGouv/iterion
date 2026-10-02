package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// #2046: #1931 deliberately left an UNRESOLVABLE /api/teams/{id} spelling
// its old semantics — and for a super-admin canManageTeam passes over any
// spelling, so a mutating call keyed its rows by the ghost (a case-variant
// TEAM-A next to slug team-a), invisible to every UUID reader. No authz
// bypass — a member is 403 through the ghost either way — but the store
// split silently. The mutating team routes now refuse the ghost with a 404
// naming the spelling, at the same route-resolution chokepoint that
// canonicalizes the resolvable spellings; read routes keep the ghost
// semantics. These are the witnesses.

// A super-admin's POST/DELETE/PATCH through a spelling that resolves to NO
// team is a 404 that names the spelling, and nothing reaches the store —
// before the refusal the POST answered 200 and keyed the secret by TEAM-A.
func TestGhostTeamMutatingRouteIsRefusedAndWritesNothing(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})

	code, raw := teamCall(t, ts, "POST", "/api/teams/TEAM-A/secrets", sre, `{"name":"ghost_probe","secret":"s3cr3t-value"}`)
	if code != http.StatusNotFound {
		t.Fatalf("super-admin POST through the ghost spelling: %d, want 404 (%s)", code, raw)
	}
	if !strings.Contains(string(raw), "TEAM-A") {
		t.Fatalf("the refusal does not name the unknown spelling: %s", raw)
	}
	// The store holds no row keyed by the ghost spelling.
	rows, err := srv.genericSecrets.ListByTeam(context.Background(), "TEAM-A", "sre")
	if err != nil {
		t.Fatalf("list the ghost tenant: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a row was keyed by the ghost spelling: %+v", rows)
	}

	code, raw = teamCall(t, ts, "DELETE", "/api/teams/TEAM-A/secrets/whatever", sre, "")
	if code != http.StatusNotFound {
		t.Fatalf("super-admin DELETE through the ghost spelling: %d, want 404 (%s)", code, raw)
	}
	code, raw = teamCall(t, ts, "PATCH", "/api/teams/TEAM-A/secrets/whatever", sre, `{"secret":"n3w-value"}`)
	if code != http.StatusNotFound {
		t.Fatalf("super-admin PATCH through the ghost spelling: %d, want 404 (%s)", code, raw)
	}
}

// The same ghost spelling on a READ route keeps the semantics #1931
// preserved: a super-admin's list of a ghost team is the empty 200 it
// always was.
func TestGhostTeamReadRouteKeepsGhostSemantics(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})
	if code, raw := teamCall(t, ts, "GET", "/api/teams/TEAM-A/secrets", sre, ""); code != http.StatusOK {
		t.Fatalf("a read through the ghost spelling changed semantics: %d %s", code, raw)
	}
}

// A caller the ghost's authz rejects keeps the handler's 403 on a mutating
// route: the 404 is reserved to callers who would otherwise PASS, so it is
// no existence oracle over team spellings (403 both ways for everyone else).
func TestGhostTeamMutatingRouteKeepsTheMemberForbidden(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	stranger := bearer(t, srv, caller("u-member-b", "tenant-B"))
	if code, raw := teamCall(t, ts, "POST", "/api/teams/TEAM-A/secrets", stranger, `{"name":"x","secret":"y"}`); code != http.StatusForbidden {
		t.Fatalf("a member without standing through the ghost spelling: %d, want 403 (%s)", code, raw)
	}
}

// The refusal changes nothing for a spelling that DOES resolve: the slug
// still canonicalizes to the UUID and the write lands keyed by the team.
func TestResolvableSpellingMutatingRouteIsUnchanged(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})
	code, raw := teamCall(t, ts, "POST", "/api/teams/team-a/secrets", sre, `{"name":"real_probe","secret":"s3cr3t-value"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("a resolvable spelling refused: %d %s", code, raw)
	}
	rows, err := srv.genericSecrets.ListByTeam(context.Background(), "tenant-A", "sre")
	if err != nil || len(rows) != 1 {
		t.Fatalf("the write did not key the canonical team: %v rows=%d", err, len(rows))
	}
}

// An identity-store OUTAGE is not a ghost: when both team lookups fail
// with something that is NOT ErrNotFound (a Mongo blip, not an answer),
// the chokepoint must NOT refuse a mutating call on a REAL team with the
// lying "unknown team" 404 — it defers, and the request reaches the
// handler. Same rule the run-scope lineage arms hold
// (TestRunByIDLineageOutageIsNotARefusal): a failed read is an error,
// never an absence.
func TestTeamLookupOutageIsNotTheGhostRefusal(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})

	// Wrap the seeded store so ONLY the two canonicalization lookups fail,
	// with an outage-class error; everything else (memberships, the generic
	// secrets store) answers normally.
	swapIdentityStore(t, srv, failingTeamLookup{
		Store:   srv.authStore(),
		teamErr: context.DeadlineExceeded,
		slugErr: context.DeadlineExceeded,
	})

	// A mutating call on the REAL team (by its slug, which the outage
	// hides) reaches the handler — the ghost 404 must not appear.
	code, raw := teamCall(t, ts, "POST", "/api/teams/team-a/secrets", sre, `{"name":"outage_probe","secret":"s3cr3t-value"}`)
	if code == http.StatusNotFound && strings.Contains(string(raw), "unknown team") {
		t.Fatalf("an outage was reported as a ghost team — the 404 names a healthy team as nonexistent: %d %s", code, raw)
	}
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("the call did not reach the handler: %d %s", code, raw)
	}
}

// Arm 2 of the gate: GetTeam answers a clean ErrNotFound but the SLUG
// read is the one down. The verdict must still defer — the error gate
// requires a clean not-found from BOTH lookups, so a suite that reverted
// the slug arm to "any error is a ghost" turns this red.
func TestTeamSlugLookupOutageIsNotTheGhostRefusal(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})
	swapIdentityStore(t, srv, failingTeamLookup{
		Store:   srv.authStore(),
		teamErr: identity.ErrNotFound,
		slugErr: context.DeadlineExceeded,
	})

	code, raw := teamCall(t, ts, "POST", "/api/teams/team-a/secrets", sre, `{"name":"outage_probe_2","secret":"s3cr3t-value"}`)
	if code == http.StatusNotFound && strings.Contains(string(raw), "unknown team") {
		t.Fatalf("a slug-read outage was reported as a ghost team: %d %s", code, raw)
	}
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("the call did not reach the handler: %d %s", code, raw)
	}
}

// The slug fallthrough survives an id-read outage (#2043's behavior, kept
// by the error gate): GetTeam times out — the Mongo-failover shape — but
// the slug read lands on the new primary, so {id} is still rewritten to
// the UUID and the row keys the canonical team. Reverting to "an id-read
// outage skips the slug lookup" leaves the raw slug in {id} and this
// turns red: the row would key team-a, invisible to tenant-A's readers.
func TestTeamIDOutageStillCanonicalizesTheSlug(t *testing.T) {
	srv, ts := newTeamSlugServer(t)
	sre := bearer(t, srv, auth.Identity{UserID: "sre", IsSuperAdmin: true})
	swapIdentityStore(t, srv, failingTeamLookup{
		Store:   srv.authStore(),
		teamErr: context.DeadlineExceeded,
	})

	code, raw := teamCall(t, ts, "POST", "/api/teams/team-a/secrets", sre, `{"name":"failover_probe","secret":"s3cr3t-value"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("create via slug during an id-read outage: %d %s", code, raw)
	}
	rows, err := srv.genericSecrets.ListByTeam(context.Background(), "tenant-A", "sre")
	if err != nil || len(rows) != 1 {
		t.Fatalf("the write did not key the canonical UUID: %v rows=%d", err, len(rows))
	}
	if rows, err := srv.genericSecrets.ListByTeam(context.Background(), "team-a", "sre"); err != nil || len(rows) != 0 {
		t.Fatalf("a row keyed by the raw slug coexists with the canonical one: %v rows=%d", err, len(rows))
	}
}

// swapIdentityStore rebuilds the server's auth service over st — the way
// a test arms a degraded identity store without re-seeding the fixture.
// The started httptest server closes over srv, so the swap is enough.
func swapIdentityStore(t *testing.T, srv *Server, st identity.Store) {
	t.Helper()
	svc, err := auth.NewService(auth.Config{
		Store:      st,
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     srv.signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("rebuild auth service over the failing store: %v", err)
	}
	srv.authSvc = svc
}

// failingTeamLookup degrades the two canonicalization lookups
// independently — a nil error delegates to the embedded store, a set one
// fails the call with it — so a test can arm each arm of the ghost gate:
// an outage (NOT ErrNotFound — same shape as run_scope_test's
// failingTeamRead), a clean not-found, or a healthy read.
type failingTeamLookup struct {
	identity.Store
	teamErr, slugErr error
}

func (d failingTeamLookup) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	if d.teamErr != nil {
		return identity.Team{}, d.teamErr
	}
	return d.Store.GetTeam(ctx, id)
}

func (d failingTeamLookup) GetTeamBySlug(ctx context.Context, slug string) (identity.Team, error) {
	if d.slugErr != nil {
		return identity.Team{}, d.slugErr
	}
	return d.Store.GetTeamBySlug(ctx, slug)
}
