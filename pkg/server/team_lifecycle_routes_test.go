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
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
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

// The org-level twin. Without it handlePutTeamMember cannot serve the case
// it exists for: a user with NO org at all is still reachable only by
// email, so the round trip is merely moved one level up.
func TestTeamLifecycle_putOrgMemberPlacesAnOrphanAccount(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	if _, err := s.authStore().CreateUser(ctx, identity.User{
		ID: "orphan", Email: "orphan@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// A team admin is not an org admin: the org roster is not theirs.
	w := httptest.NewRecorder()
	s.handlePutOrgMember(w, teamReq(teamAdminCtx(), "PUT", "/api/orgs/o1/members/orphan", `{"role":"member"}`, "o1", "orphan"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("place as team admin: code=%d body=%s, want 403", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handlePutOrgMember(w, teamReq(orgAdminCtx(), "PUT", "/api/orgs/o1/members/orphan", `{"role":"member"}`, "o1", "orphan"))
	if w.Code != http.StatusOK {
		t.Fatalf("place as org admin: code=%d body=%s", w.Code, w.Body.String())
	}
	om, err := s.authStore().GetOrgMembership(ctx, "orphan", "o1")
	if err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("org membership = %+v (%v), want role member", om, err)
	}

	// And now the team placement — the pair is what makes onboarding an
	// existing account a two-call operation instead of an email.
	w = httptest.NewRecorder()
	s.handlePutTeamMember(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/orphan", `{"role":"member"}`, "t1", "orphan"))
	if w.Code != http.StatusOK {
		t.Fatalf("team placement after the org one: code=%d body=%s", w.Code, w.Body.String())
	}
}

// The lost-update race, in the dangerous direction: a rename must not carry
// back the Status it read. With `UpdateTeam` (a whole-document ReplaceOne),
// renaming a team another admin suspended IN BETWEEN silently RESUMED it — a
// governance action undone by an unrelated edit, with nothing in the audit
// log saying so.
//
// Two sequential handler calls do NOT reproduce it (the second re-reads the
// already-suspended row and writes it back correctly). The window is between
// a handler's READ and its WRITE, so the test hands the handler a STALE
// snapshot — what a read taken before the suspension would have returned —
// while the store itself holds the suspended row. A handler that writes back
// what it read resumes the team; one that patches only the fields it owns
// cannot, because it never reads.
type staleTeamReadStore struct {
	identity.Store
	teamID string
	reads  int
}

func (r *staleTeamReadStore) GetTeam(ctx context.Context, id string) (identity.Team, error) {
	t, err := r.Store.GetTeam(ctx, id)
	if err != nil || id != r.teamID {
		return t, err
	}
	r.reads++
	// The snapshot as it was BEFORE the concurrent suspension landed.
	t.Status = identity.TeamStatusActive
	t.SuspendedAt, t.SuspendedBy, t.SuspendReason = nil, "", ""
	return t, nil
}

func TestTeamLifecycle_renameDoesNotRevertAConcurrentSuspension(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()

	// The suspension is the state of the world; the handler is about to be
	// told otherwise.
	w := httptest.NewRecorder()
	s.handleSetTeamStatus(w, teamReq(orgAdminCtx(), "POST", "/api/teams/t1/status", `{"status":"suspended","reason":"migration"}`, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("suspend: code=%d body=%s", w.Code, w.Body.String())
	}

	real := s.authStore()
	s.authSvc.SetStoreForTest(&staleTeamReadStore{Store: real, teamID: "t1"})

	w = httptest.NewRecorder()
	s.handleUpdateTeam(w, teamReq(teamAdminCtx(), "PATCH", "/api/teams/t1", `{"name":"QE"}`, "t1", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("rename: code=%d body=%s", w.Code, w.Body.String())
	}

	got, err := real.GetTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "QE" {
		t.Errorf("name = %q, want QE — the rename did not land", got.Name)
	}
	if !got.Suspended() {
		t.Fatalf("status = %q after a rename, want suspended — the rename wrote back a stale snapshot and resumed the team", got.EffectiveStatus())
	}
	if got.SuspendReason != "migration" {
		t.Errorf("suspend reason = %q, want it preserved across the rename", got.SuspendReason)
	}
}

// The store contract the fix rests on, asserted directly: a patch touches
// ONLY the fields it names.
func TestTeamLifecycle_patchTeamLeavesUnnamedFieldsAlone(t *testing.T) {
	st := identity.NewMemoryStore()
	ctx := context.Background()
	if _, err := st.CreateTeam(ctx, identity.Team{ID: "t", Name: "old", Slug: "old", OrgID: "o"}); err != nil {
		t.Fatal(err)
	}
	susp := identity.TeamStatusSuspended
	if _, err := st.PatchTeam(ctx, "t", identity.TeamPatch{Status: &susp, SuspendedBy: "admin", SuspendReason: "why"}); err != nil {
		t.Fatal(err)
	}
	name := "new"
	got, err := st.PatchTeam(ctx, "t", identity.TeamPatch{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "new" || got.Slug != "old" {
		t.Errorf("name/slug = %q/%q, want new/old", got.Name, got.Slug)
	}
	if !got.Suspended() || got.SuspendReason != "why" {
		t.Errorf("a name-only patch moved the suspension: %+v", got)
	}
	// And resuming clears the trio — a resumed team keeping a SuspendedAt is
	// how "who suspended this, and when" stops having an answer.
	active := identity.TeamStatusActive
	got, err = st.PatchTeam(ctx, "t", identity.TeamPatch{Status: &active})
	if err != nil {
		t.Fatal(err)
	}
	if got.SuspendedAt != nil || got.SuspendedBy != "" || got.SuspendReason != "" {
		t.Errorf("resuming left the suspension trio behind: %+v", got)
	}
}

// The same class one level up: the credential audience and the governance
// settings are independent editors of ONE org document, so neither may
// revert the other.
func TestTeamLifecycle_orgAudienceAndSettingsDoNotClobberEachOther(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()

	w := httptest.NewRecorder()
	s.handleUpdateOrgSettings(w, orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/settings",
		`{"require_provision_approval":true,"provision_approval_scope":"shared_credentials"}`, "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("settings: code=%d body=%s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleUpdateOrgCredentialAudience(w, orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/credential-audience", `{"teams":["t1"]}`, "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("audience: code=%d body=%s", w.Code, w.Body.String())
	}

	o, err := s.authStore().GetOrg(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if !o.CredentialAudience.Allows("t1") {
		t.Errorf("audience did not land: %+v", o.CredentialAudience)
	}
	if !o.RequireProvisionApproval {
		t.Error("the audience write reverted require_provision_approval — a governance flip undone by an unrelated edit")
	}
	if o.EffectiveProvisionApprovalScope() != identity.ProvisionApprovalSharedCredentials {
		t.Errorf("approval scope = %q, want it preserved", o.EffectiveProvisionApprovalScope())
	}
}

// The residue list must cover what still FIRES after the team is gone. A
// standalone webhook is the sharp case: its config authenticates on its own
// token, so deliveries keep arriving for a tenant nobody owns.
func TestTeamLifecycle_deleteRefusesATeamWithAWebhook(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.webhookConfigs = webhooks.NewMemoryConfigStore()
	if err := s.webhookConfigs.Create(withTenantCtx("t2"), webhooks.Config{
		ID: "wh1", TenantID: "t2", Name: "hook", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete a team holding a webhook: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "webhook") {
		t.Errorf("the refusal does not name the webhook: %s", body)
	}
}

// And what still holds a CREDENTIAL: a team forfait left behind is a sealed
// subscription belonging to a tenant nothing can reach.
func TestTeamLifecycle_deleteRefusesATeamWithASharedForfait(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.oauthStore = secrets.NewMemoryOAuthStore()
	sealed, err := secrets.SealOAuthPayload(s.sealer, secrets.OrgOwnerKey("t2"), secrets.OAuthKindClaudeCode, []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.oauthStore.Upsert(withTenantCtx("t2"), secrets.OAuthRecord{
		UserID: secrets.OrgOwnerKey("t2"), Kind: secrets.OAuthKindClaudeCode, SealedPayload: sealed,
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t2", "", "t2", ""))
	if w.Code != http.StatusConflict {
		t.Fatalf("delete a team holding a forfait: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "forfait") {
		t.Errorf("the refusal does not name the forfait: %s", body)
	}
}
