package auth

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// TestEraseUser walks an erasure collection by collection on the memory
// twins: the refusal that protects an organization from losing its last
// owner, then what the erasure leaves — a pseudonymous row under its marker
// and nothing else — and that erasing twice is once.
//
// The mutation this reddens: drop the DeleteAccountSessions call from
// Service.EraseUser — the "sessions are deleted, not marked" assertion fails.
func TestEraseUser(t *testing.T) {
	ctx := context.Background()
	st := identity.NewMemoryStore()
	sessions := NewMemorySessionStore()
	svc := newTestServiceOn(t, st, sessions)
	now := time.Now().UTC()

	alice, err := svc.Register(ctx, "alice@example.org", "correcthorse", "Alice", "", "ua-one", "10.0.0.1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	second, err := svc.Login(ctx, "alice@example.org", "correcthorse", "ua-two", "10.0.0.2")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	bob, err := svc.Register(ctx, "bob@example.org", "correcthorse", "Bob", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register bob: %v", err)
	}
	aliceID, bobID := alice.User.ID, bob.User.ID

	// A shared org alice owns alone, with a team she is granted in; an
	// invitation addressed to her there; an SSO link; GitHub orgs on the row.
	if _, err := st.CreateOrg(ctx, identity.Org{ID: "solo", Name: "Solo", Slug: "solo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: aliceID, OrgID: "solo", Role: identity.OrgRoleOwner, JoinedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTeam(ctx, identity.Team{ID: "solo-core", OrgID: "solo", Name: "Core", Slug: "solo-core", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMembership(ctx, identity.Membership{UserID: aliceID, TeamID: "solo-core", Role: identity.RoleAdmin, JoinedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInvitation(ctx, identity.Invitation{ID: "inv-alice", TeamID: "solo-core", Email: "Alice@example.org", Role: identity.RoleMember, TokenHash: "h-alice", InvitedBy: bobID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInvitation(ctx, identity.Invitation{ID: "inv-bob", TeamID: "solo-core", Email: "bob@example.org", Role: identity.RoleMember, TokenHash: "h-bob", InvitedBy: aliceID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertOIDCLink(ctx, identity.OIDCLink{Provider: "github", ProviderUserID: "777", UserID: aliceID, Email: "alice@example.org", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	ghOrgs := []string{"acme"}
	if _, err := st.PatchUser(ctx, aliceID, identity.UserPatch{GitHubOrgs: &ghOrgs}); err != nil {
		t.Fatal(err)
	}
	personalOrgID := alice.ActiveOrgID
	if personalOrgID == "" {
		t.Fatal("the registration opened no personal org")
	}

	sessionOf := func(t *testing.T, refresh string) (Session, error) {
		t.Helper()
		return sessions.GetSessionByTokenHash(ctx, HashRefreshToken(refresh))
	}

	t.Run("the last owner of a shared org is refused before any write", func(t *testing.T) {
		hooked := false
		_, err := svc.EraseUser(ctx, aliceID, EraseOptions{PersonalOrgGrace: time.Hour, BeforeAnonymize: func(context.Context, identity.User) error {
			hooked = true
			return nil
		}})
		if hooked {
			t.Fatal("a refused erasure ran the other stores' half")
		}
		if !errors.Is(err, ErrLastOrgOwner) || !strings.Contains(err.Error(), "solo") {
			t.Fatalf("EraseUser = %v, want ErrLastOrgOwner naming solo", err)
		}
		u, err := st.GetUser(ctx, aliceID)
		if err != nil || u.Status != identity.UserStatusActive || u.Name != "Alice" {
			t.Fatalf("a refused erasure touched the row: %+v (%v)", u, err)
		}
		if _, err := sessionOf(t, alice.RefreshToken); err != nil {
			t.Fatalf("a refused erasure touched the sessions: %v", err)
		}
		if mems, _ := st.ListMembershipsByUser(ctx, aliceID); len(mems) != 2 {
			t.Fatalf("a refused erasure touched the memberships: %d left, want 2", len(mems))
		}
		if o, _ := st.GetOrg(ctx, personalOrgID); o.Status == identity.TeamStatusPendingDeletion {
			t.Fatal("a refused erasure marked the personal org for deletion")
		}
	})

	if err := st.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: bobID, OrgID: "solo", Role: identity.OrgRoleOwner, JoinedAt: now}); err != nil {
		t.Fatal(err)
	}

	var res EraseResult
	t.Run("the erasure leaves a pseudonymous row and nothing that names or lets in the person", func(t *testing.T) {
		var err error
		res, err = svc.EraseUser(ctx, aliceID, EraseOptions{PersonalOrgGrace: time.Hour})
		if err != nil {
			t.Fatalf("EraseUser: %v", err)
		}
		svc.WaitRevocationPasses()
		u := res.User
		if u.ID != aliceID || u.Status != identity.UserStatusErased || u.Email != identity.ErasedEmail(aliceID) {
			t.Fatalf("erased row = %+v", u)
		}
		if u.Name != "" || u.PasswordHash != "" || len(u.GitHubOrgs) != 0 || u.LastLoginAt != nil || u.DefaultTeamID != "" || u.DefaultOrgID != "" {
			t.Fatalf("the row still names or lets in the person: %+v", u)
		}
		if _, err := st.GetUserByEmail(ctx, "alice@example.org"); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("the address still resolves: %v", err)
		}
		if links, _ := st.ListOIDCLinksByUser(ctx, aliceID); len(links) != 0 || res.SSOLinksRemoved != 1 {
			t.Fatalf("oidc_links: %d left, %d removed", len(links), res.SSOLinksRemoved)
		}
		if mems, _ := st.ListMembershipsByUser(ctx, aliceID); len(mems) != 0 || res.MembershipsRemoved != 2 {
			t.Fatalf("memberships: %d left, %d removed (want 0, 2: the personal team and solo-core)", len(mems), res.MembershipsRemoved)
		}
		if oms, _ := st.ListOrgMembershipsByUser(ctx, aliceID); len(oms) != 0 || res.OrgMembershipsRemoved != 2 {
			t.Fatalf("org_memberships: %d left, %d removed (want 0, 2)", len(oms), res.OrgMembershipsRemoved)
		}
		if _, err := st.GetInvitation(ctx, "inv-alice"); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("the invitation addressed to the account survived: %v", err)
		}
		// Accepting an invitation reads nothing of its creator: the one the
		// account created would still seat its invitee, so it goes too.
		if _, err := st.GetInvitation(ctx, "inv-bob"); !errors.Is(err, identity.ErrNotFound) || res.InvitationsRemoved != 2 {
			t.Fatalf("the invitation the account created survived (%v, %d removed, want 2)", err, res.InvitationsRemoved)
		}
		if len(res.PersonalOrgsMarkedGone) != 1 || res.PersonalOrgsMarkedGone[0] != personalOrgID {
			t.Fatalf("personal orgs marked for deletion = %v, want [%s]", res.PersonalOrgsMarkedGone, personalOrgID)
		}
		if o, err := st.GetOrg(ctx, personalOrgID); err != nil || o.Status != identity.TeamStatusPendingDeletion || o.PurgeAfter == nil {
			t.Fatalf("the personal org is not pending deletion: %+v (%v)", o, err)
		}
		if o, err := st.GetOrg(ctx, "solo"); err != nil || o.Status == identity.TeamStatusPendingDeletion {
			t.Fatalf("the shared org went with the account: %+v (%v)", o, err)
		}
		if om, err := st.GetOrgMembership(ctx, bobID, "solo"); err != nil || om.Role != identity.OrgRoleOwner {
			t.Fatalf("the other owner lost solo: %+v (%v)", om, err)
		}
		// Sessions are DELETED, not marked: a row carries the IP and user
		// agent of its sign-in.
		if res.SessionsDeleted != 2 {
			t.Fatalf("sessions deleted = %d, want 2", res.SessionsDeleted)
		}
		for _, refresh := range []string{alice.RefreshToken, second.RefreshToken} {
			if sess, err := sessionOf(t, refresh); !errors.Is(err, ErrSessionNotFound) {
				t.Fatalf("a session of the erased account is still stored (%+v, %v): deleted, not revoked, expected", sess, err)
			}
		}
		if _, err := sessionOf(t, bob.RefreshToken); err != nil {
			t.Fatalf("another account's session went with the erasure: %v", err)
		}
	})

	t.Run("nothing signs the erased account in", func(t *testing.T) {
		if _, err := svc.Login(ctx, "alice@example.org", "correcthorse", "ua", "ip"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("login by the old address = %v, want invalid credentials (the address resolves to nothing)", err)
		}
		if _, err := svc.Login(ctx, identity.ErasedEmail(aliceID), "correcthorse", "ua", "ip"); !errors.Is(err, ErrAccountErased) {
			t.Fatalf("login by the marker = %v, want ErrAccountErased", err)
		}
		if _, err := svc.Refresh(ctx, second.RefreshToken, "ua", "ip"); err == nil {
			t.Fatal("a refresh token of the erased account still refreshes")
		}
		if _, err := svc.CheckStanding(ctx, Identity{UserID: aliceID, Credential: CredentialAccessToken}); !errors.Is(err, ErrAccountErased) || !errors.Is(err, ErrAccountDisabled) {
			t.Fatalf("the standing of an access token of the erased account = %v, want ErrAccountErased (which is ErrAccountDisabled)", err)
		}
	})

	t.Run("erasing twice is once", func(t *testing.T) {
		before, err := st.GetUser(ctx, aliceID)
		if err != nil {
			t.Fatal(err)
		}
		again, err := svc.EraseUser(ctx, aliceID, EraseOptions{PersonalOrgGrace: time.Hour})
		if err != nil {
			t.Fatalf("second EraseUser: %v", err)
		}
		if !again.AlreadyErased {
			t.Fatal("the second erasure does not report the row was erased already")
		}
		if !reflect.DeepEqual(again.User, before) {
			t.Fatalf("the second erasure changed the row: %+v, was %+v", again.User, before)
		}
		if again.SessionsDeleted != 0 || again.SSOLinksRemoved != 0 || again.MembershipsRemoved != 0 || again.OrgMembershipsRemoved != 0 || again.InvitationsRemoved != 0 || len(again.PersonalOrgsMarkedGone) != 0 {
			t.Fatalf("the second erasure removed something: %+v", again)
		}
	})

	t.Run("an unknown account is ErrNotFound", func(t *testing.T) {
		if _, err := svc.EraseUser(ctx, "nobody", EraseOptions{PersonalOrgGrace: time.Hour}); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("EraseUser(nobody) = %v", err)
		}
	})
}

// TestEraseUser_TheHookRunsWithTheAddressAndItsFailureLeavesTheRowForTheRetry
// pins the order the server's half relies on: BeforeAnonymize is not run on
// a refusal, receives the row while it still carries the person's address,
// and its failure aborts before the anonymization — so the repeated request
// finds the address again (what scrubs the invitation rows naming it). The
// row a failed hook leaves is erased already, not active: nothing signs it
// in while the erasure waits for its repetition.
//
// The mutation this reddens: move MarkUserErased after the hook (the order
// where the status lands with the anonymization) — the password sign-in
// after the failed hook succeeds.
func TestEraseUser_TheHookRunsWithTheAddressAndItsFailureLeavesTheRowForTheRetry(t *testing.T) {
	ctx := context.Background()
	st := identity.NewMemoryStore()
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	carol, err := svc.Register(ctx, "carol@example.org", "correcthorse", "Carol", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	id := carol.User.ID

	var seen []string
	failing := EraseOptions{PersonalOrgGrace: time.Hour, BeforeAnonymize: func(_ context.Context, u identity.User) error {
		seen = append(seen, u.Email)
		return errors.New("push store down")
	}}
	res, err := svc.EraseUser(ctx, id, failing)
	if err == nil || !strings.Contains(err.Error(), "push store down") {
		t.Fatalf("EraseUser with a failing hook = %v, want its error", err)
	}
	u, err := st.GetUser(ctx, id)
	if err != nil || u.Email != "carol@example.org" {
		t.Fatalf("a failed hook let the row be anonymized: %+v (%v)", u, err)
	}
	if u.Status != identity.UserStatusErased {
		t.Fatalf("a failed hook left the row %s, want erased: an account emptied of its memberships must not sign in meanwhile", u.Status)
	}
	if _, err := svc.Login(ctx, "carol@example.org", "correcthorse", "ua", "ip"); !errors.Is(err, ErrAccountErased) {
		t.Fatalf("a password sign-in after the failed hook = %v, want ErrAccountErased", err)
	}
	if _, err := svc.Refresh(ctx, carol.RefreshToken, "ua", "ip"); err == nil {
		t.Fatal("a refresh after the failed hook still refreshes")
	}
	if !res.Marked {
		t.Fatal("the failed erasure does not report the row it marked erased")
	}

	res, err = svc.EraseUser(ctx, id, EraseOptions{PersonalOrgGrace: time.Hour, BeforeAnonymize: func(_ context.Context, u identity.User) error {
		seen = append(seen, u.Email)
		return nil
	}})
	if err != nil {
		t.Fatalf("the repeated erasure: %v", err)
	}
	if res.AlreadyErased {
		t.Fatal("the repeated erasure of a row still carrying its address reports it erased already")
	}
	if res.User.Status != identity.UserStatusErased || res.User.Email != identity.ErasedEmail(id) {
		t.Fatalf("the repeated erasure left %+v", res.User)
	}
	if !reflect.DeepEqual(seen, []string{"carol@example.org", "carol@example.org"}) {
		t.Fatalf("the hook saw %v, want the address on both calls", seen)
	}
}

// TestEraseUser_NoSignInOpensDuringTheErasureAndWhatOneWroteIsSwept drives
// sign-ins from inside the hook — the window between the first sweep and
// the anonymization. A password and an SSO sign-in (the provider trusted for
// auto-link by address) are refused there; the grants a sign-in that read
// the account before the mark would still write — an SSO link carrying the
// address, a membership, a session row with its IP — are written there too,
// and are gone once the erasure returns.
//
// The mutations this reddens: move MarkUserErased after the hook — the SSO
// sign-in links the identity; drop the second sweepAccountGrants — the
// link and the membership written in the window survive.
func TestEraseUser_NoSignInOpensDuringTheErasureAndWhatOneWroteIsSwept(t *testing.T) {
	ctx := context.Background()
	st := identity.NewMemoryStore()
	sessions := NewMemorySessionStore()
	svc := newTestServiceOn(t, st, sessions)
	svc.trustedAutoLinkProviders = map[string]struct{}{"oidc-test": {}}
	dave, err := svc.Register(ctx, "dave@example.org", "correcthorse", "Dave", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	id := dave.User.ID
	now := time.Now().UTC()
	if _, err := st.CreateOrg(ctx, identity.Org{ID: "shared", Name: "Shared", Slug: "shared", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTeam(ctx, identity.Team{ID: "shared-core", OrgID: "shared", Name: "Core", Slug: "shared-core", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	hook := func(hctx context.Context, u identity.User) error {
		if _, err := svc.Login(hctx, "dave@example.org", "correcthorse", "ua", "ip"); !errors.Is(err, ErrAccountErased) {
			t.Errorf("a password sign-in during the erasure = %v, want ErrAccountErased", err)
		}
		ext := oidc.ExternalUser{Provider: "oidc-test", Subject: "sub-live", Email: "dave@example.org"}
		if res, err := svc.LoginWithExternal(hctx, ext, "ua", "ip"); !errors.Is(err, ErrAccountErased) {
			t.Errorf("an SSO sign-in during the erasure = %v (a session: %t), want ErrAccountErased", err, res.RefreshToken != "")
		}
		// What a sign-in that read the account before the mark writes after
		// the first sweep.
		if err := st.UpsertOIDCLink(hctx, identity.OIDCLink{Provider: "oidc-test", ProviderUserID: "sub-late", UserID: u.ID, Email: u.Email, CreatedAt: now}); err != nil {
			return err
		}
		if err := st.UpsertMembership(hctx, identity.Membership{UserID: u.ID, TeamID: "shared-core", Role: identity.RoleMember, JoinedAt: now}); err != nil {
			return err
		}
		if err := st.UpsertOrgMembership(hctx, identity.OrgMembership{UserID: u.ID, OrgID: "shared", Role: identity.OrgRoleMember, JoinedAt: now}); err != nil {
			return err
		}
		return sessions.CreateSession(hctx, Session{ID: "late-session", UserID: u.ID, TokenHash: "late-hash", IP: "10.9.9.9", UserAgent: "late-ua", IssuedAt: now, ExpiresAt: now.Add(time.Hour)})
	}
	res, err := svc.EraseUser(ctx, id, EraseOptions{PersonalOrgGrace: time.Hour, BeforeAnonymize: hook})
	if err != nil {
		t.Fatalf("EraseUser: %v", err)
	}
	svc.WaitRevocationPasses()
	if links, _ := st.ListOIDCLinksByUser(ctx, id); len(links) != 0 {
		t.Fatalf("SSO links left after the erasure: %+v", links)
	}
	if mems, _ := st.ListMembershipsByUser(ctx, id); len(mems) != 0 {
		t.Fatalf("team memberships left after the erasure: %+v", mems)
	}
	if oms, _ := st.ListOrgMembershipsByUser(ctx, id); len(oms) != 0 {
		t.Fatalf("org memberships left after the erasure: %+v", oms)
	}
	if _, err := sessions.GetSessionByTokenHash(ctx, "late-hash"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("the session written during the erasure is still stored: %v", err)
	}
	if res.SSOLinksRemoved != 1 || res.SessionsDeleted != 2 {
		t.Fatalf("counts: %d link(s), %d session(s) removed, want 1 and 2 (the registration's and the late one)", res.SSOLinksRemoved, res.SessionsDeleted)
	}
}

// raceStore is the memory identity store with a hook on DeleteOrgMembership:
// it runs once, before the delete of the membership it names, the write a
// concurrent request lands in the window between a refusal's read and the
// erasure's own delete.
type raceStore struct {
	*identity.MemoryStore
	userID, orgID string
	before        func()
}

func (r *raceStore) DeleteOrgMembership(ctx context.Context, userID, orgID string) error {
	if userID == r.userID && orgID == r.orgID && r.before != nil {
		before := r.before
		r.before = nil
		before()
	}
	return r.MemoryStore.DeleteOrgMembership(ctx, userID, orgID)
}

// TestEraseUser_TheLastOwnerRefusalHoldsWhenTheOtherOwnerGoesMeanwhile: two
// co-owners of a shared org, the other one's ownership released between
// this erasure's refusal read and its own delete — a concurrent erasure of
// the other owner, or a demotion. The erasure is refused, and the org keeps
// an owner.
//
// The mutation this reddens: drop the recount in releaseOwnerships — the
// erasure succeeds and the org is left with no owner.
func TestEraseUser_TheLastOwnerRefusalHoldsWhenTheOtherOwnerGoesMeanwhile(t *testing.T) {
	ctx := context.Background()
	mem := identity.NewMemoryStore()
	st := &raceStore{MemoryStore: mem, orgID: "duo"}
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	erin, err := svc.Register(ctx, "erin@example.org", "correcthorse", "Erin", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	now := time.Now().UTC()
	if _, err := mem.CreateOrg(ctx, identity.Org{ID: "duo", Name: "Duo", Slug: "duo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{erin.User.ID, "frank"} {
		if err := mem.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: uid, OrgID: "duo", Role: identity.OrgRoleOwner, JoinedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	st.userID = erin.User.ID
	st.before = func() {
		if err := mem.DeleteOrgMembership(ctx, "frank", "duo"); err != nil {
			t.Errorf("the racing release: %v", err)
		}
	}

	hooked := false
	_, err = svc.EraseUser(ctx, erin.User.ID, EraseOptions{PersonalOrgGrace: time.Hour, BeforeAnonymize: func(context.Context, identity.User) error {
		hooked = true
		return nil
	}})
	if !errors.Is(err, ErrLastOrgOwner) || !strings.Contains(err.Error(), "duo") {
		t.Fatalf("EraseUser with the other owner gone meanwhile = %v, want ErrLastOrgOwner naming duo", err)
	}
	if hooked {
		t.Fatal("the refused erasure ran the other stores' half")
	}
	om, err := mem.GetOrgMembership(ctx, erin.User.ID, "duo")
	if err != nil || om.Role != identity.OrgRoleOwner {
		t.Fatalf("the org is left without its owner: %+v (%v)", om, err)
	}
	if u, err := mem.GetUser(ctx, erin.User.ID); err != nil || u.Status != identity.UserStatusActive {
		t.Fatalf("the refused erasure touched the row: %+v (%v)", u, err)
	}
}

// cancelingStore is raceStore whose writes answer the context's error, the
// way the Mongo store does once a request is canceled.
type cancelingStore struct {
	*raceStore
}

func (c *cancelingStore) UpsertOrgMembership(ctx context.Context, m identity.OrgMembership) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.raceStore.UpsertOrgMembership(ctx, m)
}

// TestEraseUser_ACanceledRequestStillPutsTheOwnershipBack: the request goes
// away between the release of an ownership and the refusal that puts it
// back. The compensation runs detached from it, so the org keeps its owner.
//
// The mutation this reddens: restoreOwnerships on the request's context.
func TestEraseUser_ACanceledRequestStillPutsTheOwnershipBack(t *testing.T) {
	mem := identity.NewMemoryStore()
	race := &raceStore{MemoryStore: mem, orgID: "duo"}
	st := &cancelingStore{raceStore: race}
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gina, err := svc.Register(ctx, "gina@example.org", "correcthorse", "Gina", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	now := time.Now().UTC()
	if _, err := mem.CreateOrg(ctx, identity.Org{ID: "duo", Name: "Duo", Slug: "duo", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{gina.User.ID, "hugo"} {
		if err := mem.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: uid, OrgID: "duo", Role: identity.OrgRoleOwner, JoinedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	race.userID = gina.User.ID
	race.before = func() {
		if err := mem.DeleteOrgMembership(context.Background(), "hugo", "duo"); err != nil {
			t.Errorf("the racing release: %v", err)
		}
		cancel() // the client goes away mid-erasure
	}

	_, err = svc.EraseUser(ctx, gina.User.ID, EraseOptions{PersonalOrgGrace: time.Hour})
	if !errors.Is(err, ErrLastOrgOwner) {
		t.Fatalf("EraseUser = %v, want ErrLastOrgOwner", err)
	}
	om, err := mem.GetOrgMembership(context.Background(), gina.User.ID, "duo")
	if err != nil || om.Role != identity.OrgRoleOwner {
		t.Fatalf("the canceled request left the org without its owner: %+v (%v)", om, err)
	}
}

// TestEraseUser_TheInvitationsTheAccountCreatedGo: accepting an invitation
// reads nothing of its creator, so the pending invitations an erased account
// created would still seat their invitees; the erasure removes them, and
// counts them.
func TestEraseUser_TheInvitationsTheAccountCreatedGo(t *testing.T) {
	ctx := context.Background()
	st := identity.NewMemoryStore()
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	ivy, err := svc.Register(ctx, "ivy@example.org", "correcthorse", "Ivy", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	now := time.Now().UTC()
	for _, inv := range []identity.Invitation{
		{ID: "by-ivy", TeamID: "ta", Email: "guest@example.org", Role: identity.RoleMember, TokenHash: "th-ivy", InvitedBy: ivy.User.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "by-other", TeamID: "ta", Email: "guest2@example.org", Role: identity.RoleMember, TokenHash: "th-other", InvitedBy: "someone-else", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	} {
		if err := st.CreateInvitation(ctx, inv); err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.EraseUser(ctx, ivy.User.ID, EraseOptions{PersonalOrgGrace: time.Hour})
	if err != nil {
		t.Fatalf("EraseUser: %v", err)
	}
	if _, err := st.GetInvitation(ctx, "by-ivy"); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("the invitation the erased account created survived: %v", err)
	}
	if _, err := st.GetInvitation(ctx, "by-other"); err != nil {
		t.Fatalf("another account's invitation went too: %v", err)
	}
	if res.InvitationsRemoved < 1 {
		t.Fatalf("InvitationsRemoved = %d, want the created invitation counted", res.InvitationsRemoved)
	}
}

// TestEraseUser_NoInvitationReachesAnAddressUnderErasure: once the erasure
// marked the account, an invitation to its address would outlive the sweep
// by address — and a retry, once the row is anonymized, no longer knows the
// address. The invitation is refused while the row still carries it.
func TestEraseUser_NoInvitationReachesAnAddressUnderErasure(t *testing.T) {
	ctx := context.Background()
	st := identity.NewMemoryStore()
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	jade, err := svc.Register(ctx, "jade@example.org", "correcthorse", "Jade", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	inviteCtx := mintVerdictCtx(ctx)
	_, err = svc.EraseUser(ctx, jade.User.ID, EraseOptions{PersonalOrgGrace: time.Hour, BeforeAnonymize: func(context.Context, identity.User) error {
		// The row is marked erased and still carries the address here.
		if _, _, ierr := svc.CreateInvitation(inviteCtx, jade.ActiveTeamID, "jade@example.org", identity.RoleMember, "someone-else"); !errors.Is(ierr, ErrGrantRefused) {
			t.Errorf("an invitation to the address under erasure = %v, want ErrGrantRefused", ierr)
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("EraseUser: %v", err)
	}
	invs, err := st.ListInvitationsByTeam(ctx, jade.ActiveTeamID)
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range invs {
		if inv.AddressedTo("jade@example.org") {
			t.Fatalf("an invitation to the erased address survived: %+v", inv)
		}
	}
}

// grantBeforeMarkStore is the memory identity store on which before runs
// once, ahead of the erased status landing on userID: what a concurrent
// request writes after the erasure read the memberships and before its mark.
type grantBeforeMarkStore struct {
	*identity.MemoryStore
	userID string
	before func()
}

func (g *grantBeforeMarkStore) MarkUserErased(ctx context.Context, id string) (identity.User, error) {
	if id == g.userID && g.before != nil {
		before := g.before
		g.before = nil
		before()
	}
	return g.MemoryStore.MarkUserErased(ctx, id)
}

// TestEraseUser_AnOwnershipGrantedDuringTheErasureIsRecounted: the account
// creates an organization in the window between the erasure's read of its
// memberships and its mark — the creation read the account active on both
// sides of its write. The sweep finds the ownership step 1 never saw and
// recounts: no other owner, so the ownership is put back and the erasure
// refused before the anonymization, naming the org. The org keeps its owner.
//
// The mutations this reddens: the sweep deleting the ownership without the
// recount — the org is left with no owner; restoreOwnerships without the
// compensation mark — the grant guard refuses to put back an ownership of an
// account already marked erased.
func TestEraseUser_AnOwnershipGrantedDuringTheErasureIsRecounted(t *testing.T) {
	ctx := context.Background()
	mem := identity.NewMemoryStore()
	st := &grantBeforeMarkStore{MemoryStore: mem}
	svc := newTestServiceOn(t, st, NewMemorySessionStore())
	kim, err := svc.Register(ctx, "kim@example.org", "correcthorse", "Kim", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	now := time.Now().UTC()
	st.userID = kim.User.ID
	st.before = func() {
		if _, err := mem.CreateOrg(ctx, identity.Org{ID: "late", Name: "Late", Slug: "late", CreatedBy: kim.User.ID, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Errorf("the racing org: %v", err)
		}
		if err := mem.UpsertOrgMembership(ctx, identity.OrgMembership{UserID: kim.User.ID, OrgID: "late", Role: identity.OrgRoleOwner, JoinedAt: now}); err != nil {
			t.Errorf("the racing ownership: %v", err)
		}
	}

	hooked := false
	_, err = svc.EraseUser(ctx, kim.User.ID, EraseOptions{PersonalOrgGrace: time.Hour, BeforeAnonymize: func(context.Context, identity.User) error {
		hooked = true
		return nil
	}})
	if !errors.Is(err, ErrLastOrgOwner) || !strings.Contains(err.Error(), "late") {
		t.Fatalf("EraseUser with an ownership granted during it = %v, want ErrLastOrgOwner naming late", err)
	}
	if hooked {
		t.Fatal("the refused erasure ran the other stores' half")
	}
	om, err := mem.GetOrgMembership(ctx, kim.User.ID, "late")
	if err != nil || om.Role != identity.OrgRoleOwner {
		t.Fatalf("the org granted during the erasure is left without its owner: %+v (%v)", om, err)
	}
	u, err := mem.GetUser(ctx, kim.User.ID)
	if err != nil || u.Status != identity.UserStatusErased || identity.IsErasedEmail(u.Email) {
		t.Fatalf("the row = %+v (%v), want it marked erased with its address kept for the repeat", u, err)
	}
}
