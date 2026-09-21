package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

func seedUser(t *testing.T, s *Server, u identity.User) identity.User {
	t.Helper()
	if u.Status == "" {
		u.Status = identity.UserStatusActive
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now().UTC()
	}
	got, err := s.authStore().CreateUser(context.Background(), u)
	if err != nil {
		t.Fatalf("seed user %s: %v", u.ID, err)
	}
	return got
}

func detailFor(t *testing.T, s *Server, userID string) adminUserDetailView {
	t.Helper()
	u, err := s.authStore().GetUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("get user %s: %v", userID, err)
	}
	v, err := s.buildAdminUserDetail(context.Background(), u)
	if err != nil {
		t.Fatalf("buildAdminUserDetail(%s): %v", userID, err)
	}
	return v
}

// TestAdminUserDetail_ReportsGrantsNotReachability is the witness for the
// one mistake this view exists to avoid.
//
// buildOrgTree answers "what can this user reach", and to do so it hands an
// org admin an implied `admin` role on EVERY team of their org — see
// TestBuildOrgTree_BulkLookup, which pins that behaviour deliberately. An
// admin console rendering that as the account's memberships would show a
// team grant where none was ever made, and an operator removing "the grant"
// would find nothing to remove.
//
// Re-wiring buildOrgTree into buildAdminUserDetail is the mutation this
// reddens.
func TestAdminUserDetail_ReportsGrantsNotReachability(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedOrg(t, s, "o1", "org-one")
	for i, tm := range []identity.Team{
		{ID: "ta", OrgID: "o1", Name: "TA", Slug: "ta"},
		{ID: "tb", OrgID: "o1", Name: "TB", Slug: "tb"},
	} {
		tm.CreatedAt = now.Add(time.Duration(i) * time.Second)
		if _, err := s.authStore().CreateTeam(ctx, tm); err != nil {
			t.Fatalf("seed team %s: %v", tm.ID, err)
		}
	}
	seedUser(t, s, identity.User{ID: "u1", Email: "u1@example.org"})
	// Org admin of o1 — which makes every team of o1 REACHABLE...
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "u1", OrgID: "o1", Role: identity.OrgRoleAdmin, JoinedAt: now,
	}); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}
	// ...but granted on ta only.
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u1", TeamID: "ta", Role: identity.RoleMember, JoinedAt: now,
	}); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}

	// Guard the premise: buildOrgTree really does surface the ungranted
	// team. Without this the assertion below could pass for the wrong
	// reason — a tree that happened to be empty proves nothing.
	tree, err := s.buildOrgTree(ctx, "u1")
	if err != nil {
		t.Fatalf("buildOrgTree: %v", err)
	}
	if len(tree) != 1 || len(tree[0].Teams) != 2 {
		t.Fatalf("premise broken: buildOrgTree no longer implies ungranted teams: %+v", tree)
	}

	got := detailFor(t, s, "u1")
	if len(got.Teams) != 1 {
		t.Fatalf("got %d team rows, want 1 (only the GRANTED team): %+v", len(got.Teams), got.Teams)
	}
	if got.Teams[0].TeamID != "ta" || got.Teams[0].Role != "member" {
		t.Fatalf("granted team row wrong: %+v", got.Teams[0])
	}
	if len(got.Orgs) != 1 || got.Orgs[0].OrgID != "o1" || got.Orgs[0].Role != "admin" {
		t.Fatalf("org row wrong: %+v", got.Orgs)
	}
}

// TestAdminUserDetail_SubmitterSignature pins the shape of the account that
// motivated the whole console: a GitHub login outside the SSO allow-list is
// provisioned active, with an SSO link, NO password, and an empty roster.
// Read as four separate facts it is indistinguishable from a broken
// deployment; read together it is a diagnosis.
func TestAdminUserDetail_SubmitterSignature(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedUser(t, s, identity.User{ID: "sub1", Email: "someone@externes.example.org", Name: "someone"})
	if err := s.authStore().UpsertOIDCLink(ctx, identity.OIDCLink{
		Provider: "github", ProviderUserID: "1234567", UserID: "sub1",
		Email: "someone@externes.example.org", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed oidc link: %v", err)
	}

	got := detailFor(t, s, "sub1")
	if got.HasPassword {
		t.Fatalf("a submitter has no password: %+v", got)
	}
	if len(got.Orgs) != 0 || len(got.Teams) != 0 {
		t.Fatalf("a submitter has an empty roster: orgs=%+v teams=%+v", got.Orgs, got.Teams)
	}
	if len(got.SSOLinks) != 1 {
		t.Fatalf("got %d sso links, want 1: %+v", len(got.SSOLinks), got.SSOLinks)
	}
	if got.SSOLinks[0].Provider != "github" || got.SSOLinks[0].Subject != "1234567" {
		t.Fatalf("sso link row wrong: %+v", got.SSOLinks[0])
	}
	if got.User.Status != string(identity.UserStatusActive) {
		t.Fatalf("a submitter is ACTIVE — that is the confusing part: %+v", got.User)
	}
}

// TestAdminUserDetail_HasPasswordNeverShipsTheHash asserts the derived
// boolean tracks the stored hash and that the hash itself is absent from
// the view. UserView carries `json:"-"` on PasswordHash; this holds the
// property at the level a reader of this endpoint cares about.
func TestAdminUserDetail_HasPasswordNeverShipsTheHash(t *testing.T) {
	s := newOrgTestServer(t)

	seedUser(t, s, identity.User{ID: "pw1", Email: "pw1@example.org", PasswordHash: "$2a$10$notarealhash"})
	seedUser(t, s, identity.User{ID: "pw0", Email: "pw0@example.org"})

	if got := detailFor(t, s, "pw1"); !got.HasPassword {
		t.Fatalf("an account with a stored hash must report has_password")
	}
	if got := detailFor(t, s, "pw0"); got.HasPassword {
		t.Fatalf("an account with no stored hash must not report has_password")
	}

	// Marshal the view exactly as the handler ships it: a field that is
	// only absent from the struct we read in Go could still be added back
	// by a future embed, and the wire is where that shows.
	raw, err := json.Marshal(detailFor(t, s, "pw1"))
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	for _, forbidden := range []string{"notarealhash", "password_hash", "PasswordHash"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the response carries %q: %s", forbidden, raw)
		}
	}
}

// TestAdminUserDetail_OrphanGrantIsNamed covers the drift the console
// exists to surface: a team grant whose org membership is missing. The
// invariant says it cannot happen; a console that quietly dropped the row
// (or left its org unnamed) would hide the one case worth opening the page
// for.
func TestAdminUserDetail_OrphanGrantIsNamed(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedOrg(t, s, "o9", "org-nine")
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t9", OrgID: "o9", Name: "T9", Slug: "t9", CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	seedUser(t, s, identity.User{ID: "u9", Email: "u9@example.org"})
	// A team grant with NO matching org membership.
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "u9", TeamID: "t9", Role: identity.RoleAdmin, JoinedAt: now,
	}); err != nil {
		t.Fatalf("seed team membership: %v", err)
	}

	got := detailFor(t, s, "u9")
	if len(got.Teams) != 1 {
		t.Fatalf("the orphan grant must still render: %+v", got.Teams)
	}
	if !got.Teams[0].OrphanGrant {
		t.Fatalf("the orphan grant must be NAMED as one: %+v", got.Teams[0])
	}
	// And its org is resolved even though the user is not a member of it —
	// resolving only the orgs from the membership list would leave this
	// blank, which is the row an operator most needs to read.
	if got.Teams[0].OrgID != "o9" || got.Teams[0].OrgName != "o9" {
		t.Fatalf("the orphan grant's org must be resolved: %+v", got.Teams[0])
	}

	// A dangling reference must arrive ABSENT, not empty. A client renders
	// `name ?? id`, and `""` is not nullish — shipping the empty string turns
	// the one row this console exists to surface into a blank cell with no
	// name, no slug and no id. Removing `omitempty` reddens this.
	{
		s2 := newOrgTestServer(t)
		seedUser(t, s2, identity.User{ID: "ud", Email: "ud@example.org"})
		if err := s2.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
			UserID: "ud", OrgID: "o-vanished", Role: identity.OrgRoleMember, JoinedAt: now,
		}); err != nil {
			t.Fatalf("seed dangling org membership: %v", err)
		}
		raw, err := json.Marshal(detailFor(t, s2, "ud"))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var wire struct {
			Orgs []map[string]any `json:"orgs"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(wire.Orgs) != 1 {
			t.Fatalf("the dangling membership must still render: %s", raw)
		}
		for _, k := range []string{"org_name", "org_slug"} {
			if v, present := wire.Orgs[0][k]; present {
				t.Fatalf("%s must be ABSENT when unknown, got %q: %s", k, v, raw)
			}
		}
		if wire.Orgs[0]["org_id"] != "o-vanished" {
			t.Fatalf("the id is the fact and must survive: %s", raw)
		}
	}

	// A grant that DOES have its org membership must not be flagged — a
	// marker that is always on names nothing.
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "u9", OrgID: "o9", Role: identity.OrgRoleMember, JoinedAt: now,
	}); err != nil {
		t.Fatalf("seed org membership: %v", err)
	}
	if again := detailFor(t, s, "u9"); again.Teams[0].OrphanGrant {
		t.Fatalf("a grant backed by an org membership is not an orphan: %+v", again.Teams[0])
	}
}

// TestAdminUserDetail_OrderIsStable holds the row order as a decision
// rather than as an accident. Both stores sort these lists by JoinedAt and
// leave ties to a map walk, so seeding every membership at the SAME instant
// is precisely the case where an unsorted view would flap between reads.
func TestAdminUserDetail_OrderIsStable(t *testing.T) {
	s := newOrgTestServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedOrg(t, s, "oB", "org-b")
	seedOrg(t, s, "oA", "org-a")
	seedUser(t, s, identity.User{ID: "uo", Email: "uo@example.org"})
	for _, orgID := range []string{"oB", "oA"} {
		if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
			UserID: "uo", OrgID: orgID, Role: identity.OrgRoleMember, JoinedAt: now,
		}); err != nil {
			t.Fatalf("seed org membership %s: %v", orgID, err)
		}
	}
	for i, tm := range []identity.Team{
		{ID: "t-b1", OrgID: "oB", Name: "Zeta", Slug: "t-b1"},
		{ID: "t-a1", OrgID: "oA", Name: "Beta", Slug: "t-a1"},
		{ID: "t-a2", OrgID: "oA", Name: "Alpha", Slug: "t-a2"},
	} {
		tm.CreatedAt = now.Add(time.Duration(i) * time.Second)
		if _, err := s.authStore().CreateTeam(ctx, tm); err != nil {
			t.Fatalf("seed team %s: %v", tm.ID, err)
		}
		if err := s.authStore().UpsertMembership(ctx, identity.Membership{
			UserID: "uo", TeamID: tm.ID, Role: identity.RoleMember, JoinedAt: now,
		}); err != nil {
			t.Fatalf("seed team membership %s: %v", tm.ID, err)
		}
	}

	// Ten reads of the same unchanged state must agree — one read cannot
	// tell a stable order from a lucky map walk.
	want := []string{"oA", "oB"}
	wantTeams := []string{"t-a2", "t-a1", "t-b1"} // (org name, team name) asc
	for i := 0; i < 10; i++ {
		got := detailFor(t, s, "uo")
		for j, id := range want {
			if got.Orgs[j].OrgID != id {
				t.Fatalf("read %d: org order %v, want %v", i, ids(got.Orgs), want)
			}
		}
		for j, id := range wantTeams {
			if got.Teams[j].TeamID != id {
				t.Fatalf("read %d: team order %v, want %v", i, teamIDs(got.Teams), wantTeams)
			}
		}
	}
}

func ids(rows []adminUserOrgView) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.OrgID)
	}
	return out
}

func teamIDs(rows []adminUserTeamView) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.TeamID)
	}
	return out
}
