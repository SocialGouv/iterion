package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/auth/orgsso"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Two rules on the org rank, both read from the membership ROW: a write of a
// member's org role carries no rank above the writer's own standing, and a
// standing that drops takes with it the pending invitations its holder minted
// above what they now hold — the one place a cap read at the mint
// (requireRankToInvite) could be outlived.

// seedAccount registers an account the accept paths can resolve: an
// invitation is addressed to an ADDRESS, which they read from the account's
// own row (identity.Invitation.AddressedTo).
func seedAccount(t *testing.T, s *Server, id, email string) {
	t.Helper()
	if _, err := s.authStore().CreateUser(context.Background(), identity.User{
		ID: id, Email: email, Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// mintInvitationAs mints through the route an operator uses, so the row
// records its creator the way production writes it (Invitation.InvitedBy),
// and answers the id and the token the accept takes.
func mintInvitationAs(t *testing.T, s *Server, ctx context.Context, teamID, email string, role identity.Role) (string, string) {
	t.Helper()
	w := newErrorRecorder()
	s.handleCreateInvitation(w, teamReq(ctx, "POST", "/api/teams/"+teamID+"/invitations",
		`{"email":"`+email+`","role":"`+string(role)+`"}`, teamID, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("minting a %s invitation for %s in %s: code=%d body=%s", role, email, teamID, w.Code, w.Body.String())
	}
	var minted struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil || minted.ID == "" || minted.Token == "" {
		t.Fatalf("the mint answered no invitation: %v %s", err, w.Body.String())
	}
	return minted.ID, minted.Token
}

// patchOrgRole writes a member's org role the way the roster does.
func patchOrgRole(s *Server, ctx context.Context, orgID, memberID string, role identity.OrgRole) *errorRecorder {
	w := newErrorRecorder()
	s.handleUpdateOrgMember(w, teamReq(ctx, "PATCH", "/api/orgs/"+orgID+"/members/"+memberID,
		`{"role":"`+string(role)+`"}`, orgID, memberID))
	return w
}

// tenantAuditRows reads one action out of a tenant's trail. The action filter
// is the STORE's (audit.Page.Action), never a loop over a default page: an
// unset Page.Limit clamps to the 50 NEWEST rows of every action
// (audit.ClampLimit), which would hide the row a scenario asserts on — and
// read a capped page as exactly the count it expects. 500 is the store's own
// ceiling.
func tenantAuditRows(t *testing.T, st audit.Store, tenantID, action string) []audit.Event {
	t.Helper()
	evs, err := st.ListByTenant(context.Background(), tenantID, audit.Page{Action: action, Limit: 500})
	if err != nil {
		t.Fatalf("list %s audit: %v", tenantID, err)
	}
	return evs
}

// waitForTenantAuditRows polls a tenant's trail until the want rows a
// scenario accounts for carry action, then holds for the quiet window and
// reports any extra — so the count also says what the trail does NOT hold.
// The inserts are detached (goSafe, one goroutine per revoked row), so a read
// taken at the FIRST matching row can only under-count: a sweep that revoked
// three would be read as one, and an over-revoking sweep would pass.
func waitForTenantAuditRows(t *testing.T, st audit.Store, tenantID, action string, want int) []audit.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(tenantAuditRows(t, st, tenantID, action)) < want {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of the %d %s rows this scenario accounts for landed on tenant %q",
				len(tenantAuditRows(t, st, tenantID, action)), want, action, tenantID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(auditQuietWindow)
	hits := tenantAuditRows(t, st, tenantID, action)
	if len(hits) != want {
		t.Errorf("%d %s rows landed on tenant %q where this scenario accounts for %d: %+v",
			len(hits), action, tenantID, want, hits)
	}
	return hits
}

// seedOrgOwner adds an org OWNER to o1 — the standing above the admin rung,
// which the rule leaves free to appoint its own.
func seedOrgOwner(t *testing.T, s *Server) context.Context {
	t.Helper()
	if err := s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: "orgowner", OrgID: "o1", Role: identity.OrgRoleOwner, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "orgowner"})
}

// The one selection every write that lowers a standing makes, asserted on
// the predicate the three sites share (standingTakesInvitation): the two
// sweeps and the mint's confirmation. Each half is measured alone, and so is
// the gate that keeps the addressed-to half to the standing that is GONE: an
// invitation addressed to a member who KEEPS a membership row stands, which
// is what makes a demotion need no address at all, while the same row goes
// when the removal leaves them none.
func TestOrgRank_oneSelectionForEveryWriteThatLowersAStanding(t *testing.T) {
	const own, addr = "leaver", "leaver@example.com"
	inv := func(role identity.Role, by, email string) identity.Invitation {
		return identity.Invitation{ID: "i", TeamID: "t1", Role: role, InvitedBy: by, Email: email}
	}
	for _, c := range []struct {
		name     string
		standing identity.OrgRole
		addr     string
		known    bool
		inv      identity.Invitation
		want     bool
	}{
		{"a removal takes the ladder role they minted", "", addr, true,
			inv(identity.RoleAdmin, own, "newcomer@example.com"), true},
		{"a removal leaves the role with no mirror they minted for somebody else", "", addr, true,
			inv(identity.RoleConfigEditor, own, "newcomer@example.com"), false},
		{"a removal takes the role with no mirror they minted for themselves", "", addr, true,
			inv(identity.RoleConfigEditor, own, addr), true},
		{"a removal takes what anybody addressed to them", "", addr, true,
			inv(identity.RoleConfigEditor, "somebody-else", addr), true},
		{"an address the mixed-case row carries is the one the accept paths read", "", addr, true,
			inv(identity.RoleConfigEditor, "somebody-else", "Leaver@Example.com"), true},
		{"no address read at all selects nothing by the address", "", "", false,
			inv(identity.RoleConfigEditor, own, addr), false},
		{"another member's ladder invitation is not theirs to lose", "", addr, true,
			inv(identity.RoleAdmin, "somebody-else", "newcomer@example.com"), false},
		{"a demotion takes what they minted above the standing it leaves", identity.OrgRoleMember, addr, true,
			inv(identity.RoleAdmin, own, "newcomer@example.com"), true},
		{"a demotion leaves what is at the standing it leaves", identity.OrgRoleMember, addr, true,
			inv(identity.RoleMember, own, "newcomer@example.com"), false},
		{"a demotion leaves the row addressed to a member who keeps their own", identity.OrgRoleMember, addr, true,
			inv(identity.RoleConfigEditor, own, addr), false},
		{"a raise leaves everything they minted", identity.OrgRoleOwner, addr, true,
			inv(identity.RoleOwner, own, "newcomer@example.com"), false},
		// The addressed-to half fires at the membership row's ABSENCE, which
		// is the sentinel its callers pass from a read that found no row —
		// not at a rank of zero. A rung this build does not rank is held by
		// a member who still HAS a row, so what they addressed to themselves
		// stands, as it does on a demotion.
		{"a rung this build does not rank is a row that is still there", identity.OrgRole("billing"), addr, true,
			inv(identity.RoleConfigEditor, own, addr), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := standingTakesInvitation(own, c.standing, c.addr, c.known, c.inv); got != c.want {
				t.Errorf("standingTakesInvitation(standing=%q, addr=%q, known=%v, {role:%s by:%s to:%s}) = %v, want %v",
					c.standing, c.addr, c.known, c.inv.Role, c.inv.InvitedBy, c.inv.Email, got, c.want)
			}
		})
	}
}

// The same fact on the confirmation's side, where the standing is READ
// instead of being a sentinel: identity.OrgRole.Valid() is rank() > 0, so a
// rung this build does not rank reads as "no standing" while its holder still
// HAS a membership row — a member the demotion rule covers, not one a removal
// took out. The confirmation carries the row's presence from the read that
// made it (readOwnOrgStanding) and asks the addressed-to half only where
// there is no row, so such a member keeps what they addressed to themselves.
//
// No route reaches this today, which is why it is exercised at the
// confirmation itself: both manage checks demand AtLeast(OrgRoleAdmin), which
// refuses an unranked rung, and the stores refuse to WRITE one (`identity:
// invalid role`), so the row is supplied through the read, as the build that
// ranked the rung would have written it. It becomes reachable the day a rung
// is added to identity.OrgRole — which is exactly when a guard reading a rank
// as a row would start taking rows from members who still hold one.
func TestOrgRank_theAddressedToHalfNeedsTheRowsAbsenceNotARankOfZero(t *testing.T) {
	const unranked = identity.OrgRole("billing")
	if unranked.Valid() {
		t.Fatal("the rung this test injects is ranked — then it is not the fact the test describes")
	}
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacer(t, s)
	real := s.authStore()
	// No grant in the team either, so nothing but the membership row stands
	// between this member and the addressed-to half.
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Fatalf("the member holds a t1 grant (%+v) — then the grant, not the row, is what keeps the invitation", mb)
	}
	s.authSvc.SetStoreForTest(&unrankedStandingGate{Store: real, userID: racerID, orgID: "o1", role: unranked})
	defer s.authSvc.SetStoreForTest(real)

	inv := identity.Invitation{ID: "i", TeamID: "t1", Role: identity.RoleConfigEditor,
		InvitedBy: racerID, Email: "racer@example.com"}
	taken, have, _, err := s.standingAfterMintTakesInvitation(ctx, racerID, "o1", inv)
	if err != nil {
		t.Fatalf("confirming the standing of a member at a rung this build does not rank: %v", err)
	}
	if have != unranked {
		t.Fatalf("the standing read is %q, want the rung the row carries — then this is not the state the test describes", have)
	}
	if taken {
		t.Error("the confirmation took the self-addressed invitation of a member who still HOLDS a membership row, reading a rank of zero as no row at all")
	}
}

// unrankedStandingGate answers one account's org membership with a role this
// build cannot rank — the row a build that ranked that rung wrote, which the
// stores' own role validation refuses to write here.
type unrankedStandingGate struct {
	identity.Store
	userID, orgID string
	role          identity.OrgRole
}

func (g *unrankedStandingGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID == g.userID && orgID == g.orgID {
		return identity.OrgMembership{UserID: userID, OrgID: orgID, Role: g.role, JoinedAt: time.Now()}, nil
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// ---- rule 1: no org role is written above the writer's own standing ----

// The roster's role write is gated on canManageOrg, which is
// AtLeast(OrgRoleAdmin): an org ADMIN reached `owner` on any row of the org,
// their own included, and handed out — or took — the rank above their own.
// The rule caps the role written at the writer's own standing, read from the
// membership ROW. Measured on the row, not on the body: a refusal that
// answered 403 and wrote anyway hands out the rank it refused.
func TestOrgMembers_aRoleWriteCannotRaiseAboveTheWritersOwnStanding(t *testing.T) {
	for _, c := range []struct {
		name, target string
		was          identity.OrgRole
	}{
		{"another member's row", racerID, identity.OrgRoleMember},
		{"their own row", "orgadmin", identity.OrgRoleAdmin},
	} {
		t.Run("an org admin writes owner on "+c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacer(t, s)
			ctx := context.Background()
			real := s.authStore()

			w := patchOrgRole(s, orgAdminCtx(), "o1", c.target, identity.OrgRoleOwner)
			if w.Code != http.StatusForbidden {
				t.Fatalf("writing owner as an org admin: code=%d body=%s, want 403", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range []string{
				"cannot be raised above the writer's own",
				"the org role owner",
				"your own org role in this org is admin",
				// The remedy: the cap leaves an org whose roster holds no
				// owner — what the teams→orgs backfill creates, where
				// OrgRoleForTeamRole tops out at org admin — with no member
				// able to appoint one, so the refusal names who can.
				"a member of this org who already holds owner writes it, or a platform admin",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal does not name %q: %s", want, body)
				}
			}
			om, err := real.GetOrgMembership(ctx, c.target, "o1")
			if err != nil {
				t.Fatalf("the refused write took the row away: %v", err)
			}
			if om.Role != c.was {
				t.Errorf("the refused write wrote anyway: role=%q, want %q", om.Role, c.was)
			}
		})
	}
}

// What the cap does not take away: every rank a writer already holds, in both
// directions — an org owner appoints an owner, an org admin writes admin, an
// org admin LOWERS an admin to member (the rank the row carried, so the write
// is a lowering and not a no-op), an org owner lowers another owner — equal
// rank, which the rank of the TARGET passes too (requireRankOverMember) — and
// a platform admin writes anything (IsSuperAdmin is the escape hatch, and
// they hold no org membership for a cap to read).
func TestOrgMembers_theRoleWritesThatStillGoThrough(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  func(*testing.T, *Server) context.Context
		// was is the rank the target's row carries before the write, when
		// the case needs one other than the member seedRacer leaves.
		was  identity.OrgRole
		role identity.OrgRole
	}{
		{"an org owner appoints an owner", func(t *testing.T, s *Server) context.Context { return seedOrgOwner(t, s) }, "", identity.OrgRoleOwner},
		{"an org admin writes admin", func(*testing.T, *Server) context.Context { return orgAdminCtx() }, "", identity.OrgRoleAdmin},
		{"an org admin lowers an admin to member", func(*testing.T, *Server) context.Context { return orgAdminCtx() }, identity.OrgRoleAdmin, identity.OrgRoleMember},
		{"an org owner lowers an owner to member", func(t *testing.T, s *Server) context.Context { return seedOrgOwner(t, s) }, identity.OrgRoleOwner, identity.OrgRoleMember},
		{"a platform admin writes owner", func(*testing.T, *Server) context.Context { return superAdminCtx() }, "", identity.OrgRoleOwner},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacer(t, s)
			ctx := c.ctx(t, s)
			if c.was != "" {
				if err := s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
					UserID: racerID, OrgID: "o1", Role: c.was, JoinedAt: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
			}

			w := patchOrgRole(s, ctx, "o1", racerID, c.role)
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s, want 200", w.Code, w.Body.String())
			}
			om, err := s.authStore().GetOrgMembership(context.Background(), racerID, "o1")
			if err != nil {
				t.Fatalf("the accepted write left no row: %v", err)
			}
			if om.Role != c.role {
				t.Errorf("the row carries %q, want %q", om.Role, c.role)
			}
		})
	}
}

// The cap is on the rank WRITTEN, so it leaves a LOWERING free whatever rank
// the row carries: `member` on an OWNER's row is written, and that write
// carries the invitation sweep, so what the owner minted above `member` goes
// with it. WHO may make it is the target's rank (requireRankOverMember): an
// owner's row takes an owner or a platform admin, which is what writes here.
func TestOrgMembers_aLoweringOfAnOwnersRowGoesThroughAndSweeps(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	ownerCtx := seedOrgOwner(t, s)
	handOverID, handOverTok := mintInvitationAs(t, s, ownerCtx, "t1", "successor@example.com", identity.RoleOwner)
	seedAccount(t, s, "successor", "successor@example.com")

	w := patchOrgRole(s, superAdminCtx(), "o1", "orgowner", identity.OrgRoleMember)
	if w.Code != http.StatusOK {
		t.Fatalf("a platform admin lowering the org owner: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, "orgowner", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Errorf("the owner's row is %+v (%v), want a plain member", om, err)
	}
	if got, err := real.GetInvitation(ctx, handOverID); err == nil {
		t.Errorf("the sweep did not follow the lowering of a higher-ranked row: %+v", got)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "successor", handOverTok); err == nil {
		t.Error("the revoked hand-over invitation was still accepted")
	}
}

// And the same chain written by an author the two rank rules BIND, since a
// platform admin returns at the first line of both: an org OWNER lowering a
// PEER owner passes the target rule at equal rank and the cap below their own
// standing, so the cap, the rank guard, the write and the sweep are measured
// in one request — the chain a change that swept only for platform admins
// would leave witnessed nowhere.
func TestOrgMembers_anOwnerLoweringAPeerOwnerSweepsWhatTheyMinted(t *testing.T) {
	const peerOwner = "orgowner2"
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	ownerCtx := seedOrgOwner(t, s)
	handOverID, handOverTok := mintInvitationAs(t, s, ownerCtx, "t1", "successor@example.com", identity.RoleOwner)
	seedAccount(t, s, "successor", "successor@example.com")
	seedOrgRow(t, s, peerOwner, "o1", identity.OrgRoleOwner)

	w := patchOrgRole(s, memberCtx(peerOwner), "o1", "orgowner", identity.OrgRoleMember)
	if w.Code != http.StatusOK {
		t.Fatalf("an org owner lowering a peer owner: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, "orgowner", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Errorf("the peer's row is %+v (%v), want a plain member", om, err)
	}
	if got, err := real.GetInvitation(ctx, handOverID); err == nil {
		t.Errorf("the sweep did not follow a lowering written by an author the rank rules bind: %+v", got)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "successor", handOverTok); err == nil {
		t.Error("the revoked hand-over invitation was still accepted")
	}
}

// The standing is the ROW, not the claim: a token carries the org role its
// session was minted with, which a demotion since does not rewrite — reading
// it here would hand the cap to the holder of a stale claim.
func TestOrgMembers_aTokenClaimingMoreThanTheRowDoesNotWriteTheRank(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	stale := auth.Identity{UserID: "orgadmin", OrgID: "o1", OrgRole: identity.OrgRoleOwner}

	w := patchOrgRole(s, auth.WithIdentity(ctx, stale), "o1", racerID, identity.OrgRoleOwner)
	if w.Code != http.StatusForbidden {
		t.Fatalf("writing owner on a claim the membership row does not back: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "your own org role in this org is admin") {
		t.Errorf("the refusal does not name the role the ROW carries: %s", body)
	}
	if om, err := s.authStore().GetOrgMembership(ctx, racerID, "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Errorf("the refused write moved the row: %+v (%v)", om, err)
	}
}

// The standing is the membership ROW, read again by the cap: canManageOrg
// read it before the body named the role, and a removal landing in between
// refuses the write rather than letting it through on a rank that no longer
// exists.
func TestOrgMembers_aStandingThatVanishesAfterTheGateRefusesTheRoleWrite(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	gate := &orgStandingVanishGate{Store: real, userID: "orgadmin", orgID: "o1"}
	s.authSvc.SetStoreForTest(gate)

	w := patchOrgRole(s, orgAdminCtx(), "o1", racerID, identity.OrgRoleAdmin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("writing on a standing that vanished after the gate: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "your own org role in this org is none") {
		t.Errorf("the refusal does not name the standing it read: %s", body)
	}
	if got := gate.readCount(); got != 2 {
		t.Errorf("the writer's org membership was read %d times, want 2 — the gate's read and the cap's own", got)
	}
	s.authSvc.SetStoreForTest(real)
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Errorf("the refused write moved the row: %+v (%v)", om, err)
	}
}

// orgStandingLateErrorGate answers the FIRST read of one account's org
// membership and fails every read from the second on — a store that goes away
// between a gate and the act it authorized, where orgStandingErrorGate (which
// fails from the first read) is refused by canManageOrg before the cap is
// reached at all.
type orgStandingLateErrorGate struct {
	identity.Store
	userID, orgID string

	mu    sync.Mutex
	reads int
}

func (g *orgStandingLateErrorGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	g.reads++
	n := g.reads
	g.mu.Unlock()
	if n > 1 {
		return identity.OrgMembership{}, errors.New("identity: get org membership: connection reset by peer")
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// A standing that cannot be READ is not a standing: the write is refused with
// the store's own error rather than passed through on a membership nobody
// could see.
func TestOrgMembers_aStandingThatCannotBeReadRefusesTheRoleWrite(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	ctx := context.Background()
	real := s.authStore()
	s.authSvc.SetStoreForTest(&orgStandingLateErrorGate{Store: real, userID: "orgadmin", orgID: "o1"})

	w := patchOrgRole(s, orgAdminCtx(), "o1", racerID, identity.OrgRoleAdmin)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("writing while the writer's standing cannot be read: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if body := w.Diagnostic(t); !strings.Contains(body, "connection reset by peer") {
		t.Errorf("the 500 does not carry the store's own error: %s", body)
	}
	s.authSvc.SetStoreForTest(real)
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Errorf("the refused write moved the row: %+v (%v)", om, err)
	}
}

// ---- rule 2: a standing that drops takes the invitations minted above it ----

// A demotion lowers the standing every invitation its holder MINTED was
// capped at: accepting one writes the team grant and, on the ladder, the org
// membership its role is mirrored up to (pkg/auth
// grantOrgMembershipForTeam), so an invitation above the role just written
// would still hand out a rank its creator no longer holds. What stands: an
// invitation at the new standing, one mirrored up to NOTHING (config_editor,
// ADR-078), and one somebody ELSE minted — including one they minted FOR the
// demoted member, above the new standing. Invitations are revoked by who
// minted them, blind to who receives them, so one the demoted member minted
// for their own address goes too.
func TestOrgMembers_aDemotionRevokesTheInvitationsTheMemberMintedAboveTheirNewStanding(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	ctx := context.Background()
	real := s.authStore()
	seedRacerAsTeamAdmin(t, s)
	seedAccount(t, s, "newcomer", "newcomer@example.com")
	seedAccount(t, s, "second", "second@example.com")
	// The demoted member holds an account row too, so the ADDRESS an
	// invitation names them at is resolvable (identity.Invitation.AddressedTo
	// reads it from that row): without it, a sweep that selected on the
	// address the way the REMOVAL's does could not reach them at all, and the
	// case below that pins the address half would stand on a fixture that
	// cannot tell the two predicates apart.
	seedAccount(t, s, "orgadmin", "orgadmin@example.com")

	aboveID, aboveTok := mintInvitationAs(t, s, orgAdminCtx(), "t1", "newcomer@example.com", identity.RoleAdmin)
	// In another team of the same org: the sweep walks every team of it.
	aboveInT2ID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t2", "int2@example.com", identity.RoleAdmin)
	atID, atTok := mintInvitationAs(t, s, orgAdminCtx(), "t1", "second@example.com", identity.RoleMember)
	offLadderID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "third@example.com", identity.RoleConfigEditor)
	racerCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	othersID, _ := mintInvitationAs(t, s, racerCtx, "t1", "fourth@example.com", identity.RoleMember)
	// Another member's invitation ABOVE the standing being taken away: the
	// sweep selects on who MINTED, not on the role alone.
	ownerCtx := seedOrgOwner(t, s)
	byOwnerID, _ := mintInvitationAs(t, s, ownerCtx, "t1", "fifth@example.com", identity.RoleAdmin)
	// Addressed TO the demoted member and above the new standing, minted by
	// somebody else: the half the rule leaves alone, measured on the address
	// rather than on a role that would have kept it anyway.
	toThemID, _ := mintInvitationAs(t, s, ownerCtx, "t1", "orgadmin@example.com", identity.RoleAdmin)
	// And the one they minted FOR THEMSELVES above the new standing, which
	// the sweep takes: the predicate is who MINTED it, blind to the address,
	// so an invitation to their own address is not a way to keep the rank.
	selfAddressedID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "orgadmin@example.com", identity.RoleAdmin)

	demote := patchOrgRole(s, superAdminCtx(), "o1", "orgadmin", identity.OrgRoleMember)
	if demote.Code != http.StatusOK {
		t.Fatalf("demoting the creator: code=%d body=%s, want 200", demote.Code, demote.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, "orgadmin", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the creator is %+v (%v), want a plain member", om, err)
	}

	// The row, then the accept that row authorized: the invitation above the
	// new standing is gone, so the accept has nothing to claim and writes
	// neither the org membership nor the grant.
	if got, err := real.GetInvitation(ctx, aboveID); err == nil {
		t.Fatalf("the admin invitation of a demoted creator stands: %+v", got)
	}
	if got, err := real.GetInvitation(ctx, aboveInT2ID); err == nil {
		t.Errorf("the admin invitation of a demoted creator in another team of the org stands: %+v", got)
	}
	if got, err := real.GetInvitation(ctx, selfAddressedID); err == nil {
		t.Errorf("the admin invitation the demoted creator minted for their OWN address stands: %+v", got)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "newcomer", aboveTok); err == nil {
		t.Error("the revoked invitation was still accepted")
	}
	if om, err := real.GetOrgMembership(ctx, "newcomer", "o1"); err == nil {
		t.Errorf("the revoked invitation walked its invitee into the org: %+v", om)
	}
	if mb, err := real.GetMembership(ctx, "newcomer", "t1"); err == nil {
		t.Errorf("the revoked invitation wrote its team grant: %+v", mb)
	}

	// The member row went too: the demoted creator holds no grant in any team
	// of the org, so the cap that would bind their minting now measures a
	// grant that covers nothing and refuses the member row the old standing
	// carried — the team half of the same predicate the mirror half answers.
	if _, err := real.GetInvitation(ctx, atID); err == nil {
		t.Errorf("the member invitation of a demoted creator who holds no grant stands: the cap that binds them now refuses it")
	}
	for _, kept := range []struct{ what, id string }{
		{"mirrored up to nothing", offLadderID},
		{"somebody else minted for the demoted member, above the new standing", toThemID},
		{"minted by somebody else", othersID},
		{"minted by somebody else above the new standing", byOwnerID},
	} {
		if _, err := real.GetInvitation(ctx, kept.id); err != nil {
			t.Errorf("the demotion revoked the invitation %s: %v", kept.what, err)
		}
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "second", atTok); err == nil {
		t.Errorf("the member invitation of a creator who can no longer mint it still accepts")
	}

	// Recorded where the row lived, with the cause — and exactly the rows
	// the sweep took, held past the quiet window: a count read at the first
	// insert would stay green on a sweep that took everything. Three rows:
	// the admin one, its self-addressed twin, and the member row the
	// creator's absent grant no longer covers.
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 3)
	var named *audit.Event
	for i, e := range rows {
		if e.TargetID == aboveID {
			named = &rows[i]
		}
	}
	if named == nil {
		t.Fatalf("no audit line names the revoked invitation: %+v", rows)
	}
	if named.Meta["reason"] != "org_member_demoted" {
		t.Errorf("the audit line does not name the cause: %+v", named.Meta)
	}
	if named.Meta["email"] != "newcomer@example.com" {
		t.Errorf("the audit line does not name the address the revoked invitation held: %+v", named.Meta)
	}
	inT2 := waitForTenantAuditRows(t, s.auditStore, "t2", "invitation.deleted", 1)
	if inT2[0].TargetID != aboveInT2ID {
		t.Errorf("the t2 audit line names %q, want the invitation revoked there (%s)", inT2[0].TargetID, aboveInT2ID)
	}
}

// A removal leaves the member no standing in the org at all, so what they
// minted is measured against THAT — the demotion's rule at the standing a
// removal leaves: every invitation carrying an org role
// (identity.OrgRoleForTeamRole) is above it and goes with them, and the one
// mirrored up to NOTHING (config_editor, ADR-078) is above no standing and
// stands, as it does on a demotion — unless it is addressed to the member
// THEMSELVES, which the removal's other half takes whatever its role, since
// accepting it would seat the off-boarded member back in a team of the org.
// It holds for a target with NO account row, where the invitations addressed
// TO them cannot be selected at all (the address lives on that row), and it
// is scoped to the org the removal names: what the same creator minted in
// another org stands, as does another creator's invitation.
func TestOrgOffboarding_aRemovalRevokesTheInvitationsTheMemberMintedAboveWhatTheyAreLeftWith(t *testing.T) {
	for _, c := range []struct {
		name    string
		account bool
	}{
		{"with an account row", true},
		{"with no account row", false},
	} {
		t.Run("the removed member "+c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			ctx := context.Background()
			real := s.authStore()
			if c.account {
				seedAccount(t, s, "orgadmin", "orgadmin@example.com")
			}
			seedRacerAsTeamAdmin(t, s)

			minted := map[string]string{}
			for _, role := range []identity.Role{identity.RoleOwner, identity.RoleAdmin, identity.RoleMember} {
				id, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", string(role)+"@example.com", role)
				minted[string(role)] = id
			}
			// The role whose org mirror is EMPTY: no standing outranks it,
			// so the removal measures it the way a demotion does and leaves
			// it standing.
			offLadder, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "cfged@example.com", identity.RoleConfigEditor)
			// The same role, addressed to the minter's OWN address: the
			// minted half leaves it, and the addressed-to half takes it —
			// where an account row names that address. With no row there is
			// no address to select by, and it stands.
			selfAddressed, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "orgadmin@example.com", identity.RoleConfigEditor)
			// t2 as well: the sweep walks every team of the org.
			inT2, _ := mintInvitationAs(t, s, orgAdminCtx(), "t2", "t2@example.com", identity.RoleAdmin)
			racerCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
			othersID, _ := mintInvitationAs(t, s, racerCtx, "t1", "byracer@example.com", identity.RoleMember)
			// The same creator in ANOTHER org, through the service: the route
			// would refuse it, since they hold no standing in o2.
			_, elsewhere, err := s.authSvc.CreateInvitation(testMintVerdictContext(ctx), "t9", "elsewhere@example.com", identity.RoleAdmin, "orgadmin")
			if err != nil {
				t.Fatal(err)
			}

			rm := newErrorRecorder()
			s.handleRemoveOrgMember(rm, teamReq(superAdminCtx(), "DELETE", "/api/orgs/o1/members/orgadmin", "", "o1", "orgadmin"))
			if rm.Code != http.StatusNoContent {
				t.Fatalf("removing the creator: code=%d body=%s, want 204", rm.Code, rm.Body.String())
			}

			for role, id := range minted {
				if got, err := real.GetInvitation(ctx, id); err == nil {
					t.Errorf("the %s invitation of a removed creator stands: %+v", role, got)
				}
			}
			if _, err := real.GetInvitation(ctx, offLadder); err != nil {
				t.Errorf("the removal revoked the invitation mirrored up to NOTHING, which no standing outranks: %v", err)
			}
			switch _, err := real.GetInvitation(ctx, selfAddressed); {
			case c.account && err == nil:
				t.Error("the removal left the invitation the member minted for their OWN address, which seats them back in a team of the org they just left")
			case !c.account && err != nil:
				t.Errorf("the removal took an invitation addressed to an address no account row named: %v", err)
			}
			if got, err := real.GetInvitation(ctx, inT2); err == nil {
				t.Errorf("an invitation in another team of the same org stands: %+v", got)
			}
			if _, err := real.GetInvitation(ctx, othersID); err != nil {
				t.Errorf("the removal revoked an invitation it did not mint: %v", err)
			}
			if _, err := real.GetInvitation(ctx, elsewhere.ID); err != nil {
				t.Errorf("the removal reached into another org: %v", err)
			}
		})
	}
}

// invitationListFailGate fails the first failures listings of a team's
// invitations, the way a saturated pool does.
type invitationListFailGate struct {
	identity.Store
	failures int

	mu    sync.Mutex
	calls int
}

func (g *invitationListFailGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	g.mu.Lock()
	g.calls++
	fail := g.calls <= g.failures
	g.mu.Unlock()
	if fail {
		return nil, errors.New("identity: list invitations: connection reset by peer")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

// A sweep that could not READ the invitations is not a sweep that found none:
// the role is written, so the answer says which state that leaves and asks
// for the re-issue that finishes it. The re-issue is what the reconciliation
// measures against the role WRITTEN rather than against the one it replaced:
// the same role again, on a row that already carries it, still revokes what
// the failure left.
func TestOrgMembers_aDemotionWhoseInvitationSweepFailedSaysSoAndFinishesOnTheReissue(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedAccount(t, s, "newcomer", "newcomer@example.com")
	aboveID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "newcomer@example.com", identity.RoleAdmin)

	s.authSvc.SetStoreForTest(&invitationListFailGate{Store: real, failures: 1})
	w := patchOrgRole(s, superAdminCtx(), "o1", "orgadmin", identity.OrgRoleMember)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a demotion whose invitation sweep failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	body := w.Diagnostic(t)
	for _, want := range []string{
		"is now member",
		"revoking the pending invitations they created",
		"connection reset by peer",
		"re-issue the role change",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not name %q: %s", want, body)
		}
	}
	if om, err := real.GetOrgMembership(ctx, "orgadmin", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the role was not written: %+v (%v)", om, err)
	}
	if _, err := real.GetInvitation(ctx, aboveID); err != nil {
		t.Fatalf("the invitation is gone though the sweep failed (%v) — then this is not the exit the test describes", err)
	}

	again := patchOrgRole(s, superAdminCtx(), "o1", "orgadmin", identity.OrgRoleMember)
	if again.Code != http.StatusOK {
		t.Fatalf("re-issuing the role change: code=%d body=%s, want 200", again.Code, again.Diagnostic(t))
	}
	if got, err := real.GetInvitation(ctx, aboveID); err == nil {
		t.Errorf("the re-issue did not revoke the invitation the failure left: %+v", got)
	}
	s.authSvc.SetStoreForTest(real)
}

// The org placement writes an EXISTING membership too, so it is the other way
// an org role goes DOWN, and it reconciles what its subject minted exactly as
// the roster's PATCH does. The rank cap does not apply to it — the route is
// super-admin only, which the cap exempts — but the standing of its SUBJECT
// is what this sweep reads.
func TestOrgMembers_aPlacementThatLowersARoleRevokesTheInvitationsAboveIt(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedAccount(t, s, "orgadmin", "orgadmin@example.com")
	seedAccount(t, s, "newcomer", "newcomer@example.com")
	aboveID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "newcomer@example.com", identity.RoleAdmin)
	atID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "second@example.com", identity.RoleMember)

	w := newErrorRecorder()
	s.handlePutOrgMember(w, teamReq(superAdminCtx(), "PUT", "/api/orgs/o1/members/orgadmin",
		`{"role":"member"}`, "o1", "orgadmin"))
	if w.Code != http.StatusOK {
		t.Fatalf("placing an org admin back at member: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, "orgadmin", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the placement wrote %+v (%v), want a plain member", om, err)
	}
	if got, err := real.GetInvitation(ctx, aboveID); err == nil {
		t.Errorf("the admin invitation of a member placed back at member stands: %+v", got)
	}
	// The member row went with it: the creator placed back at member holds no
	// grant in any team, so the cap that would bind their minting now
	// measures a grant that covers nothing and refuses the row the old
	// standing carried.
	if got, err := real.GetInvitation(ctx, atID); err == nil {
		t.Errorf("the member invitation of a creator who can no longer mint it stands: %+v", got)
	}
}

// roleWriteCancelGate cancels the request right after the role write lands
// and honours the context on the sweep's own first read.
type roleWriteCancelGate struct {
	identity.Store
	userID, orgID string
	cancel        func()

	mu   sync.Mutex
	done bool
}

func (g *roleWriteCancelGate) UpsertOrgMembership(ctx context.Context, om identity.OrgMembership) error {
	if err := g.Store.UpsertOrgMembership(ctx, om); err != nil {
		return err
	}
	if om.UserID != g.userID || om.OrgID != g.orgID {
		return nil
	}
	g.mu.Lock()
	first := !g.done
	g.done = true
	g.mu.Unlock()
	if first {
		g.cancel()
	}
	return nil
}

func (g *roleWriteCancelGate) ListTeamsByOrg(ctx context.Context, orgID string) ([]identity.Team, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("identity: list teams by org: %w", err)
	}
	return g.Store.ListTeamsByOrg(ctx, orgID)
}

// The standing is already lowered when the sweep runs, so a client that hangs
// up in between must not take the sweep with it: it runs on a context
// detached from the request.
func TestOrgMembers_aClientGoneAfterTheRoleWriteStillGetsTheInvitationSweep(t *testing.T) {
	s := newOrgCredsTestServer(t)
	bg := context.Background()
	real := s.authStore()
	seedAccount(t, s, "newcomer", "newcomer@example.com")
	aboveID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "newcomer@example.com", identity.RoleAdmin)

	ctx, cancel := context.WithCancel(superAdminCtx())
	defer cancel()
	s.authSvc.SetStoreForTest(&roleWriteCancelGate{Store: real, userID: "orgadmin", orgID: "o1", cancel: cancel})
	w := patchOrgRole(s, ctx, "o1", "orgadmin", identity.OrgRoleMember)
	s.authSvc.SetStoreForTest(real)

	if om, err := real.GetOrgMembership(bg, "orgadmin", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the role write did not land: %+v (%v) — then this is not the exit the test describes", om, err)
	}
	if got, err := real.GetInvitation(bg, aboveID); err == nil {
		t.Errorf("the invitation above the new standing outlived the demotion (%+v) — the sweep died with the request (the write answered %d: %s)",
			got, w.Code, w.Body.String())
	}
}

// mintInWindowGate mints an invitation as the member being demoted, in the
// window between the read of their row and the write that lowers it — the
// mint the old standing still authorizes.
type mintInWindowGate struct {
	identity.Store
	s             *Server
	userID, orgID string

	mu    sync.Mutex
	done  bool
	code  int
	body  string
	invID string
}

func (g *mintInWindowGate) UpsertOrgMembership(ctx context.Context, om identity.OrgMembership) error {
	if om.UserID != g.userID || om.OrgID != g.orgID {
		return g.Store.UpsertOrgMembership(ctx, om)
	}
	g.mu.Lock()
	first := !g.done
	g.done = true
	g.mu.Unlock()
	if first {
		w := newErrorRecorder()
		g.s.handleCreateInvitation(w, teamReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: g.userID}),
			"POST", "/api/teams/t1/invitations", `{"email":"inwindow@example.com","role":"admin"}`, "t1", ""))
		var minted struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &minted)
		g.mu.Lock()
		g.code, g.body, g.invID = w.Code, w.Body.String(), minted.ID
		g.mu.Unlock()
	}
	return g.Store.UpsertOrgMembership(ctx, om)
}

// The sweep runs AFTER the role write, which is what makes it cover the
// window: an invitation minted on the standing being taken away — by the
// member themselves, in the window the demotion's own read opened — is swept
// by the sweep that follows the write, where one running before it would
// leave exactly that row behind.
func TestOrgMembers_theInvitationSweepFollowsTheRoleWriteSoTheWindowIsCovered(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedAccount(t, s, "inwindow", "inwindow@example.com")
	gate := &mintInWindowGate{Store: real, s: s, userID: "orgadmin", orgID: "o1"}
	s.authSvc.SetStoreForTest(gate)

	w := patchOrgRole(s, superAdminCtx(), "o1", "orgadmin", identity.OrgRoleMember)
	s.authSvc.SetStoreForTest(real)
	if w.Code != http.StatusOK {
		t.Fatalf("demoting the creator: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	gate.mu.Lock()
	code, body, invID := gate.code, gate.body, gate.invID
	gate.mu.Unlock()
	if code != http.StatusOK || invID == "" {
		t.Fatalf("the mint in the window: code=%d body=%s — then this is not the window the test describes", code, body)
	}
	if got, err := real.GetInvitation(ctx, invID); err == nil {
		t.Errorf("the invitation minted in the window stands: %+v", got)
	}
}

// The body an operator reads names the act THEY issued: a placement whose
// sweep failed asks for the placement again, not for a role change.
func TestOrgMembers_aPlacementWhoseInvitationSweepFailedNamesThePlacement(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedAccount(t, s, "orgadmin", "orgadmin@example.com")
	seedAccount(t, s, "newcomer", "newcomer@example.com")
	aboveID, _ := mintInvitationAs(t, s, orgAdminCtx(), "t1", "newcomer@example.com", identity.RoleAdmin)

	s.authSvc.SetStoreForTest(&invitationListFailGate{Store: real, failures: 1})
	w := newErrorRecorder()
	s.handlePutOrgMember(w, teamReq(superAdminCtx(), "PUT", "/api/orgs/o1/members/orgadmin",
		`{"role":"member"}`, "o1", "orgadmin"))
	s.authSvc.SetStoreForTest(real)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a placement whose invitation sweep failed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if body := w.Diagnostic(t); !strings.Contains(body, "re-issue the placement") {
		t.Errorf("the 500 does not name the act to re-issue: %s", body)
	}
	if _, err := real.GetInvitation(ctx, aboveID); err != nil {
		t.Fatalf("the invitation is gone though the sweep failed (%v) — then this is not the exit the test describes", err)
	}
}

// A role write that does not lower the standing revokes nothing: each
// invitation is measured against the role WRITTEN, and the ranks at or below
// it stand.
func TestOrgMembers_aRaiseLeavesTheInvitationsTheMemberMinted(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedRacerAsTeamAdmin(t, s)
	seedAccount(t, s, "newcomer", "newcomer@example.com")
	racerCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	mineID, mineTok := mintInvitationAs(t, s, racerCtx, "t1", "newcomer@example.com", identity.RoleMember)

	w := patchOrgRole(s, orgAdminCtx(), "o1", racerID, identity.OrgRoleAdmin)
	if w.Code != http.StatusOK {
		t.Fatalf("raising a member to admin: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if _, err := real.GetInvitation(ctx, mineID); err != nil {
		t.Fatalf("the raise revoked an invitation below the standing it wrote: %v", err)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "newcomer", mineTok); err != nil {
		t.Errorf("that invitation no longer accepts: %v", err)
	}
}

// ---- the mint confirms its own cap, so the sweep's window is closed ----

// mintParkGate parks the invitation INSERT of one address until the test
// releases it: the mint whose standing read precedes a write that lowers it
// and whose row lands after that write's sweep has listed the team.
type mintParkGate struct {
	identity.Store
	email   string
	reached chan struct{}
	release chan struct{}

	mu   sync.Mutex
	once bool
}

func (g *mintParkGate) CreateInvitation(ctx context.Context, inv identity.Invitation) error {
	g.mu.Lock()
	first := !g.once && inv.Email == g.email
	if first {
		g.once = true
	}
	g.mu.Unlock()
	if first {
		close(g.reached)
		<-g.release
	}
	return g.Store.CreateInvitation(ctx, inv)
}

// The order the sweep cannot cover, through the real pair of handlers: the
// mint reads its standing, a demotion writes the row and sweeps the team, and
// only then does the mint's row land. The sweep listed a team that did not
// hold it yet, so what refuses it is the mint's own confirmation — without
// which that invitation seats an org admin on a plain member's authority.
func TestTeamInvitations_aMintThatLandsAfterTheSweepIsRefusedByItsOwnConfirmation(t *testing.T) {
	for _, c := range []struct {
		name string
		mint func(*Server, context.Context) *errorRecorder
	}{
		{"the team route", func(s *Server, ctx context.Context) *errorRecorder {
			w := newErrorRecorder()
			s.handleCreateInvitation(w, teamReq(ctx, "POST", "/api/teams/t1/invitations",
				`{"email":"accomplice@example.com","role":"admin"}`, "t1", ""))
			return w
		}},
		{"the org route", func(s *Server, ctx context.Context) *errorRecorder {
			w := newErrorRecorder()
			s.handleCreateOrgInvitation(w, teamReq(ctx, "POST", "/api/orgs/o1/invitations",
				`{"email":"accomplice@example.com","role":"admin","team_id":"t1"}`, "o1", ""))
			return w
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			s.auditStore = audit.NewMemoryStore()
			ctx := context.Background()
			real := s.authStore()
			seedAccount(t, s, "accomplice", "accomplice@example.com")
			gate := &mintParkGate{Store: real, email: "accomplice@example.com",
				reached: make(chan struct{}), release: make(chan struct{})}
			s.authSvc.SetStoreForTest(gate)

			done := make(chan *errorRecorder, 1)
			go func() { done <- c.mint(s, orgAdminCtx()) }()
			awaitGate(t, gate.reached, "the mint parked just before its insert")

			demote := patchOrgRole(s, superAdminCtx(), "o1", "orgadmin", identity.OrgRoleMember)
			if demote.Code != http.StatusOK {
				t.Fatalf("the demotion that ran inside the mint: code=%d body=%s, want 200", demote.Code, demote.Body.String())
			}
			close(gate.release)
			w := <-done
			s.authSvc.SetStoreForTest(real)

			if w.Code != http.StatusForbidden {
				t.Fatalf("the mint whose standing dropped before its row landed: code=%d body=%s, want 403", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range []string{
				"cannot carry an org role above its creator's",
				"your own org role in this team's organization is member by the time it was minted",
				"the invitation this request minted was revoked",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal does not name %q: %s", want, body)
				}
			}
			// The row, and the token with it: an invitation the confirmation
			// refused is gone from the store, so no accept can reach it.
			invs, err := real.ListInvitationsByTeam(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			for _, inv := range invs {
				if inv.Email == "accomplice@example.com" {
					t.Fatalf("the refused mint left its invitation standing: %+v", inv)
				}
			}
			rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
			if got := rows[0].Meta["reason"]; got != "creator_standing_dropped" {
				t.Errorf("the audit line of the revoked invitation names %v, want creator_standing_dropped", got)
			}
		})
	}
}

// A role mirrored up to NOTHING (config_editor, ADR-078) carries no org
// standing for the MINTED half to measure, so no rank refuses it: no CAP at
// the mint, and no sweep of that half when its creator's standing drops — a
// removal included, which revokes what they minted above the standing it
// leaves them (revokeOrgTeamAccess). A mint that straddles the removal is the
// row that sweep cannot see, and it meets the same rule from the other side:
// the confirmation leaves what the sweep would have left, so one rule decides
// such an invitation's fate whichever side of the sweep its row lands on.
//
// The row kept here is addressed to SOMEBODY ELSE — the capability is handed
// over, not withdrawn, when its creator leaves. The one addressed to the
// minter themselves is what the removal's addressed-to half takes on either
// side of the sweep (the witness below).
func TestTeamInvitations_aMintStraddlingTheRemovalKeepsTheRoleMirrorLessRow(t *testing.T) {
	for _, c := range []struct {
		name, minter, email string
		seed                func(*testing.T, *Server)
		mintCtx, removerCtx func() context.Context
		mint                func(*Server, context.Context, string) *errorRecorder
	}{
		{
			// The minter is a t1 admin and a plain member of o1, minting for
			// an address that is not theirs: what the accept seats is
			// somebody else, on a capability that carries no org rank.
			name: "the team route", minter: racerID, email: "colleague@example.com",
			seed:    seedRacerAsTeamAdmin,
			mintCtx: func() context.Context { return auth.WithIdentity(context.Background(), auth.Identity{UserID: racerID}) },
			// The org admin off-boards them, as the roster does.
			removerCtx: orgAdminCtx,
			mint: func(s *Server, ctx context.Context, email string) *errorRecorder {
				w := newErrorRecorder()
				s.handleCreateInvitation(w, teamReq(ctx, "POST", "/api/teams/t1/invitations",
					`{"email":"`+email+`","role":"config_editor"}`, "t1", ""))
				return w
			},
		},
		{
			// The org route's minter manages the org, so the one who
			// off-boards them is a platform admin.
			name: "the org route", minter: "orgadmin", email: "latecomer@example.com",
			seed: func(*testing.T, *Server) {}, mintCtx: orgAdminCtx, removerCtx: superAdminCtx,
			mint: func(s *Server, ctx context.Context, email string) *errorRecorder {
				w := newErrorRecorder()
				s.handleCreateOrgInvitation(w, teamReq(ctx, "POST", "/api/orgs/o1/invitations",
					`{"email":"`+email+`","role":"config_editor","team_id":"t1"}`, "o1", ""))
				return w
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			s.auditStore = audit.NewMemoryStore()
			ctx := context.Background()
			real := s.authStore()
			c.seed(t, s)
			gate := &mintParkGate{Store: real, email: c.email,
				reached: make(chan struct{}), release: make(chan struct{})}
			s.authSvc.SetStoreForTest(gate)

			done := make(chan *errorRecorder, 1)
			go func() { done <- c.mint(s, c.mintCtx(), c.email) }()
			awaitGate(t, gate.reached, "the mint parked just before its insert")

			off := newErrorRecorder()
			s.handleRemoveOrgMember(off, teamReq(c.removerCtx(), "DELETE", "/api/orgs/o1/members/"+c.minter, "", "o1", c.minter))
			if off.Code != http.StatusNoContent {
				t.Fatalf("the removal that ran inside the mint: code=%d body=%s, want 204", off.Code, off.Body.String())
			}
			close(gate.release)
			w := <-done
			s.authSvc.SetStoreForTest(real)

			if om, err := real.GetOrgMembership(ctx, c.minter, "o1"); err == nil {
				t.Fatalf("the minter is still an org member (%+v) — then this is not the window the test describes", om)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("the mint of a role carrying no org role, whose creator left the org before its row landed: code=%d body=%s, want 200",
					w.Code, w.Body.String())
			}
			// The row, and with it the token the accept takes: a capability
			// that carries no org rank is handed over, not withdrawn, when its
			// creator leaves.
			invs, err := real.ListInvitationsByTeam(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != 1 || invs[0].Email != c.email || invs[0].Role != identity.RoleConfigEditor {
				t.Fatalf("the invitation the rule leaves standing is %+v, want the one this mint created for %s", invs, c.email)
			}
			// And nothing took it behind the answer either: the confirmation's
			// own revoke is detached from the request, so the row it would have
			// taken is read past the quiet window.
			time.Sleep(auditQuietWindow)
			if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 0 {
				t.Errorf("%d invitation.deleted rows landed on an invitation the rule leaves: %+v", len(got), got)
			}
			if _, err := real.GetInvitation(ctx, invs[0].ID); err != nil {
				t.Errorf("the invitation the rule leaves was revoked behind the answer: %v", err)
			}
		})
	}
}

// The row the removal takes on BOTH sides of its sweep: a role mirrored up
// to NOTHING addressed to the MINTER's own address. The removal's
// addressed-to half takes such a row where its listing reaches it
// (revokeOrgTeamAccess), so the mint's confirmation has to take the one that
// lands after it, or the same invitation stands or falls by which side of the
// sweep its row happened to land on — and what it stands as is a re-entry
// ticket the off-boarded member minted for themselves: accepting it writes
// them a grant in a team of the org they were just removed from. The refusal
// names the address rather than a rank, since the role carries none.
//
// What the half is gated on is that the minter can no longer REACH that team:
// no membership row in the org AND no grant in the team. The minter who still
// holds the grant is the orphan-grant holder minting for their own address,
// whose accept gains them nothing and whose state the console reports rather
// than refuses — measured by
// TestTeamMembers_anOrphanGrantLaunderedThroughItsOwnInvitationStaysOnTheDriftReport,
// which this rule must leave standing.
func TestTeamInvitations_aMintStraddlingTheRemovalLosesTheRowAddressedToItsOwnCreator(t *testing.T) {
	for _, c := range []struct {
		name, minter, email string
		seed                func(*testing.T, *Server)
		mintCtx, removerCtx func() context.Context
		mint                func(*Server, context.Context, string) *errorRecorder
	}{
		{
			// A t1 admin and a plain member of o1, off-boarded by the org
			// admin as the roster does.
			name: "the team route", minter: racerID, email: "racer@example.com",
			seed:       seedRacerAsTeamAdmin,
			mintCtx:    func() context.Context { return auth.WithIdentity(context.Background(), auth.Identity{UserID: racerID}) },
			removerCtx: orgAdminCtx,
			mint: func(s *Server, ctx context.Context, email string) *errorRecorder {
				w := newErrorRecorder()
				s.handleCreateInvitation(w, teamReq(ctx, "POST", "/api/teams/t1/invitations",
					`{"email":"`+email+`","role":"config_editor"}`, "t1", ""))
				return w
			},
		},
		{
			// The org route's minter manages the org, so a platform admin
			// off-boards them. Their account row is what names the address
			// the invitation is addressed to.
			name: "the org route", minter: "orgadmin", email: "orgadmin@example.com",
			seed:    func(t *testing.T, s *Server) { seedAccount(t, s, "orgadmin", "orgadmin@example.com") },
			mintCtx: orgAdminCtx, removerCtx: superAdminCtx,
			mint: func(s *Server, ctx context.Context, email string) *errorRecorder {
				w := newErrorRecorder()
				s.handleCreateOrgInvitation(w, teamReq(ctx, "POST", "/api/orgs/o1/invitations",
					`{"email":"`+email+`","role":"config_editor","team_id":"t1"}`, "o1", ""))
				return w
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			s.auditStore = audit.NewMemoryStore()
			ctx := context.Background()
			real := s.authStore()
			c.seed(t, s)
			gate := &mintParkGate{Store: real, email: c.email,
				reached: make(chan struct{}), release: make(chan struct{})}
			s.authSvc.SetStoreForTest(gate)

			done := make(chan *errorRecorder, 1)
			go func() { done <- c.mint(s, c.mintCtx(), c.email) }()
			awaitGate(t, gate.reached, "the mint parked just before its insert")

			off := newErrorRecorder()
			s.handleRemoveOrgMember(off, teamReq(c.removerCtx(), "DELETE", "/api/orgs/o1/members/"+c.minter, "", "o1", c.minter))
			if off.Code != http.StatusNoContent {
				t.Fatalf("the removal that ran inside the mint: code=%d body=%s, want 204", off.Code, off.Body.String())
			}
			close(gate.release)
			w := <-done
			s.authSvc.SetStoreForTest(real)

			if om, err := real.GetOrgMembership(ctx, c.minter, "o1"); err == nil {
				t.Fatalf("the minter is still an org member (%+v) — then this is not the window the test describes", om)
			}
			if w.Code != http.StatusForbidden {
				t.Fatalf("the mint of a row addressed to its own creator, who left the org before it landed: code=%d body=%s, want 403",
					w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, want := range []string{
				"addressed to its creator's own address",
				"your own org role in this team's organization is none by the time it was minted",
				"would seat you back in a team of it",
				"the invitation this request minted was revoked",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal does not name %q: %s", want, body)
				}
			}
			// The row, and with it the token an accept would take: the
			// off-boarded member has nothing left to accept.
			invs, err := real.ListInvitationsByTeam(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			for _, inv := range invs {
				if inv.Email == c.email {
					t.Errorf("the refused mint left the row addressed to its own creator standing: %+v", inv)
				}
			}
			rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
			if got := rows[0].Meta["reason"]; got != "creator_standing_dropped" {
				t.Errorf("the audit line of the revoked invitation names %v, want creator_standing_dropped", got)
			}
			if got := rows[0].Meta["email"]; got != c.email {
				t.Errorf("the audit line does not name the address the revoked invitation held: %v", got)
			}
		})
	}
}

// removalPass2GrantListGate runs a hook at the removal's SECOND listing of
// the target's team grants — the one pass 2 makes after its own invitation
// sweep and before its grant revoke. The org admin's authorization reads
// GetOrgMembership and the invitation half reads teams and invitations, so
// the only two hits on this call are the two passes.
type removalPass2GrantListGate struct {
	identity.Store
	userID string

	mu   sync.Mutex
	hits int
	hook func()
}

func (g *removalPass2GrantListGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	if userID == g.userID {
		g.mu.Lock()
		g.hits++
		run := g.hits == 2 && g.hook != nil
		hook := g.hook
		if run {
			g.hook = nil
		}
		g.mu.Unlock()
		if run {
			hook()
		}
	}
	return g.Store.ListMembershipsByUser(ctx, userID)
}

// The window INSIDE a pass, which that pass's own sweeps cannot cover and the
// mint's confirmation KEEPS a row in: the invitation half runs before the
// grant revoke, so a self-addressed row minted between the two is read by the
// confirmation while the minter's grant is still there — the orphan-grant
// state it sanctions — and the pass revokes that grant one store call later.
// What the removed member is left holding is then their own invitation into a
// team of the org they were just removed from, at a role carrying no org rank
// so no cap measures it, with no grant and no membership row: a re-entry
// ticket behind a 204.
//
// What answers it is the removal's ORDER rather than a fourth read: the
// invitation sweep runs once MORE, after every other write the removal makes
// (handleRemoveOrgMember), so a 204 means no row this rule takes stands
// behind it. The audit cause is what says which side took the row.
func TestTeamInvitations_aMintInsideTheRemovalsSecondPassLosesItsOwnReEntryTicket(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})

	var mint *errorRecorder
	var keptID string
	gate := &removalPass2GrantListGate{Store: real, userID: racerID}
	gate.hook = func() {
		// The grant written back inside the window the second pass exists
		// for: a placement or a role change that read the membership while
		// it was still live writes its grant here, and the store has no
		// conditional write, so that handler's own re-read is what takes it.
		if err := real.UpsertMembership(ctx, identity.Membership{
			UserID: racerID, TeamID: "t1", Role: identity.RoleAdmin, JoinedAt: time.Now(),
		}); err != nil {
			t.Error(err)
			return
		}
		mint = newErrorRecorder()
		s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
			`{"email":"racer@example.com","role":"config_editor"}`, "t1", ""))
		if mint.Code != http.StatusOK {
			t.Errorf("the mint inside the second pass: code=%d body=%s, want 200 — the confirmation keeps the row of a minter who still holds the grant",
				mint.Code, mint.Body.String())
			return
		}
		// The premise, asserted where it holds: at this point, inside the
		// pass, the row the confirmation KEPT is standing.
		invs, err := real.ListInvitationsByTeam(ctx, "t1")
		if err != nil {
			t.Error(err)
			return
		}
		if len(invs) != 1 {
			t.Errorf("%d invitations stand inside the pass, want the one the confirmation kept: %+v", len(invs), invs)
			return
		}
		keptID = invs[0].ID
	}
	s.authSvc.SetStoreForTest(gate)
	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)

	if off.Code != http.StatusNoContent {
		t.Fatalf("the removal: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	if gate.hits < 2 || keptID == "" {
		t.Fatalf("the mint did not land inside the second pass (grant listings=%d, row kept=%q) — then this is not the window the test describes",
			gate.hits, keptID)
	}
	// The state that makes such a row a ticket rather than a hand-over: no
	// membership row in the org, and no grant in the team either.
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatalf("the member is still an org member (%+v) — then this is not the state the test describes", om)
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Fatalf("the member still holds the t1 grant (%+v) — then the row is no re-entry ticket", mb)
	}
	if inv, err := real.GetInvitation(ctx, keptID); err == nil {
		t.Errorf("RE-ENTRY TICKET: the removal answered 204 with %+v standing, addressed to the member it removed and minted by them", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the row after the removal: %v", err)
	}
	// Which side took it: the removal's own last sweep (org_member_removed),
	// not the mint's confirmation (creator_standing_dropped) — the
	// confirmation kept it, which is the premise above.
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "org_member_removed" {
		t.Errorf("the row was taken under cause %v, want org_member_removed: the removal's last sweep is what has to take it", got)
	}
	// And the consequence, from the token the mint handed out: there is
	// nothing left to accept.
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(mint.Body.Bytes(), &minted); err != nil || minted.Token == "" {
		t.Fatalf("the mint answered no token: %v %s", err, mint.Body.String())
	}
	if mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, minted.Token); err == nil {
		t.Errorf("the off-boarded member accepted their own invitation and holds %+v", mb)
	}
}

// finalInvitationSweepFailGate arms, at the removal's LAST delete of the
// membership row, both halves of one scenario: a self-addressed invitation
// that lands where only the removal's final invitation sweep could take it,
// and a listing failure that stops that sweep from reading it. The row is
// written straight to the store because no route can land one there — the
// window is between two writes of the same handler.
type finalInvitationSweepFailGate struct {
	identity.Store
	userID, orgID string
	seed          func()

	mu      sync.Mutex
	deletes int
	armed   bool
}

func (g *finalInvitationSweepFailGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	err := g.Store.DeleteOrgMembership(ctx, userID, orgID)
	if userID != g.userID || orgID != g.orgID {
		return err
	}
	g.mu.Lock()
	g.deletes++
	arm := g.deletes == 2
	seed := g.seed
	if arm {
		g.armed = true
		g.seed = nil
	}
	g.mu.Unlock()
	if arm && seed != nil {
		seed()
	}
	return err
}

func (g *finalInvitationSweepFailGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	g.mu.Lock()
	armed := g.armed
	g.mu.Unlock()
	if armed {
		return nil, errors.New("identity: list invitations: connection reset by peer")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

// The removal's LAST write has a failure of its own, and it is the one that
// decides whether a 204 may be answered at all: a sweep that could not READ
// the invitations is not one that found none, so its error joins the second
// pass's — the body that reports the grants revoked "with the pending
// invitations they minted here" must not be the answer a failed invitation
// sweep gives. The state asserted is the one the body describes: the member
// removed, the grants gone, and a row addressed to them standing — and the
// re-issue it asks for takes that row.
func TestOrgOffboarding_aFailedLastInvitationSweepSaysSoAndFinishesOnTheReissue(t *testing.T) {
	s := newOrgCredsTestServer(t)
	log := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelInfo, log)
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()

	const strandedID = "stranded-invitation"
	gate := &finalInvitationSweepFailGate{Store: real, userID: racerID, orgID: "o1"}
	gate.seed = func() {
		// A row somebody ELSE addressed to the removed member, landing after
		// both passes: the addressed-to half takes it whatever its role.
		if err := real.CreateInvitation(ctx, identity.Invitation{
			ID: strandedID, TeamID: "t1", Email: "racer@example.com",
			Role: identity.RoleConfigEditor, InvitedBy: "orgadmin",
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		}); err != nil {
			t.Error(err)
		}
	}
	s.authSvc.SetStoreForTest(gate)
	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)

	if off.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose last invitation sweep failed: code=%d body=%s, want 500", off.Code, off.Diagnostic(t))
	}
	body := off.Diagnostic(t)
	for _, want := range []string{
		"the last sweep of their pending invitations failed",
		"connection reset by peer",
		"a pending invitation they minted here that carries an org role",
		"addressed to them",
		"re-issue the removal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not say what may be left (missing %q): %s", want, body)
		}
	}
	// And it does not say what IS done: both passes went through on this
	// exit, so a body routing this failure into the grant sweep's words
	// would send the operator looking for access that is gone.
	for _, forbidden := range []string{
		"they may still hold a team grant in this org",
		"re-sweeping their team access failed",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the 500 of a failed invitation sweep names a grant that is gone (%q): %s", forbidden, body)
		}
	}
	// The state the body names: the member IS removed and the grants ARE
	// gone — both passes went through — and the row is what stands.
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Errorf("the membership row stands (%+v) — then this is not the exit the test describes", om)
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Errorf("the t1 grant stands (%+v) — then this is not the exit the test describes", mb)
	}
	if _, err := real.GetInvitation(ctx, strandedID); err != nil {
		t.Fatalf("the row the failure left is already gone (%v) — then this is not the exit the test describes", err)
	}
	if got := log.String(); !strings.Contains(got, "the last sweep of their pending invitations failed") {
		t.Errorf("the log does not name the sweep that failed: %s", got)
	}
	// And the advice works: the removal is idempotent, so re-issuing it takes
	// the row the failure left.
	again := newErrorRecorder()
	s.handleRemoveOrgMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if again.Code != http.StatusNoContent {
		t.Fatalf("the re-issued removal: code=%d body=%s, want 204", again.Code, again.Diagnostic(t))
	}
	if inv, err := real.GetInvitation(ctx, strandedID); err == nil {
		t.Errorf("the re-issued removal left the row the failure stranded: %+v", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("reading the row after the re-issue: %v", err)
	}
}

// standingDropsAsTheMintLandsGate lowers the minter's own org standing at the
// instant their invitation row lands — the same window in one deterministic
// order, with no sweep to take the row either, which is the residual the
// confirmation exists for. failRevoke puts the confirmation on the exit where
// its own delete fails too.
type standingDropsAsTheMintLandsGate struct {
	identity.Store
	userID, orgID string
	to            identity.OrgRole
	// revokeErr is what the store answers the confirmation's own delete. An
	// ErrNotFound takes the row first, the way the sweep of the write that
	// lowered the standing would have; any other error takes nothing, the
	// way a store that cannot serve the delete answers.
	revokeErr error

	mu   sync.Mutex
	done bool
}

func (g *standingDropsAsTheMintLandsGate) CreateInvitation(ctx context.Context, inv identity.Invitation) error {
	if err := g.Store.CreateInvitation(ctx, inv); err != nil {
		return err
	}
	g.mu.Lock()
	first := !g.done
	g.done = true
	g.mu.Unlock()
	if !first {
		return nil
	}
	return g.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: g.userID, OrgID: g.orgID, Role: g.to, JoinedAt: time.Now(),
	})
}

func (g *standingDropsAsTheMintLandsGate) DeleteInvitation(ctx context.Context, id string) error {
	if g.revokeErr == nil {
		return g.Store.DeleteInvitation(ctx, id)
	}
	if errors.Is(g.revokeErr, identity.ErrNotFound) {
		if err := g.Store.DeleteInvitation(ctx, id); err != nil {
			return err
		}
	}
	return g.revokeErr
}

// What the confirmation must NOT refuse, measured in the same window: a role
// mirrored up to NOTHING (config_editor, ADR-078) carries no standing for the
// minted half to measure — and the minter here KEEPS a membership row, which
// is what the addressed-to half needs gone — a team with no parent org has no
// boundary to read, and a platform admin holds no org membership for a cap to
// read. Each mints and keeps its row though the minter's standing dropped as
// it landed.
func TestTeamInvitations_theMintsTheConfirmationLetsThrough(t *testing.T) {
	for _, c := range []struct {
		name, teamID, role string
		ctx                func() context.Context
	}{
		{"a role mirrored up to nothing", "t1", "config_editor", orgAdminCtx},
		{"a team with no parent org", "tnone", "admin", orgAdminCtx},
		{"a platform admin", "t1", "admin", superAdminCtx},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			ctx := context.Background()
			real := s.authStore()
			if _, err := real.CreateTeam(ctx, identity.Team{
				ID: "tnone", Name: "tnone", Slug: "tnone", CreatedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := real.UpsertMembership(ctx, identity.Membership{
				UserID: "orgadmin", TeamID: "tnone", Role: identity.RoleAdmin, JoinedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			s.authSvc.SetStoreForTest(&standingDropsAsTheMintLandsGate{
				Store: real, userID: "orgadmin", orgID: "o1", to: identity.OrgRoleMember,
			})
			w := newErrorRecorder()
			s.handleCreateInvitation(w, teamReq(c.ctx(), "POST", "/api/teams/"+c.teamID+"/invitations",
				`{"email":"newcomer@example.com","role":"`+c.role+`"}`, c.teamID, ""))
			s.authSvc.SetStoreForTest(real)
			if w.Code != http.StatusOK {
				t.Fatalf("the mint the confirmation must let through: code=%d body=%s, want 200", w.Code, w.Body.String())
			}
			var minted struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil || minted.ID == "" {
				t.Fatalf("the mint answered no invitation: %v %s", err, w.Body.String())
			}
			if _, err := real.GetInvitation(ctx, minted.ID); err != nil {
				t.Errorf("the confirmation revoked an invitation it exempts: %v", err)
			}
		})
	}
}

// standingUnreadableAfterTheMintGate answers every read of the minter's org
// membership until their invitation row lands, and fails them from there on
// — the store that goes away between the cap and its confirmation. It keys on
// the row having landed rather than on a read COUNT, which the guards before
// it shift.
type standingUnreadableAfterTheMintGate struct {
	identity.Store
	userID, orgID string

	mu     sync.Mutex
	minted bool
}

func (g *standingUnreadableAfterTheMintGate) CreateInvitation(ctx context.Context, inv identity.Invitation) error {
	if err := g.Store.CreateInvitation(ctx, inv); err != nil {
		return err
	}
	g.mu.Lock()
	g.minted = true
	g.mu.Unlock()
	return nil
}

func (g *standingUnreadableAfterTheMintGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	g.mu.Lock()
	landed := g.minted
	g.mu.Unlock()
	if landed && userID == g.userID && orgID == g.orgID {
		return identity.OrgMembership{}, errors.New("identity: get org membership: connection reset by peer")
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// A cap that cannot be CONFIRMED is not one: the invitation is taken back and
// the mint answers 500 with the store's own error, where the cap's own read
// failing answers 500 before any row exists.
func TestTeamInvitations_aStandingUnreadableAfterTheMintTakesTheInvitationBack(t *testing.T) {
	// At every rung, the one with no mirror at all included: a standing that
	// cannot be re-read is not one that is still there, whichever org role
	// the invitation would be mirrored up to — and a role mirrored up to
	// NOTHING is measured against that standing too, since the removal's
	// addressed-to half fires at a standing that is GONE and an unreadable
	// one cannot be told apart from it.
	for _, role := range []identity.Role{identity.RoleAdmin, identity.RoleMember, identity.RoleConfigEditor} {
		t.Run("a "+string(role)+" mint", func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			s.auditStore = audit.NewMemoryStore()
			log := &lockedBuffer{}
			s.logger = iterlog.New(iterlog.LevelInfo, log)
			ctx := context.Background()
			real := s.authStore()
			s.authSvc.SetStoreForTest(&standingUnreadableAfterTheMintGate{Store: real, userID: "orgadmin", orgID: "o1"})

			w := newErrorRecorder()
			s.handleCreateInvitation(w, teamReq(orgAdminCtx(), "POST", "/api/teams/t1/invitations",
				`{"email":"newcomer@example.com","role":"`+string(role)+`"}`, "t1", ""))
			s.authSvc.SetStoreForTest(real)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("a mint whose cap cannot be confirmed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
			}
			body := w.Diagnostic(t)
			for _, want := range []string{
				"connection reset by peer",
				// The STANDING, not the org role: the confirmation measures
				// a membership row for a role that mirrors up to no org role
				// at all, and this exit is the one both role shapes share.
				"confirming your own org standing in this team's organization",
				"a cap that cannot be confirmed is not one",
				"the invitation this request minted was revoked",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the 500 does not name %q: %s", want, body)
				}
			}
			invs, err := real.ListInvitationsByTeam(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != 0 {
				t.Errorf("the unconfirmed mint left its invitation standing: %+v", invs)
			}
			// Said where it survives the caller too: the client going away is the
			// ordinary reason a standing cannot be re-confirmed, and then the log is
			// the only account of the invitation this request took back.
			if got := log.String(); !strings.Contains(got, "confirming the org standing of orgadmin in org o1 after invitation") {
				t.Errorf("the log does not name the confirmation that failed: %s", got)
			}
			// And the trail says which of the two exits took the row: here the
			// standing did NOT drop — the row still carries admin — it could not be
			// READ, so an operator querying the trail for a standing that dropped
			// must not find this revoke among those.
			if om, err := real.GetOrgMembership(ctx, "orgadmin", "o1"); err != nil || om.Role != identity.OrgRoleAdmin {
				t.Fatalf("the membership row is %+v (%v), want the admin it still holds — then this is not the exit the test describes", om, err)
			}
			rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
			if got := rows[0].Meta["reason"]; got != "creator_standing_unconfirmed" {
				t.Errorf("the audit line of the invitation taken back names %v, want creator_standing_unconfirmed: the standing was not read as lower, it could not be read", got)
			}
			if got := rows[0].Meta["email"]; got != "newcomer@example.com" {
				t.Errorf("the audit line does not name the address the revoked invitation held: %v", got)
			}
		})
	}
}

// grantUnreadableAfterTheMintGate fails the Nth read of one account's grant
// in one team. The mint path reads it twice for a caller the team-admin check
// admits on that grant: once for the check itself, which must go through or
// there is no mint to confirm, and once for the confirmation's own question
// (ownTeamGrantGone).
type grantUnreadableAfterTheMintGate struct {
	identity.Store
	userID, teamID string
	failOn         int

	mu   sync.Mutex
	hits int
}

func (g *grantUnreadableAfterTheMintGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if userID == g.userID && teamID == g.teamID {
		g.mu.Lock()
		g.hits++
		fail := g.hits == g.failOn
		g.mu.Unlock()
		if fail {
			return identity.Membership{}, errors.New("identity: get membership: connection reset by peer")
		}
	}
	return g.Store.GetMembership(ctx, userID, teamID)
}

// The GRANT read, on its failure path. It is the fact that separates the
// member a removal took out from the holder of an orphan grant minting for
// their own address, so one that cannot be made leaves the confirmation
// unable to tell the row it must take from the row it must keep: the
// invitation goes back and the mint answers 500 — naming THAT read, not the
// standing, which was read fine — under its own audit cause. A retry on a
// healthy store mints the row the platform sanctions.
func TestTeamInvitations_aGrantUnreadableAfterTheMintTakesTheInvitationBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	log := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelInfo, log)
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	real := s.authStore()
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})

	gate := &grantUnreadableAfterTheMintGate{Store: real, userID: racerID, teamID: "t1", failOn: 2}
	s.authSvc.SetStoreForTest(gate)
	w := newErrorRecorder()
	s.handleCreateInvitation(w, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
		`{"email":"racer@example.com","role":"config_editor"}`, "t1", ""))
	s.authSvc.SetStoreForTest(real)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a mint whose grant read failed after the row landed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	if gate.hits < 2 {
		t.Fatalf("the grant was read %d times — then the confirmation did not ask for it and this is not the exit the test describes", gate.hits)
	}
	body := w.Diagnostic(t)
	for _, want := range []string{
		"connection reset by peer",
		// The read that actually failed, in the refusal's own words and in
		// the store error it carries.
		"whether you still hold a grant in the team this invitation joins",
		"resolve the team t1 grant of " + racerID,
		"a cap that cannot be confirmed is not one",
		"the invitation this request minted was revoked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not name %q: %s", want, body)
		}
	}
	if strings.Contains(body, "your own org standing in this team's organization") {
		t.Errorf("the 500 blames the org standing, which was read fine: %s", body)
	}
	if invs, err := real.ListInvitationsByTeam(ctx, "t1"); err != nil {
		t.Fatal(err)
	} else if len(invs) != 0 {
		t.Errorf("the unconfirmed mint left its invitation standing: %+v", invs)
	}
	if got := log.String(); !strings.Contains(got, "confirming the team grant of "+racerID+" in org o1 after invitation") {
		t.Errorf("the log does not name the read that failed: %s", got)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_grant_unconfirmed" {
		t.Errorf("the audit line of the invitation taken back names %v, want creator_team_grant_unconfirmed: one cause for three reads cannot be queried for either", got)
	}
	// The retry the body asks for, on a healthy store: the orphan-grant
	// holder's self-addressed row is a state the console reports rather than
	// one the mint refuses.
	again := newErrorRecorder()
	s.handleCreateInvitation(again, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
		`{"email":"racer@example.com","role":"config_editor"}`, "t1", ""))
	if again.Code != http.StatusOK {
		t.Errorf("the retry on a healthy store: code=%d body=%s, want 200", again.Code, again.Diagnostic(t))
	}
}

// addressUnreadableAfterTheMintGate fails one account's own row once the test
// arms it. The removal's cascade reads that row too (readOwnInvitationAddress,
// three times), so arming it before the removal has returned would fail the
// cascade instead of the confirmation.
type addressUnreadableAfterTheMintGate struct {
	identity.Store
	userID string

	mu    sync.Mutex
	armed bool
}

func (g *addressUnreadableAfterTheMintGate) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

func (g *addressUnreadableAfterTheMintGate) GetUser(ctx context.Context, userID string) (identity.User, error) {
	g.mu.Lock()
	fail := g.armed && userID == g.userID
	g.mu.Unlock()
	if fail {
		return identity.User{}, errors.New("identity: get user: connection reset by peer")
	}
	return g.Store.GetUser(ctx, userID)
}

// The ADDRESS read, on its failure path — reachable only through the
// straddle, since a caller with no membership row in the org AND no grant in
// the team could not have been authorized to mint at all: the mint parks
// before its insert, the removal runs through, and only then does the account
// row become unreadable. The address is the whole addressed-to half, so one
// that cannot be read is a half that cannot be applied: the invitation goes
// back, and the 500 and the audit cause name THAT read.
func TestTeamInvitations_anAddressUnreadableAfterTheMintTakesTheInvitationBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	log := &lockedBuffer{}
	s.logger = iterlog.New(iterlog.LevelInfo, log)
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})

	park := &mintParkGate{Store: real, email: "racer@example.com",
		reached: make(chan struct{}), release: make(chan struct{})}
	gate := &addressUnreadableAfterTheMintGate{Store: park, userID: racerID}
	s.authSvc.SetStoreForTest(gate)

	done := make(chan *errorRecorder, 1)
	go func() {
		w := newErrorRecorder()
		s.handleCreateInvitation(w, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
			`{"email":"racer@example.com","role":"config_editor"}`, "t1", ""))
		done <- w
	}()
	awaitGate(t, park.reached, "the mint parked just before its insert")

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	if off.Code != http.StatusNoContent {
		t.Fatalf("the removal that ran inside the mint: code=%d body=%s, want 204", off.Code, off.Diagnostic(t))
	}
	gate.arm()
	close(park.release)
	w := <-done
	s.authSvc.SetStoreForTest(real)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a mint whose address read failed after the row landed: code=%d body=%s, want 500", w.Code, w.Diagnostic(t))
	}
	body := w.Diagnostic(t)
	for _, want := range []string{
		"connection reset by peer",
		"the address your own pending invitations are addressed to",
		"resolve the address invitations of " + racerID + " are addressed to",
		"a cap that cannot be confirmed is not one",
		"the invitation this request minted was revoked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not name %q: %s", want, body)
		}
	}
	if strings.Contains(body, "your own org standing in this team's organization") {
		t.Errorf("the 500 blames the org standing, which was read fine: %s", body)
	}
	if invs, err := real.ListInvitationsByTeam(ctx, "t1"); err != nil {
		t.Fatal(err)
	} else if len(invs) != 0 {
		t.Errorf("the unconfirmed mint left its invitation standing: %+v", invs)
	}
	if got := log.String(); !strings.Contains(got, "confirming the invitation address of "+racerID+" in org o1 after invitation") {
		t.Errorf("the log does not name the read that failed: %s", got)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_address_unconfirmed" {
		t.Errorf("the audit line of the invitation taken back names %v, want creator_address_unconfirmed", got)
	}
}

// What the confirmation's own revoke answers, both ways. A revoke the store
// could not serve leaves the row at a rank its creator does not hold — no
// silent fallback: the refusal names the invitation and asks for it to be
// revoked by hand, and the log carries it where the response may reach
// nobody. A row the sweep of the very write already took reports itself
// revoked, with no second audit line.
func TestTeamInvitations_whatTheConfirmationsRevokeAnswers(t *testing.T) {
	for _, c := range []struct {
		name      string
		revokeErr error
		stands    int
		wants     []string
	}{
		{
			name:      "a store that cannot serve the delete",
			revokeErr: errors.New("identity: delete invitation: connection reset by peer"),
			stands:    1,
			wants: []string{
				"revoking the invitation this request minted failed too",
				"connection reset by peer",
				"revoke it by hand",
			},
		},
		{
			name:      "a row the sweep already took",
			revokeErr: identity.ErrNotFound,
			stands:    0,
			wants:     []string{"the invitation this request minted is already revoked"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			counted := &countingAudit{Store: audit.NewMemoryStore()}
			s.auditStore = counted
			log := &lockedBuffer{}
			s.logger = iterlog.New(iterlog.LevelInfo, log)
			ctx := context.Background()
			real := s.authStore()
			s.authSvc.SetStoreForTest(&standingDropsAsTheMintLandsGate{
				Store: real, userID: "orgadmin", orgID: "o1",
				to: identity.OrgRoleMember, revokeErr: c.revokeErr,
			})
			w := newErrorRecorder()
			s.handleCreateInvitation(w, teamReq(orgAdminCtx(), "POST", "/api/teams/t1/invitations",
				`{"email":"newcomer@example.com","role":"admin"}`, "t1", ""))
			s.authSvc.SetStoreForTest(real)
			if w.Code != http.StatusForbidden {
				t.Fatalf("a mint whose standing dropped: code=%d body=%s, want 403", w.Code, w.Body.String())
			}
			invs, err := real.ListInvitationsByTeam(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != c.stands {
				t.Fatalf("%d invitations stand, want %d: %+v", len(invs), c.stands, invs)
			}
			body := w.Body.String()
			wants := c.wants
			if c.stands == 1 {
				wants = append(wants, invs[0].ID)
			}
			for _, want := range wants {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal does not name %q: %s", want, body)
				}
			}
			// The trail, read past the quiet window rather than on the
			// tick the handler returned on: the mint's own
			// invitation.created line and nothing else — a row this
			// confirmation did not take carries no audit line of its own.
			awaitAuditQuiet(t, counted, 1)
			if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 0 {
				t.Errorf("a revoke that took nothing wrote %d audit lines: %+v", len(got), got)
			}
			if c.stands == 1 && !strings.Contains(log.String(), "stands at a rank its creator does not hold") {
				t.Errorf("the log does not carry the row the refusal could not take: %s", log.String())
			}
		})
	}
}

// mintCancelGate lowers the minter's own org standing as their invitation row
// lands and hangs the request up with it, then refuses every store call whose
// context is already done — what a driver does, and what MemoryStore, which
// discards ctx, cannot show.
type mintCancelGate struct {
	identity.Store
	userID, orgID string
	cancel        func()

	mu   sync.Mutex
	done bool
}

func (g *mintCancelGate) CreateInvitation(ctx context.Context, inv identity.Invitation) error {
	if err := g.Store.CreateInvitation(ctx, inv); err != nil {
		return err
	}
	g.mu.Lock()
	first := !g.done
	g.done = true
	g.mu.Unlock()
	if !first {
		return nil
	}
	if err := g.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: g.userID, OrgID: g.orgID, Role: identity.OrgRoleMember, JoinedAt: time.Now(),
	}); err != nil {
		return err
	}
	g.cancel()
	return nil
}

func (g *mintCancelGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if err := ctx.Err(); err != nil {
		return identity.OrgMembership{}, fmt.Errorf("identity: get org membership: %w", err)
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

func (g *mintCancelGate) DeleteInvitation(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("identity: delete invitation: %w", err)
	}
	return g.Store.DeleteInvitation(ctx, id)
}

// ctxAwareAudit refuses an insert whose context is already done, the way a
// driver does — where audit.MemoryStore discards ctx entirely and cannot tell
// an insert detached from the request from one riding it.
type ctxAwareAudit struct {
	audit.Store
}

func (a *ctxAwareAudit) Insert(ctx context.Context, e audit.Event) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return a.Store.Insert(ctx, e)
}

// The standing has already dropped when the confirmation runs, so a client
// that hangs up in between must take neither of its two steps with it: the
// re-read and the revoke each run on a context detached from the request. A
// revoke that died with the client would leave standing exactly the
// invitation the confirmation exists to remove, and a re-read that died with
// it would answer 500 on a standing it could have read. The sibling on the
// other half of the pair, for the sweep of the write that lowers a standing,
// is TestOrgMembers_aClientGoneAfterTheRoleWriteStillGetsTheInvitationSweep.
func TestTeamInvitations_aClientGoneAtTheMintStillLosesTheUnconfirmedInvitation(t *testing.T) {
	s := newOrgCredsTestServer(t)
	bg := context.Background()
	real := s.authStore()
	ctx, cancel := context.WithCancel(orgAdminCtx())
	defer cancel()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	s.authSvc.SetStoreForTest(&mintCancelGate{Store: real, userID: "orgadmin", orgID: "o1", cancel: cancel})

	w := newErrorRecorder()
	s.handleCreateInvitation(w, teamReq(ctx, "POST", "/api/teams/t1/invitations",
		`{"email":"accomplice@example.com","role":"admin"}`, "t1", ""))
	s.authSvc.SetStoreForTest(real)

	if om, err := real.GetOrgMembership(bg, "orgadmin", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the standing did not drop: %+v (%v) — then this is not the window the test describes", om, err)
	}
	invs, err := real.ListInvitationsByTeam(bg, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Errorf("the invitation whose cap could not be confirmed outlived the request (%+v) — the revoke died with the client (the mint answered %d: %s)",
			invs, w.Code, w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("the mint answered %d: %s — want 403 in the cap's own words: a standing that is READABLE is still read once the client is gone",
			w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "is member by the time it was minted") {
		t.Errorf("the refusal does not name the standing the re-read found: %s", body)
	}
	// The third step of the same exit, and the only account the client cannot
	// be given: the audit line of the revoke. Its insert runs on a context of
	// its own (auditWrite), so a store that refuses a done one still carries
	// the row and the cause it names.
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_standing_dropped" {
		t.Errorf("the audit line of the revoke names %v, want creator_standing_dropped", got)
	}
}

// What the counts of these scenarios rest on: the two helpers that read a
// tenant's trail answer the rows they are asked for, all of them, whatever
// else that trail holds. A read taken on the store's default page answers
// from the 50 NEWEST rows (audit.ClampLimit) of what it selects, which is a
// false RED on the row a scenario asserts on — and, one step further, a
// capped page read as exactly the count it expected. Each helper is held
// against noise its own selector KEEPS: the same action for the one that
// filters by action in the store, the same action on another target for the
// one that selects by target in Go.
func TestAuditHelpers_readPastTheStoresDefaultPage(t *testing.T) {
	st := audit.NewMemoryStore()
	ctx := context.Background()
	base := time.Now().UTC()
	// The rows the scenarios are about, oldest: one selected by action, one
	// by target — the two shapes the helpers select on.
	for _, e := range []audit.Event{
		{ID: "revoked", Scope: audit.ScopeTenant, TenantID: "t1",
			Action: "invitation.deleted", TargetID: "inv-1", CreatedAt: base},
		{ID: "removed", Scope: audit.ScopeTenant, TenantID: "t1",
			Action: "member.removed", TargetID: "inv-1", CreatedAt: base},
	} {
		if err := st.Insert(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// Newer rows of the action under test, on another target, filling the
	// store's default page on their own: the action filter is the STORE's and
	// runs BEFORE the clamp, so noise of ANOTHER action would be selected out
	// and leave the limit dead — only a page past that clamp still reaches
	// the oldest row, which is the one each scenario asserts on.
	for i := 0; i < audit.ClampLimit(0); i++ {
		if err := st.Insert(ctx, audit.Event{
			ID: fmt.Sprintf("noise-%d", i), Scope: audit.ScopeTenant, TenantID: "t1",
			Action: "invitation.deleted", TargetID: "inv-noise",
			CreatedAt: base.Add(time.Duration(i+1) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	deleted := tenantAuditRows(t, st, "t1", "invitation.deleted")
	oldest := ""
	if len(deleted) > 0 {
		oldest = deleted[len(deleted)-1].ID
	}
	if len(deleted) != 1+audit.ClampLimit(0) || oldest != "revoked" {
		t.Errorf("tenantAuditRows reads %d invitation.deleted rows of a trail that holds %d, oldest %q: the row a scenario asserts on is the one the store's default page drops",
			len(deleted), 1+audit.ClampLimit(0), oldest)
	}
	got := auditRows(t, &Server{auditStore: st}, "t1", "inv-1", "invitation.deleted", "member.removed")
	if len(got) != 2 {
		t.Errorf("auditRows reads %v of the two rows that name target inv-1, behind %d newer rows of another target",
			auditActionsOf(got), audit.ClampLimit(0))
	}
}

// Whoever withdraws the premise sweeps the rows kept on it. The mint's own
// confirmation keeps a self-addressed invitation whose creator holds no org
// membership row on ONE fact: their grant in the team the invitation joins
// (standingAfterMintTakesInvitation). The removal cascade may take that
// grant, but it is not the only write that does: DELETE
// /api/teams/{id}/members/{user_id}, the compensation that unwinds a grant
// write and the GitHub SSO reconciliation of a login the allow-list no
// longer grants the team take it with no removal in flight, and a row left
// behind is a re-entry ticket into the team the grant was just taken from.

// The team-member DELETE, on the state the console reports as an orphan
// grant: the holder minted an invitation to their own address, which the
// confirmation kept on that grant, and the natural cleanup of the drift is
// the request that takes the grant away.
func TestTeamMembers_removingTheGrantTakesTheRowItsOwnMintKeptOnIt(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, token := mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the mint's confirmation did not keep the row on the grant (%v) — then this is not the state the test describes", err)
	}

	drop := newErrorRecorder()
	s.handleRemoveMember(drop, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
	if drop.Code != http.StatusNoContent {
		t.Fatalf("removing the orphan grant: code=%d body=%s, want 204", drop.Code, drop.Body.String())
	}
	if inv, err := s.authStore().GetInvitation(ctx, invID); err == nil {
		t.Errorf("RE-ENTRY TICKET: the grant is gone and %+v stands, addressed to its own creator who holds no org membership row", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the row after the grant was removed: %v", err)
	}
	// The trail names the write that withdrew the premise, not a removal
	// from the org, which did not happen.
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_grant_removed" {
		t.Errorf("the row was taken under cause %v, want creator_team_grant_removed", got)
	}
	// And the consequence, from the token the mint handed out.
	if mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, token); err == nil {
		t.Errorf("the holder walked back into the team on their own invitation: %+v", mb)
	}
}

// orgRowVanishesAfterPreCheckGate answers the boundary read of a grant write
// ONCE — the pre-check's — and ErrNotFound after it: the org membership went
// away between the check and the confirmation, which is the window
// confirmOrgMembershipForGrant exists for and the state its compensating
// revoke acts on (reason grant_unwound_org_member_removed).
type orgRowVanishesAfterPreCheckGate struct {
	identity.Store
	userID, orgID string

	mu    sync.Mutex
	reads int
}

func (g *orgRowVanishesAfterPreCheckGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	g.reads++
	first := g.reads == 1
	g.mu.Unlock()
	if first {
		return identity.OrgMembership{UserID: userID, OrgID: orgID, Role: identity.OrgRoleMember, JoinedAt: time.Now()}, nil
	}
	return identity.OrgMembership{}, identity.ErrNotFound
}

// The same rule at the other site that takes a grant for want of an org
// membership: the compensation that unwinds a grant write whose boundary
// went away. It KNOWS the membership is gone — that is why it revokes — so
// the rows its own confirmation kept on the grant it just took are its to
// sweep.
func TestTeamMembers_theCompensationThatUnwindsAGrantTakesTheRowKeptOnIt(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, _ := mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	real := s.authStore()
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the mint's confirmation did not keep the row on the grant (%v) — then this is not the state the test describes", err)
	}

	s.authSvc.SetStoreForTest(&orgRowVanishesAfterPreCheckGate{Store: real, userID: racerID, orgID: "o1"})
	patch := newErrorRecorder()
	s.handleUpdateMember(patch, teamReq(orgAdminCtx(), "PATCH",
		"/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	s.authSvc.SetStoreForTest(real)

	if patch.Code != http.StatusConflict {
		t.Fatalf("a role change whose boundary went away under it: code=%d body=%s, want 409 — then this is not the compensation the test describes",
			patch.Code, patch.Body.String())
	}
	if mb, err := real.GetMembership(ctx, racerID, "t1"); err == nil {
		t.Fatalf("the compensation did not take the grant (%+v) — then this is not the write the test describes", mb)
	}
	if inv, err := real.GetInvitation(ctx, invID); err == nil {
		t.Errorf("RE-ENTRY TICKET: the compensation unwound the grant and %+v stands, addressed to its own creator who holds no org membership row", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the row after the compensation: %v", err)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_grant_unwound" {
		t.Errorf("the row was taken under cause %v, want creator_team_grant_unwound — the compensation is a refused write taken back, not a removal", got)
	}
}

// The scope of that sweep, on both sides. A member whose org membership row
// ANSWERS the sweep's own read keeps what they minted for themselves: the
// confirmation kept those rows on the ROW, not on this grant, and a
// demotion leaving them a row keeps them too — so a grant removed in one
// team takes nothing. That read is taken once, before the selection, so a
// row written back in between is read as absent and what rests on it goes
// with the sweep (revokeInvitationsKeptOnTeamGrant). And a row addressed to
// SOMEBODY ELSE stands whatever grant its creator holds: it is a capability
// handed over, which no standing of its creator outranks.
func TestTeamMembers_removingAGrantTakesOnlyWhatThatGrantWasThePremiseOf(t *testing.T) {
	t.Run("a member whose org row answers the sweep's read keeps their own invitation", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		ctx := context.Background()
		seedRacerAsTeamAdmin(t, s)
		selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
		invID, _ := mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)

		drop := newErrorRecorder()
		s.handleRemoveMember(drop, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
		if drop.Code != http.StatusNoContent {
			t.Fatalf("removing the grant of a member of the org: code=%d body=%s, want 204", drop.Code, drop.Body.String())
		}
		if om, err := s.authStore().GetOrgMembership(ctx, racerID, "o1"); err != nil {
			t.Fatalf("the member lost their org row too (%v) — then this is not the state the test describes", om)
		}
		if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
			t.Errorf("the invitation of a member whose org membership row answered the sweep's read was swept with their team grant: %v", err)
		}
	})

	t.Run("a row addressed to somebody else stands", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		ctx := context.Background()
		seedOrphanAdminGrant(t, s)
		seedAccount(t, s, "newcomer", "newcomer@example.com")
		selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
		invID, _ := mintInvitationAs(t, s, selfCtx, "t1", "newcomer@example.com", identity.RoleConfigEditor)

		drop := newErrorRecorder()
		s.handleRemoveMember(drop, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
		if drop.Code != http.StatusNoContent {
			t.Fatalf("removing the orphan grant: code=%d body=%s, want 204", drop.Code, drop.Body.String())
		}
		if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
			t.Errorf("the invitation addressed to somebody else was swept with its creator's grant: %v", err)
		}
	})
}

// The sweep of a removed grant selects AFTER the delete, which is what keeps
// a mint out of its own window: a selection made before the delete reaches no
// row minted between the two, and the mint's own confirmation would keep such
// a row on the grant this request is about to revoke. The order is read from
// the mint's answer — issued through the route, right after the kept-on-grant
// sweep's own listing (the write selects once more after it, for the rows its
// creator minted) — the grant being already gone, so a creator has none left
// for a confirmation to keep a row on.
func TestTeamMembers_theSweepOfARemovedGrantSelectsAfterTheDelete(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, _ := mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	real := s.authStore()
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the mint's confirmation did not keep the row on the grant (%v) — then this is not the state the test describes", err)
	}

	windowCode := 0
	windowBody := ""
	windowID := ""
	gate := &afterNthInvitationListingGate{Store: real, teamID: "t1", n: 1}
	// The window is the sweep's own listing, which FOLLOWS the delete of the
	// grant — the premise every assertion below rests on, so the delete is
	// what arms the hook rather than the listing's rank.
	gate.armAfterDeleteOf(racerID, "t1")
	gate.hook = func() {
		// Read from the store the gate wraps, so this read is none of the
		// listings the gate counts.
		requireWindowPremise(ctx, t, real, racerID, "t1")
		mint := newErrorRecorder()
		s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
			`{"email":"racer@example.com","role":"config_editor"}`, "t1", ""))
		windowCode = mint.Code
		windowBody = mint.Body.String()
		var minted struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(mint.Body.Bytes(), &minted)
		windowID = minted.ID
		t.Logf("the mint issued right after the sweep's own listing: code=%d body=%s", mint.Code, mint.Body.String())
	}
	s.authSvc.SetStoreForTest(gate)
	drop := newErrorRecorder()
	s.handleRemoveMember(drop, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
	s.authSvc.SetStoreForTest(real)

	if drop.Code != http.StatusNoContent {
		t.Fatalf("removing the orphan grant: code=%d body=%s, want 204", drop.Code, drop.Body.String())
	}
	// Two listings, both after the delete: the kept-on-grant sweep's own, and
	// the minted half the same write owes (reconcileTeamRankInvitations) —
	// the removal's ordering rule makes the LAST of them the write's last
	// invitation action. The hook fired on the first.
	if listings := gate.listings(); listings != 2 {
		t.Fatalf("the request listed t1's invitations %d times, want 2 — then the mint did not land in the window the test describes", listings)
	}
	if !gate.hookFired() {
		t.Fatalf("the hook never ran: no listing of t1's invitations followed the delete of %s's grant, so this witness measured nothing", racerID)
	}
	if windowCode != http.StatusForbidden {
		t.Errorf("the mint issued after the sweep's selection answered %d, want 403: the grant is deleted before the selection, so there is none left to keep a row on",
			windowCode)
	} else if !strings.Contains(windowBody, "admin or owner required") {
		t.Errorf("the mint was refused 403 for another reason than the grant that is gone: %s", windowBody)
	}
	// And whatever that mint answered, nothing it created stands behind the
	// 204 — the row would be in no listing this request holds.
	if windowID != "" {
		if inv, err := real.GetInvitation(ctx, windowID); err == nil {
			t.Errorf("RE-ENTRY TICKET: %+v was minted after the sweep's selection and stands behind the 204", inv)
		} else if !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("reading the row minted in the window: %v", err)
		}
	}
	if _, err := real.GetInvitation(ctx, invID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the row this sweep was there to take: %v, want it gone", err)
	}
}

// standingReadFailGate fails the org-membership read of ONE account in ONE
// org while armed — the read the sweep of a removed grant makes once that
// grant is already deleted — and answers every other read normally. The read
// it cannot serve is named by the DELETE it follows rather than by an ordinal:
// the rank guard reads the same row before that delete (requireRankOverMember),
// and a gate keyed on a read count would answer the guard instead.
type standingReadFailGate struct {
	identity.Store
	userID, orgID, teamID string

	mu    sync.Mutex
	armed bool

	// removed marks the grant removal this scenario is about, which the
	// sweep's read FOLLOWS. It is held under mu with armed, so one decision
	// reads one snapshot of both.
	removed bool
}

func (g *standingReadFailGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	err := g.Store.DeleteMembership(ctx, userID, teamID)
	if userID == g.userID && teamID == g.teamID {
		g.mu.Lock()
		g.removed = true
		g.mu.Unlock()
	}
	return err
}

func (g *standingReadFailGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	g.mu.Lock()
	fire := g.armed && g.removed && userID == g.userID && orgID == g.orgID
	g.mu.Unlock()
	if fire {
		return identity.OrgMembership{}, errors.New("identity: get org membership: connection reset by peer")
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

func (g *standingReadFailGate) arm(on bool) {
	g.mu.Lock()
	g.armed = on
	g.mu.Unlock()
}

// A premise that could not be READ is not one that found no row, so the
// removal answers 500 rather than 204 and names the row that may stand. The
// listing it names is the TEAM's, as its two sibling exits name: this route
// is admitted on a team grant alone, and that caller — a team admin with no
// org membership row — is answered 403 by the org listing.
func TestTeamMembers_aStandingUnreadableAfterTheGrantWentNamesTheTeamListing(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, _ := mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	real := s.authStore()
	if om, err := real.GetOrgMembership(ctx, "teamadmin", "o1"); err == nil {
		t.Fatalf("the caller holds an org membership row (%+v) — then it is not the team-grant-only caller this exit answers", om)
	}

	gate := &standingReadFailGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1", armed: true}
	s.authSvc.SetStoreForTest(gate)
	drop := newErrorRecorder()
	s.handleRemoveMember(drop, teamReq(teamAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
	gate.arm(false)
	s.authSvc.SetStoreForTest(real)

	if drop.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose sweep could not read the org membership: code=%d body=%s, want 500 — a selection that could not be made is not one that found nothing",
			drop.Code, drop.Diagnostic(t))
	}
	body := drop.Diagnostic(t)
	for _, want := range []string{
		"reading their org membership in o1 failed",
		"connection reset by peer",
		"GET /api/teams/t1/invitations",
		"re-issue the removal to finish it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not carry %q: %s", want, body)
		}
	}
	if strings.Contains(body, "/api/orgs/o1/invitations") {
		t.Errorf("the 500 sends its caller to the ORG listing while the row is in the team: %s", body)
	}
	// The listing it names, read by the caller it answered, carrying the row.
	lw := newErrorRecorder()
	s.handleListInvitations(lw, teamReq(teamAdminCtx(), "GET", "/api/teams/t1/invitations", "", "t1", ""))
	if lw.Code != http.StatusOK || !strings.Contains(lw.Diagnostic(t), invID) {
		t.Errorf("the listing the body names answers that caller %d without the row: %s", lw.Code, lw.Diagnostic(t))
	}
	ow := newErrorRecorder()
	s.handleListOrgInvitations(ow, teamReq(teamAdminCtx(), "GET", "/api/orgs/o1/invitations", "", "o1", ""))
	if ow.Code != http.StatusForbidden {
		t.Errorf("the org listing answers this caller %d — then the two listings are interchangeable here and this witness measures nothing", ow.Code)
	}
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Errorf("the row the 500 names is gone (%v) — a sweep whose premise could not be read is not one that ran", err)
	}
}

// invitationListingFailGate fails the listing of ONE team's invitations while
// armed: the selection a sweep of a removed grant makes, and cannot answer
// 204 without.
type invitationListingFailGate struct {
	identity.Store
	teamID string

	mu    sync.Mutex
	armed bool
}

func (g *invitationListingFailGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	g.mu.Lock()
	fire := g.armed && teamID == g.teamID
	g.mu.Unlock()
	if fire {
		return nil, errors.New("identity: list invitations: no reachable servers")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

func (g *invitationListingFailGate) arm(on bool) {
	g.mu.Lock()
	g.armed = on
	g.mu.Unlock()
}

// The premise held and the sweep itself failed: the grant is gone and the row
// it was the premise of may stand, which is a re-entry ticket — so the
// removal says so instead of answering 204, names the row, and the re-issue
// it asks for finishes it.
func TestTeamMembers_aFailedSweepOfARemovedGrantSaysSoAndFinishesOnTheReissue(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, token := mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	real := s.authStore()

	gate := &invitationListingFailGate{Store: real, teamID: "t1", armed: true}
	s.authSvc.SetStoreForTest(gate)
	drop := newErrorRecorder()
	s.handleRemoveMember(drop, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
	gate.arm(false)

	if drop.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose invitation sweep failed: code=%d body=%s, want 500 — a row kept on the grant just revoked may stand",
			drop.Code, drop.Diagnostic(t))
	}
	body := drop.Diagnostic(t)
	for _, want := range []string{
		"sweeping the pending invitations they addressed to their own address here failed",
		"no reachable servers",
		"accepting it would seat them back in this team",
		"GET /api/teams/t1/invitations",
		"re-issue the removal to finish it",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 does not carry %q: %s", want, body)
		}
	}
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the row the 500 names is already gone (%v) — then this is not the residue the body describes", err)
	}

	// The repair the body names, on a store that answers again.
	s.authSvc.SetStoreForTest(real)
	again := newErrorRecorder()
	s.handleRemoveMember(again, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
	if again.Code != http.StatusNoContent {
		t.Fatalf("the re-issued removal: code=%d body=%s, want 204", again.Code, again.Diagnostic(t))
	}
	if _, err := real.GetInvitation(ctx, invID); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the re-issue did not take the row: %v", err)
	}
	if mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, token); err == nil {
		t.Errorf("the holder walked back into the team on their own invitation: %+v", mb)
	}
}

// grantUnwoundSweepFailGate answers the boundary read of a grant write ONCE —
// the pre-check's — and ErrNotFound after it, and fails the listing the sweep
// of the unwound grant makes: the compensation knows the membership row is
// gone, revokes the grant, and cannot take the row kept on it.
type grantUnwoundSweepFailGate struct {
	identity.Store
	userID, orgID, teamID string

	mu    sync.Mutex
	reads int
}

func (g *grantUnwoundSweepFailGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	g.reads++
	first := g.reads == 1
	g.mu.Unlock()
	if first {
		return identity.OrgMembership{UserID: userID, OrgID: orgID, Role: identity.OrgRoleMember, JoinedAt: time.Now()}, nil
	}
	return identity.OrgMembership{}, identity.ErrNotFound
}

func (g *grantUnwoundSweepFailGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	if teamID != g.teamID {
		return g.Store.ListInvitationsByTeam(ctx, teamID)
	}
	return nil, errors.New("identity: list invitations: no reachable servers")
}

// The same failure at the compensation: the grant write is refused and taken
// back, and the sweep of the rows kept on that grant could not be made. The
// status is the refusal's either way — the caller's write is refused — so the
// row is named in that body, which is what the operator who can revoke it by
// hand reads.
func TestTeamMembers_aCompensationWhoseSweepFailedNamesTheRowItCouldNotTake(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, _ := mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	real := s.authStore()
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the mint's confirmation did not keep the row on the grant (%v) — then this is not the state the test describes", err)
	}

	s.authSvc.SetStoreForTest(&grantUnwoundSweepFailGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1"})
	patch := newErrorRecorder()
	s.handleUpdateMember(patch, teamReq(orgAdminCtx(), "PATCH",
		"/api/teams/t1/members/"+racerID, `{"role":"member"}`, "t1", racerID))
	s.authSvc.SetStoreForTest(real)

	if patch.Code != http.StatusConflict {
		t.Fatalf("a role change whose boundary went away under it: code=%d body=%s, want 409 — then this is not the compensation the test describes",
			patch.Code, patch.Body.String())
	}
	body := patch.Body.String()
	for _, want := range []string{
		"the team grant was revoked",
		"addressed to their own address in this team failed",
		"no reachable servers",
		"stands on the grant just revoked",
		"GET /api/teams/t1/invitations",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not name the row its sweep could not take (missing %q): %s", want, body)
		}
	}
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the row the refusal names is gone (%v) — then this is not the residue the body describes", err)
	}
}

// The sweep exempts a team with no parent org, as the mint's confirmation is:
// there was no org standing for the grant to stand beside, so no row was ever
// kept on it — and the row that stands is one the accept honours, nothing
// about it having been measured against an org rank.
func TestTeamMembers_aGrantRemovedInATeamWithNoParentOrgKeepsTheRow(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedAccount(t, s, racerID, "racer@example.com")
	real := s.authStore()
	if _, err := real.CreateTeam(ctx, identity.Team{ID: "tfree", Name: "tfree", Slug: "tfree", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "tfree", Role: identity.RoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, token := mintInvitationAs(t, s, selfCtx, "tfree", "racer@example.com", identity.RoleConfigEditor)

	drop := newErrorRecorder()
	s.handleRemoveMember(drop, teamReq(selfCtx, "DELETE", "/api/teams/tfree/members/"+racerID, "", "tfree", racerID))
	if drop.Code != http.StatusNoContent {
		t.Fatalf("removing a grant in a team with no parent org: code=%d body=%s, want 204", drop.Code, drop.Body.String())
	}
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Errorf("the row of a team with no parent org was swept with the grant (%v) — no org standing was ever behind it", err)
	}
	if mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, token); err != nil {
		t.Errorf("the row the exemption leaves does not seat them back in the org-less team (%v, %+v) — then the exemption rests on something this witness does not read",
			err, mb)
	}
}

// And a team that definitively no longer exists: the dangling grant a deleted
// team leaves is cleared without a sweep, the row it would take buying
// nothing — no accept resolves it, the accept reading the team first (pkg/auth
// grantOrgMembershipForTeam). A team read that FAILED is the other case, and
// answers 500.
func TestTeamMembers_aDanglingGrantOfADeletedTeamIsClearedWithoutASweep(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedOrphanAdminGrant(t, s)
	real := s.authStore()
	if err := real.UpsertMembership(ctx, identity.Membership{
		UserID: racerID, TeamID: "t2", Role: identity.RoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	invID, token := mintInvitationAs(t, s, selfCtx, "t2", "racer@example.com", identity.RoleConfigEditor)
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the mint's confirmation did not keep the row on the grant (%v) — then this is not the state the test describes", err)
	}
	if err := real.DeleteTeam(ctx, "t2"); err != nil {
		t.Fatal(err)
	}

	drop := newErrorRecorder()
	s.handleRemoveMember(drop, teamReq(selfCtx, "DELETE", "/api/teams/t2/members/"+racerID, "", "t2", racerID))
	if drop.Code != http.StatusNoContent {
		t.Fatalf("clearing the dangling grant of a deleted team: code=%d body=%s, want 204 — a team no accept can resolve is exempt, where a read that failed is not",
			drop.Code, drop.Body.String())
	}
	if _, err := real.GetInvitation(ctx, invID); err != nil {
		t.Errorf("the exempt row was swept anyway: %v", err)
	}
	if mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, token); err == nil {
		t.Errorf("the row of a deleted team seated them back (%+v) — then the exemption's claim does not hold", mb)
	}
}

// lastDeleteAndLastSweepFailGate fails the removal's LAST delete of the
// boundary row and its LAST invitation sweep: the two tail writes whose
// failures leave different residues — a membership row that may stand, and a
// pending invitation.
type lastDeleteAndLastSweepFailGate struct {
	identity.Store
	userID, orgID, teamID string

	mu       sync.Mutex
	deletes  int
	listings int
}

func (g *lastDeleteAndLastSweepFailGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.DeleteOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	g.deletes++
	fail := g.deletes == 2
	g.mu.Unlock()
	if fail {
		return errors.New("identity: delete org membership: connection reset by peer")
	}
	return g.Store.DeleteOrgMembership(ctx, userID, orgID)
}

func (g *lastDeleteAndLastSweepFailGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	if teamID != g.teamID {
		return g.Store.ListInvitationsByTeam(ctx, teamID)
	}
	g.mu.Lock()
	g.listings++
	fail := g.listings == 3
	g.mu.Unlock()
	if fail {
		return nil, errors.New("identity: list invitations: no reachable servers")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

// The tail exit where the LAST delete and the LAST sweep both failed. The
// second pass went through, so the grants ARE revoked and the invitations are
// not: this body carries neither the grant residual — access that is gone —
// nor the promise that the rows this rule takes were taken, which is the one
// claim a failed invitation sweep may never answer with.
func TestOrgOffboarding_aLastDeleteAndALastSweepThatBothFailNameBothResidues(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&lastDeleteAndLastSweepFailGate{Store: real, userID: racerID, orgID: "o1", teamID: "t1"})

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)

	if off.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose last delete and last sweep both failed: code=%d body=%s, want 500", off.Code, off.Diagnostic(t))
	}
	body := off.Diagnostic(t)
	for _, want := range []string{
		"the team grants of racer in org o1 are revoked",
		"re-deleting their org membership failed",
		"connection reset by peer",
		"the last sweep of their pending invitations failed",
		"no reachable servers",
		"re-issue the removal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 loses one of the two residues (missing %q): %s", want, body)
		}
	}
	if strings.Contains(body, revokedMintedInvitations) {
		t.Errorf("the 500 of a failed invitation sweep reports the grants revoked WITH the invitations: %s", body)
	}
}

// afterNthInvitationListingGate runs a hook right AFTER the Nth listing of
// teamID's invitations. The removal makes three for a team of the org it
// sweeps — pass 1, pass 2 and the last sweep — so n=3 puts the hook after
// the last sweep has already READ the rows it will act on: a row written by
// the hook is in no listing this removal holds.
//
// A window whose assertions rest on a grant being GONE arms the hook on that
// grant's own delete (armAfterDeleteOf): the ordinal of a listing is not the
// fact that the delete happened, and a hook that fired before it would
// measure a holder who still has the grant — the state the mint's
// confirmation deliberately keeps a row for. Where no grant is named, the
// hook runs on the Nth listing alone.
type afterNthInvitationListingGate struct {
	identity.Store
	teamID string
	n      int
	// armUserID/armTeamID name the grant whose delete arms the hook. Empty
	// means the hook is armed from the start.
	armUserID string
	armTeamID string

	mu     sync.Mutex
	hits   int
	inHook bool
	fired  bool
	armed  bool
	hook   func()
}

// armAfterDeleteOf makes the hook wait for the delete of userID's grant in
// teamID — the premise the window's own assertions rest on.
func (g *afterNthInvitationListingGate) armAfterDeleteOf(userID, teamID string) {
	g.armUserID, g.armTeamID = userID, teamID
}

func (g *afterNthInvitationListingGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	err := g.Store.DeleteMembership(ctx, userID, teamID)
	if err == nil && userID == g.armUserID && teamID == g.armTeamID {
		g.mu.Lock()
		g.armed = true
		g.mu.Unlock()
	}
	return err
}

func (g *afterNthInvitationListingGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	out, err := g.Store.ListInvitationsByTeam(ctx, teamID)
	if teamID != g.teamID {
		return out, err
	}
	g.mu.Lock()
	if g.inHook {
		g.mu.Unlock()
		return out, err
	}
	g.hits++
	run := g.hits == g.n && g.hook != nil && (g.armUserID == "" || g.armed)
	hook := g.hook
	if run {
		g.hook = nil
		g.inHook = true
		g.fired = true
	}
	g.mu.Unlock()
	if run {
		hook()
		g.mu.Lock()
		g.inHook = false
		g.mu.Unlock()
	}
	return out, err
}

func (g *afterNthInvitationListingGate) listings() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

// hookFired reports whether the window the hook describes happened at all. A
// witness that asserts on what the hook saw asserts this first: a hook that
// never ran leaves its results at their zero values, which assert nothing.
func (g *afterNthInvitationListingGate) hookFired() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}

// grantGoneBeforeTheWindow answers the premise a window keyed on a REVOKED
// grant rests on: userID holds no grant in teamID as the store reads it. The
// error names what it read instead, because a window measured against a grant
// that is THERE measures the orphan-grant holder the mint's confirmation keeps
// a row for, not the member whose grant was just taken — and a read that
// failed is not a grant that is gone.
func grantGoneBeforeTheWindow(ctx context.Context, store identity.Store, userID, teamID string) error {
	mb, err := store.GetMembership(ctx, userID, teamID)
	if errors.Is(err, identity.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s's grant in %s: %w", userID, teamID, err)
	}
	return fmt.Errorf("%s still holds a grant in %s (%+v), want it gone", userID, teamID, mb)
}

// premiseFataler is the part of *testing.T a guard needs: say who to blame,
// then end the run. A recorder satisfies it too, which is what gives the
// refusal itself a witness (testing.TB cannot be implemented outside its
// package).
type premiseFataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

// premiseRecorder stands in for *testing.T where a guard's REFUSAL is under
// test: it records the call instead of ending the run.
type premiseRecorder struct{ fatals []string }

func (r *premiseRecorder) Helper() {}

func (r *premiseRecorder) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

// requireWindowPremise ends a window whose assertions rest on a revoked grant
// when that grant is still there (grantGoneBeforeTheWindow). A window that
// ran on the wrong premise reports a product defect it did not measure, so
// the premise is asserted where the window opens rather than inferred from
// what it saw.
func requireWindowPremise(ctx context.Context, f premiseFataler, store identity.Store, userID, teamID string) {
	f.Helper()
	if err := grantGoneBeforeTheWindow(ctx, store, userID, teamID); err != nil {
		f.Fatalf("the window's premise: %v — a mint measured against a grant that is THERE measures the orphan-grant holder the confirmation keeps a row for, not the member whose grant was just taken", err)
	}
}

// The premise is a guard, so its refusal is what there is to witness: on a
// grant that is still there it ends the window and names it, on one that is
// gone it says nothing.
func TestTeamMembers_theWindowsPremiseRefusesAGrantStillThere(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedOrphanAdminGrant(t, s)
	ctx := context.Background()
	real := s.authStore()

	there := &premiseRecorder{}
	requireWindowPremise(ctx, there, real, racerID, "t1")
	if len(there.fatals) != 1 {
		t.Fatalf("the premise raised %d refusals with %s's grant in t1 still there, want 1 — then a window keyed on that grant being gone runs on the state it excludes: %v",
			len(there.fatals), racerID, there.fatals)
	}
	for _, want := range []string{racerID, "t1"} {
		if !strings.Contains(there.fatals[0], want) {
			t.Errorf("the refusal does not name %q: %s", want, there.fatals[0])
		}
	}

	if err := real.DeleteMembership(ctx, racerID, "t1"); err != nil {
		t.Fatalf("taking %s's grant in t1: %v", racerID, err)
	}
	gone := &premiseRecorder{}
	requireWindowPremise(ctx, gone, real, racerID, "t1")
	if len(gone.fatals) != 0 {
		t.Errorf("the premise refused a window whose grant IS gone: %v", gone.fatals)
	}
}

// The gate the ordering witnesses rest on is scaffolding under test: its
// window is the Nth listing of a team's invitations that FOLLOWS the delete it
// is armed on, and the premise it reports is read from the store. A listing
// made before that delete is not the window — the arming clause, not the
// listing's rank, is what holds the hook — and the same listing fires it when
// no grant arms the gate, which is where the premise reports a grant still
// there.
func TestTeamMembers_theInvitationListingGateHoldsItsHookUntilTheDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("a listing before the delete it is armed on fires nothing", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedOrphanAdminGrant(t, s)
		real := s.authStore()
		fires := 0
		gate := &afterNthInvitationListingGate{Store: real, teamID: "t1", n: 1}
		gate.armAfterDeleteOf(racerID, "t1")
		gate.hook = func() { fires++ }
		if _, err := gate.ListInvitationsByTeam(ctx, "t1"); err != nil {
			t.Fatalf("listing t1's invitations: %v", err)
		}
		if gate.listings() != 1 {
			t.Fatalf("the gate counted %d listings, want 1 — then this listing did not reach it", gate.listings())
		}
		if fires != 0 || gate.hookFired() {
			t.Errorf("the hook ran %d times (fired=%v) on a listing made BEFORE the delete of %s's grant in t1 — then the window is the listing's rank, and a witness whose assertions rest on that grant being gone would measure a holder who still has it",
				fires, gate.hookFired(), racerID)
		}
	})

	t.Run("the same listing fires it unarmed, and the premise names the grant that is there", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedOrphanAdminGrant(t, s)
		real := s.authStore()
		var premiseErr error
		fires := 0
		gate := &afterNthInvitationListingGate{Store: real, teamID: "t1", n: 1}
		gate.hook = func() {
			fires++
			premiseErr = grantGoneBeforeTheWindow(ctx, real, racerID, "t1")
		}
		if _, err := gate.ListInvitationsByTeam(ctx, "t1"); err != nil {
			t.Fatalf("listing t1's invitations: %v", err)
		}
		if fires != 1 || !gate.hookFired() {
			t.Fatalf("the hook ran %d times (fired=%v) on the first listing of a gate no grant arms, want 1 — then the scenario above says nothing about the arming",
				fires, gate.hookFired())
		}
		if premiseErr == nil {
			t.Fatalf("the premise answered OK with %s's grant in t1 still there — then a window keyed on that grant being gone passes on the very state it excludes", racerID)
		}
		for _, want := range []string{racerID, "t1"} {
			if !strings.Contains(premiseErr.Error(), want) {
				t.Errorf("the premise does not name %q: %v", want, premiseErr)
			}
		}
	})

	t.Run("the listing that follows the delete is the window", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedOrphanAdminGrant(t, s)
		real := s.authStore()
		var premiseErr error
		premiseRead, fires := false, 0
		gate := &afterNthInvitationListingGate{Store: real, teamID: "t1", n: 1}
		gate.armAfterDeleteOf(racerID, "t1")
		gate.hook = func() {
			fires++
			premiseErr, premiseRead = grantGoneBeforeTheWindow(ctx, real, racerID, "t1"), true
		}
		if err := gate.DeleteMembership(ctx, racerID, "t1"); err != nil {
			t.Fatalf("taking %s's grant in t1: %v", racerID, err)
		}
		if _, err := gate.ListInvitationsByTeam(ctx, "t1"); err != nil {
			t.Fatalf("listing t1's invitations: %v", err)
		}
		if fires != 1 || !gate.hookFired() {
			t.Fatalf("the hook ran %d times (fired=%v) on the listing that follows the delete, want 1 — then no witness keyed on this gate measures anything",
				fires, gate.hookFired())
		}
		if !premiseRead || premiseErr != nil {
			t.Errorf("the premise (read=%v) answered %v, want nil: the grant is gone by then", premiseRead, premiseErr)
		}
	})
}

// The removal's last selection has no write of the REMOVAL after it, which
// is not the same as no write of the SYSTEM: a third actor can put the grant
// back after that selection's listing — an accept whose grant write
// straddles the whole cascade, a team admin's PATCH, a placement — and a
// mint landing in that window is kept by its own confirmation on the grant
// that came back. The removal answers 204 with the row standing, the state
// being the orphan grant the console reports; what must not happen is the
// operator's natural cleanup of that drift leaving the row behind.
func TestOrgOffboarding_aGrantWrittenBackAfterTheLastSweepIsSweptWhenItGoes(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.auditStore = audit.NewMemoryStore()
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})

	var keptID, token string
	gate := &afterNthInvitationListingGate{Store: real, teamID: "t1", n: 3}
	gate.hook = func() {
		if err := real.UpsertMembership(ctx, identity.Membership{
			UserID: racerID, TeamID: "t1", Role: identity.RoleAdmin, JoinedAt: time.Now(),
		}); err != nil {
			t.Error(err)
			return
		}
		keptID, token = mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	}
	s.authSvc.SetStoreForTest(gate)
	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)

	if off.Code != http.StatusNoContent {
		t.Fatalf("the removal: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	if gate.listings() < 3 || keptID == "" {
		t.Fatalf("the mint did not land after the last sweep's listing (t1 listings=%d, row kept=%q) — then this is not the window the test describes",
			gate.listings(), keptID)
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
		t.Fatalf("the member is still an org member (%+v) — then this is not the state the test describes", om)
	}
	if _, err := real.GetInvitation(ctx, keptID); err != nil {
		t.Fatalf("the row the window left is already gone (%v) — then this is not the state the test describes", err)
	}
	if !orphanGrantFlag(t, s, racerID) {
		t.Error("the grant written back inside the window is not reported as drift — then nothing names the state the cleanup below acts on")
	}
	// The cleanup the drift report asks for, which is also what withdraws
	// the premise the row was kept on.
	drop := newErrorRecorder()
	s.handleRemoveMember(drop, teamReq(orgAdminCtx(), "DELETE", "/api/teams/t1/members/"+racerID, "", "t1", racerID))
	if drop.Code != http.StatusNoContent {
		t.Fatalf("removing the grant written back: code=%d body=%s, want 204", drop.Code, drop.Body.String())
	}
	if inv, err := real.GetInvitation(ctx, keptID); err == nil {
		t.Errorf("RE-ENTRY TICKET: the grant is gone and %+v stands — the row the cleanup was the last premise of", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the row after the cleanup: %v", err)
	}
	if mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, token); err == nil {
		t.Errorf("the off-boarded member accepted their own invitation and holds %+v", mb)
	}
}

// secondPassAndLastSweepFailGate fails the second pass's GRANT listing and
// the last invitation sweep's own listing: the two failures the removal must
// not lose one of, each naming a different residue.
type secondPassAndLastSweepFailGate struct {
	identity.Store
	userID, teamID string

	mu       sync.Mutex
	grants   int
	listings int
}

func (g *secondPassAndLastSweepFailGate) ListMembershipsByUser(ctx context.Context, userID string) ([]identity.Membership, error) {
	if userID != g.userID {
		return g.Store.ListMembershipsByUser(ctx, userID)
	}
	g.mu.Lock()
	g.grants++
	fail := g.grants == 2
	g.mu.Unlock()
	if fail {
		return nil, errors.New("identity: list memberships: no reachable servers")
	}
	return g.Store.ListMembershipsByUser(ctx, userID)
}

func (g *secondPassAndLastSweepFailGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	if teamID != g.teamID {
		return g.Store.ListInvitationsByTeam(ctx, teamID)
	}
	g.mu.Lock()
	g.listings++
	fail := g.listings == 3
	g.mu.Unlock()
	if fail {
		return nil, errors.New("identity: list invitations: connection reset by peer")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

// Two steps of the tail fail independently, and one body carries both
// causes: the second pass, whose residue is a grant, and the last
// invitation sweep, whose residue is a row. A body that kept one cause
// would send the operator looking for the wrong half.
func TestOrgOffboarding_aSecondPassAndALastSweepThatBothFailNameBothCauses(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&secondPassAndLastSweepFailGate{Store: real, userID: racerID, teamID: "t1"})

	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)

	if off.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose second pass and last sweep both failed: code=%d body=%s, want 500", off.Code, off.Diagnostic(t))
	}
	body := off.Diagnostic(t)
	for _, want := range []string{
		"re-sweeping their team access failed",
		"no reachable servers",
		"the last sweep of their pending invitations failed",
		"connection reset by peer",
		"re-issue the removal",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the 500 loses one of the two causes (missing %q): %s", want, body)
		}
	}
}

// lastSweepBudgetGate is a store that has SPENT the budget it was handed:
// it notes the context of the removal's last delete of the membership row,
// seeds at that instant a row only the last invitation sweep can reach, and
// then refuses every read made on that same context. A sweep sharing the
// delete's budget reads nothing; one with a budget of its own takes the row.
type lastSweepBudgetGate struct {
	identity.Store
	userID, orgID string
	seed          func()

	mu      sync.Mutex
	deletes int
	spent   context.Context
}

func (g *lastSweepBudgetGate) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	err := g.Store.DeleteOrgMembership(ctx, userID, orgID)
	if userID != g.userID || orgID != g.orgID {
		return err
	}
	g.mu.Lock()
	g.deletes++
	arm := g.deletes == 2
	seed := g.seed
	if arm {
		g.spent = ctx
		g.seed = nil
	}
	g.mu.Unlock()
	if arm && seed != nil {
		seed()
	}
	return err
}

func (g *lastSweepBudgetGate) refuse(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.spent != nil && ctx == g.spent {
		return errors.New("identity: context deadline exceeded — the budget the last delete was handed is spent")
	}
	return nil
}

func (g *lastSweepBudgetGate) ListTeamsByOrg(ctx context.Context, orgID string) ([]identity.Team, error) {
	if err := g.refuse(ctx); err != nil {
		return nil, err
	}
	return g.Store.ListTeamsByOrg(ctx, orgID)
}

func (g *lastSweepBudgetGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	if err := g.refuse(ctx); err != nil {
		return nil, err
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

// The last sweep's budget is its OWN: the delete before it may have spent
// the one they would otherwise share, and a repair that still has to happen
// is exactly the call a spent budget must not kill.
func TestOrgOffboarding_theLastInvitationSweepRunsOnABudgetOfItsOwn(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()

	const strandedID = "stranded-on-a-spent-budget"
	gate := &lastSweepBudgetGate{Store: real, userID: racerID, orgID: "o1"}
	gate.seed = func() {
		if err := real.CreateInvitation(ctx, identity.Invitation{
			ID: strandedID, TeamID: "t1", Email: "racer@example.com",
			Role: identity.RoleConfigEditor, InvitedBy: "orgadmin",
			CreatedAt: time.Now(), ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		}); err != nil {
			t.Error(err)
		}
	}
	s.authSvc.SetStoreForTest(gate)
	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)

	if off.Code != http.StatusNoContent {
		t.Fatalf("a removal whose last delete spent its own budget: code=%d body=%s, want 204 — the sweep after it runs on a budget of its own",
			off.Code, off.Body.String())
	}
	if inv, err := real.GetInvitation(ctx, strandedID); err == nil {
		t.Errorf("the row the last sweep exists to take stands: %+v", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the row after the removal: %v", err)
	}
}

// The last sweep's POSITION — after the last delete of the membership row,
// not merely after the second pass — has a consequence of its own: a mint in
// the window whose confirmation keeps its row on a RESURRECTED membership
// row. The row and the invitation then stand or fall TOGETHER: with the
// delete last, a membership written inside the window stands and the
// invitation kept on it is coherent with it (the removal says so, and
// re-issuing it is the repair); with the sweep last, the delete cannot take
// the row the invitation was kept on and leave the invitation behind.
func TestOrgOffboarding_aRowKeptOnAResurrectedMembershipStandsOrFallsWithIt(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	real := s.authStore()
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})

	var keptID string
	gate := &afterNthInvitationListingGate{Store: real, teamID: "t1", n: 3}
	gate.hook = func() {
		// The boundary row back, at a rank that authorizes the mint on the
		// org's own rights (orgAdminOfTeam) — the grants are gone.
		if err := real.UpsertOrgMembership(ctx, identity.OrgMembership{
			UserID: racerID, OrgID: "o1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now(),
		}); err != nil {
			t.Error(err)
			return
		}
		keptID, _ = mintInvitationAs(t, s, selfCtx, "t1", "racer@example.com", identity.RoleConfigEditor)
	}
	s.authSvc.SetStoreForTest(gate)
	off := newErrorRecorder()
	s.handleRemoveOrgMember(off, teamReq(orgAdminCtx(), "DELETE", "/api/orgs/o1/members/"+racerID, "", "o1", racerID))
	s.authSvc.SetStoreForTest(real)

	if off.Code != http.StatusNoContent {
		t.Fatalf("the removal: code=%d body=%s, want 204", off.Code, off.Body.String())
	}
	if gate.listings() < 3 || keptID == "" {
		t.Fatalf("the mint did not land in the window (t1 listings=%d, row kept=%q) — then this is not the state the test describes",
			gate.listings(), keptID)
	}
	_, rowErr := real.GetOrgMembership(ctx, racerID, "o1")
	_, invErr := real.GetInvitation(ctx, keptID)
	if (rowErr == nil) != (invErr == nil) {
		t.Errorf("the row the invitation was kept on and the invitation disagree: membership err=%v, invitation err=%v — a membership the removal took after the sweep leaves exactly the re-entry ticket the order exists to prevent",
			rowErr, invErr)
	}
}

// newSSOGrantTestServer builds the server the way production does — New
// registers the sweep the auth service's grant-revoking writes owe on the
// service it is given (wireTeamGrantSweep) — over an auth service whose
// GitHub allow-list grants `role` in team t1 of org o1 to members of
// acme/eng. A witness of that sweep has to come through this wiring: a hook
// set by hand would measure the sweep and not the fact that the service
// carries it.
func newSSOGrantTestServer(t *testing.T, role identity.Role) *Server {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	ssoStore := orgsso.NewMemoryStore()
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
		OrgSSO:     ssoStore,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	s := New(Config{AuthService: svc}, iterlog.New(iterlog.LevelError, nil))
	s.auditStore = audit.NewMemoryStore()
	ctx := context.Background()
	if _, err := s.authStore().CreateOrg(ctx, identity.Org{ID: "o1", Name: "o1", Slug: "o1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{ID: "t1", Name: "t1", Slug: "t1", OrgID: "o1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "orgadmin", OrgID: "o1", Role: identity.OrgRoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ssoStore.Create(ctx, orgsso.OrgSSOProvider{
		ID: "gh1", TenantID: "t1", Kind: orgsso.KindGitHub, Enabled: true, AutoProvision: true,
		Grants:    []orgsso.GitHubTeamGrant{{GitHubOrg: "acme", TeamSlug: "eng", Role: role, Verified: true}},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("github row: %v", err)
	}
	return s
}

// ghLogin is the external identity of a GitHub login, whose groups decide
// which allow-list rows match.
func ghLogin(subject string, groups ...string) oidc.ExternalUser {
	return oidc.ExternalUser{Provider: "github", Subject: subject, Email: subject + "@example.com", Name: subject, Groups: groups}
}

// ssoOrphanGrantHolder logs an account in through the matching allow-list —
// which writes the github_sso grant in t1 and mirrors it up to an org
// membership in o1 — mints the row this class is about, and then takes the
// org membership row away: the orphan grant the console reports
// (orphan_grant), which is the state a self-addressed row is KEPT in by the
// mint's own confirmation (standingAfterMintTakesInvitation). It answers the
// account id, its address and the invitation id.
func ssoOrphanGrantHolder(t *testing.T, s *Server, addressedTo string, keepOrgRow bool) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	res, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("the matching login: %v", err)
	}
	uid := res.User.ID
	mb, err := s.authStore().GetMembership(ctx, uid, "t1")
	if err != nil || mb.Source != identity.MembershipSourceGitHubSSO {
		t.Fatalf("the grant the allow-list wrote: %+v (%v), want one sourced github_sso — then this is not the grant the reconcile revokes", mb, err)
	}
	// The allow-list grants at most member (orgsso RoleForGroups), and
	// minting takes admin: the role an org admin raises the grant to is what
	// makes its holder a minter, and a role change preserves the row's
	// SOURCE (grantMembership, handleUpdateMember) — so the row the reconcile
	// revokes is the one this holder minted on.
	patched := newErrorRecorder()
	s.handleUpdateMember(patched, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/"+uid, `{"role":"admin"}`, "t1", uid))
	if patched.Code != http.StatusOK {
		t.Fatalf("raising the github_sso grant to admin: code=%d body=%s", patched.Code, patched.Body.String())
	}
	if mb, err := s.authStore().GetMembership(ctx, uid, "t1"); err != nil || mb.Source != identity.MembershipSourceGitHubSSO || mb.Role != identity.RoleAdmin {
		t.Fatalf("the grant after the role change: %+v (%v), want an admin row still sourced github_sso — then it is not the row the reconcile revokes", mb, err)
	}
	addr := res.User.Email
	if addressedTo == "" {
		addressedTo = addr
	}
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: uid})
	invID, _ := mintInvitationAs(t, s, selfCtx, "t1", addressedTo, identity.RoleConfigEditor)
	if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the mint's confirmation did not keep the row (%v) — then this is not the state the test describes", err)
	}
	if !keepOrgRow {
		if err := s.authStore().DeleteOrgMembership(ctx, uid, "o1"); err != nil {
			t.Fatal(err)
		}
		if om, err := s.authStore().GetOrgMembership(ctx, uid, "o1"); err == nil {
			t.Fatalf("the holder is still an org member (%+v) — then it is not the orphan state", om)
		}
	}
	return uid, addr, invID
}

// The SSO reconciliation is the third write that withdraws the premise a
// self-addressed invitation is kept on: it revokes the github_sso grants of
// a login that no longer matches the allow-list. Whoever withdraws that
// premise takes what rests on it — through the SAME selection the HTTP
// writes use (takeInvitationsKeptOnRevokedTeamGrant) — or the row is a
// re-entry ticket into the team the grant was just taken from, on the next
// login of an account the allow-list no longer grants.
func TestTeamMembers_anSSORevokedGrantTakesTheRowKeptOnIt(t *testing.T) {
	s := newSSOGrantTestServer(t, identity.RoleAdmin)
	ctx := context.Background()
	uid, _, invID := ssoOrphanGrantHolder(t, s, "", false)

	// The login that no longer matches: the reconcile revokes the grant.
	if _, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("the login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetMembership(ctx, uid, "t1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the github_sso grant stands after a login that no longer matches: %v — then no premise was withdrawn and this witness measures nothing", err)
	}
	if inv, err := s.authStore().GetInvitation(ctx, invID); err == nil {
		t.Errorf("RE-ENTRY TICKET: %+v stands on the github_sso grant the reconcile just revoked", inv)
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the row the sweep was there to take: %v", err)
	}
	// The audit line of the revocation, under the cause that names the write
	// which withdrew the premise, written by the subsystem that acted: this
	// login served no request of its own.
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != auth.MembershipRevokedBySSOReason {
		t.Errorf("the audit line's cause is %v, want %q", got, auth.MembershipRevokedBySSOReason)
	}
	if rows[0].ActorKind != "system" || rows[0].ActorID != authGrantSweepActor {
		t.Errorf("the audit line's actor is %s/%s, want system/%s", rows[0].ActorKind, rows[0].ActorID, authGrantSweepActor)
	}
	if rows[0].TargetID != invID {
		t.Errorf("the audit line names invitation %s, want %s", rows[0].TargetID, invID)
	}
}

// The MINTED half of the SSO reconcile: the revoke is a LOWERING of the
// creator's team authority to no grant at all, so the rows they minted above
// the cap that binds NOW go with it — the member row of a plain org member,
// whose mirror their standing still covers, included. Beside the kept half
// the sweep takes (this holder's org row answers, so the kept sweep takes
// nothing of theirs), the reconcile the server registers
// (SetTeamRankReconcile) runs the minted half through the real login path,
// under the one cause every lowering write shares, attributed to the
// subsystem: this login served no request of its own.
func TestTeamMembers_anSSORevokedGrantTakesTheRowsMintedAboveWhatRemains(t *testing.T) {
	s := newSSOGrantTestServer(t, identity.RoleMember)
	ctx := context.Background()
	res, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("the matching login: %v", err)
	}
	uid := res.User.ID
	if om, err := s.authStore().GetOrgMembership(ctx, uid, "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the org row the grant mirrored up to: %+v (%v), want a plain member — the standing the mint rests on", om, err)
	}
	// The allow-list grants member; the org admin raises the grant to owner
	// (the row's SOURCE is preserved), which is what makes its holder a
	// minter above the role their standing mirrors.
	patched := newErrorRecorder()
	s.handleUpdateMember(patched, teamReq(orgAdminCtx(), "PATCH", "/api/teams/t1/members/"+uid, `{"role":"owner"}`, "t1", uid))
	if patched.Code != http.StatusOK {
		t.Fatalf("raising the github_sso grant to owner: code=%d body=%s", patched.Code, patched.Body.String())
	}
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: uid})
	invID, selfTok := mintInvitationAs(t, s, selfCtx, "t1", res.User.Email, identity.RoleMember)
	if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the mint's confirmation did not keep the row (%v) — then this is not the state the reconcile re-measures", err)
	}

	if _, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("the login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetMembership(ctx, uid, "t1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the github_sso grant stands after a login that no longer matches: %v — then no authority was lowered and this witness measures nothing", err)
	}
	if _, err := s.authStore().GetInvitation(ctx, invID); err == nil {
		t.Error("RE-ENTRY TICKET: the row the holder minted stands on the no-grant state the revoke just left them in")
	} else if !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("reading the row the reconcile was there to take: %v", err)
	}
	// The kept sweep took nothing here — this holder's org row answers — so
	// the one line on the trail is the minted half's, under the lowering's
	// own cause, written by the subsystem.
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_rank_lowered" {
		t.Errorf("the audit line's cause is %v, want creator_team_rank_lowered", got)
	}
	if rows[0].ActorKind != "system" || rows[0].ActorID != authGrantSweepActor {
		t.Errorf("the audit line's actor is %s/%s, want system/%s", rows[0].ActorKind, rows[0].ActorID, authGrantSweepActor)
	}
	// The taken row accepts no more.
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, uid, selfTok); err == nil {
		t.Error("the accept of a taken invitation succeeded")
	}
}

// The counterfactual, on the same write: a creator whose ORG STANDING covers
// the mirror and carries the team keeps the row they minted. The standing
// that answered at the mint answers at the revoke — K.2 measures no grant for
// a creator this high — and the trail holds no line: the revoke took the
// grant and nothing the holder owns beside it.
func TestTeamMembers_anSSORevokedGrantLeavesTheRowsTheCreatorStandingCovers(t *testing.T) {
	s := newSSOGrantTestServer(t, identity.RoleAdmin)
	ctx := context.Background()
	res, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "acme/*", "acme/eng"), "ua", "ip")
	if err != nil {
		t.Fatalf("the matching login: %v", err)
	}
	uid := res.User.ID
	// The allow-list grants member at most (orgsso RoleForGroups), so the
	// standing that carries the team is written here directly: the holder's
	// org row rises to admin, the state the keep side is about — a creator
	// whose standing answers for the team, whatever their grant falls to.
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: uid, OrgID: "o1", Role: identity.OrgRoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if om, err := s.authStore().GetOrgMembership(ctx, uid, "o1"); err != nil || om.Role != identity.OrgRoleAdmin {
		t.Fatalf("the holder's org row: %+v (%v), want an admin — then this is not the standing that carries the team", om, err)
	}
	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: uid})
	invID, _ := mintInvitationAs(t, s, selfCtx, "t1", res.User.Email, identity.RoleMember)
	if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the row the standing's mint kept (%v) — then this is not the state the counterfactual is about", err)
	}

	if _, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("the login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetMembership(ctx, uid, "t1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the github_sso grant stands after a login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
		t.Errorf("the standing that carries the team lost the row its holder minted (%v) — a revoke took what no cap of theirs refuses", err)
	}
	if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 0 {
		t.Errorf("the reconcile recorded %d revocations for a creator whose standing still carries the team: %+v", len(got), got)
	}
}

// The other half of that rule: the rows of a holder whose ORG MEMBERSHIP row
// answers the read before the selection rest on that ROW, which revoking a
// team grant does not touch — the writes that lower the row sweep on their
// own. So the SSO reconciliation leaves it, exactly as the team-member
// DELETE does (sweepInvitationsKeptOnTeamGrant).
func TestTeamMembers_anSSORevokedGrantLeavesTheRowOfAnOrgMember(t *testing.T) {
	s := newSSOGrantTestServer(t, identity.RoleAdmin)
	ctx := context.Background()
	uid, _, invID := ssoOrphanGrantHolder(t, s, "", true)
	if _, err := s.authStore().GetOrgMembership(ctx, uid, "o1"); err != nil {
		t.Fatalf("the holder's org membership row: %v, want it there — then this is not the state this witness measures", err)
	}

	if _, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("the login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetMembership(ctx, uid, "t1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the github_sso grant stands after a login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
		t.Errorf("the row of a holder whose org membership row answered the read is gone (%v) — it rests on that ROW, which this write does not touch", err)
	}
	if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 0 {
		t.Errorf("the reconcile recorded %d invitation revocations for a holder with an org row: %+v", len(got), got)
	}
}

// And a row addressed to SOMEBODY ELSE is a capability handed over, which no
// standing of its creator outranks and no grant of theirs was the premise
// of: the SSO reconciliation leaves it, whatever grant it takes.
func TestTeamMembers_anSSORevokedGrantLeavesARowAddressedToSomebodyElse(t *testing.T) {
	s := newSSOGrantTestServer(t, identity.RoleAdmin)
	ctx := context.Background()
	uid, addr, invID := ssoOrphanGrantHolder(t, s, "someone-else@example.com", false)
	if addr == "someone-else@example.com" {
		t.Fatalf("the fixture addressed the row to the holder's own address (%s) — then it is not the row this witness measures", addr)
	}

	if _, err := s.authSvc.LoginWithExternal(ctx, ghLogin("racer", "other/*"), "ua", "ip"); err != nil {
		t.Fatalf("the login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetMembership(ctx, uid, "t1"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the github_sso grant stands after a login that no longer matches: %v", err)
	}
	if _, err := s.authStore().GetInvitation(ctx, invID); err != nil {
		t.Errorf("the row addressed to another address is gone (%v) — a capability handed over rests on no grant of its creator", err)
	}
}
