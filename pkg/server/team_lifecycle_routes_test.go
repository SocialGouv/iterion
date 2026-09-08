package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// Team lifecycle. The three properties that make it safe: a suspension is
// an ORG decision (a team cannot un-govern itself), a delete refuses a team
// that still owns anything (a stranded forge integration keeps firing into
// a tenant nothing can reach), and placing a member requires the org
// membership that a team grant sits inside.

func teamReq(ctx context.Context, method, path, body, teamID, userID string) *http.Request {
	r := orgReq(ctx, method, path, body, teamID)
	if userID != "" {
		r.SetPathValue("user_id", userID)
	}
	return r
}

// A team admin may rename their team; a naming mistake used to be permanent.
func TestTeamLifecycle_teamAdminRenames(t *testing.T) {
	s := newOrgCredsTestServer(t)

	w := httptest.NewRecorder()
	s.handleUpdateTeam(w, teamReq(teamAdminCtx(), "PATCH", "/api/teams/t1", `{"name":"QE","slug":"qe"}`, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("rename: code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := s.authStore().GetTeam(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "QE" || got.Slug != "qe" {
		t.Fatalf("team = %q/%q, want QE/qe", got.Name, got.Slug)
	}
}

// Suspension is an ORG decision. A team admin resuming a team their org
// suspended would undo a governance action from inside the thing being
// governed.
func TestTeamLifecycle_teamAdminCannotSetStatus(t *testing.T) {
	s := newOrgCredsTestServer(t)

	w := httptest.NewRecorder()
	s.handleSetTeamStatus(w, teamReq(teamAdminCtx(), "POST", "/api/teams/t1/status", `{"status":"active"}`, "t1", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status as team admin: code=%d body=%s, want 403", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleSetTeamStatus(w, teamReq(orgAdminCtx(), "POST", "/api/teams/t1/status", `{"status":"suspended","reason":"migration"}`, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status as org admin: code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := s.authStore().GetTeam(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Suspended() {
		t.Fatalf("team status = %q, want suspended — the launch gate reads this field", got.EffectiveStatus())
	}
	if got.CanLaunch() {
		t.Error("a suspended team still reports CanLaunch")
	}
}

// pending_deletion is the ORG soft-delete state and has no team-level
// sweeper. Accepting it would park a team in a state nothing resolves.
func TestTeamLifecycle_statusRefusesPendingDeletion(t *testing.T) {
	s := newOrgCredsTestServer(t)

	w := httptest.NewRecorder()
	s.handleSetTeamStatus(w, teamReq(orgAdminCtx(), "POST", "/api/teams/t1/status", `{"status":"pending_deletion"}`, "t1", ""))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("pending_deletion: code=%d body=%s, want 400", w.Code, w.Body.String())
	}
}

// A team that still owns a repo integration cannot be deleted: its webhook
// would keep firing into a tenant nothing can reach. The refusal must NAME
// what is left, or the operator has no next step.
func TestTeamLifecycle_deleteRefusesANonEmptyTeam(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.forgeIntegrations = forge.NewMemoryRepoIntegrationStore()
	if err := s.forgeIntegrations.Create(withTenantCtx("t1"), forge.RepoIntegration{
		ID: "ri1", TenantID: "t1", ConnectionID: "c1", RepoFullName: "org/repo", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1", "", "t1", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete a non-empty team: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "repo integration") {
		t.Errorf("the refusal does not name what is left: %s", body)
	}
	if _, err := s.authStore().GetTeam(context.Background(), "t1"); err != nil {
		t.Fatalf("the team was deleted despite the refusal: %v", err)
	}
}

// An empty team deletes, and its memberships go with it — a team row
// removed while its grants survive leaves members pointing at a team that
// no longer resolves.
func TestTeamLifecycle_deleteAnEmptyTeamRevokesItsGrants(t *testing.T) {
	s := newOrgCredsTestServer(t)
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: "orgadmin", TeamID: "t2", Role: identity.RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete an empty team: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if _, err := s.authStore().GetTeam(context.Background(), "t2"); err == nil {
		t.Fatal("the team survived a 204 delete")
	}
	if _, err := s.authStore().GetMembership(context.Background(), "orgadmin", "t2"); err == nil {
		t.Error("a membership survived its team — the holder now points at a team that does not resolve")
	}
}

// Placing an existing user is the direct counterpart of the email
// invitation. It must still require the ORG membership: creating one
// silently would let a team admin pull a stranger into their org.
func TestTeamLifecycle_putMemberRequiresOrgMembership(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "outsider", Email: "outsider@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/outsider", `{"role":"member"}`, "t1", "outsider"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("place a non-org-member: code=%d body=%s, want 422", w.Code, w.Body.String())
	}
	if _, err := s.authStore().GetMembership(ctx, "outsider", "t1"); err == nil {
		t.Fatal("a team grant was created for a user outside the org")
	}

	// Once they are in the org, the placement lands — no email round trip.
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "outsider", OrgID: "o1", Role: identity.OrgRoleMember,
	}); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/outsider", `{"role":"member"}`, "t1", "outsider"))
	if w.Code != http.StatusOK {
		t.Fatalf("place an org member: code=%d body=%s", w.Code, w.Body.String())
	}
	mb, err := s.authStore().GetMembership(ctx, "outsider", "t1")
	if err != nil || mb.Role != identity.RoleMember {
		t.Fatalf("membership = %+v (%v), want role member", mb, err)
	}
}

// A disabled account must not be granted a team: the grant would sit there
// waiting for a login that is refused, and read as access nobody has.
func TestTeamLifecycle_putMemberRefusesADisabledUser(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "gone", Email: "gone@example.com", Status: identity.UserStatusDisabled, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "gone", OrgID: "o1", Role: identity.OrgRoleMember,
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/gone", `{"role":"member"}`, "t1", "gone"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("place a disabled user: code=%d body=%s, want 422", w.Code, w.Body.String())
	}
}
