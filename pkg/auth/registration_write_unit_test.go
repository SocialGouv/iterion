package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Two properties of the writes a registration runs AFTER its account exists
// (#2655):
//
//   - the landing defaults and the login stamp are cosmetic, and every path
//     that writes them has already committed the account, its space and its
//     grants — so a failure there answers the login and is LOGGED, where it
//     used to answer 500 over a complete account whose retry collides with
//     the email it just took (no path frees it);
//   - every READ of the bootstrap path answers its own failure: only a store
//     that ANSWERED can say an email is free, a membership list is empty or a
//     team is absent.
//
// Each property gets its mirror, because a tolerance that only ever sees the
// failing direction passes just as well on a writer that writes nothing.

// newLoggingGatedService answers a service whose identity store is the
// fail-the-Nth-call gate and whose logger writes into the buffer, so a test
// fails ONE call by name and reads what the service said about it. Both the
// call under test and the assertions run on the test's goroutine.
func newLoggingGatedService(t *testing.T, mode SignupMode) (*Service, *nthCallGate, *bytes.Buffer) {
	t.Helper()
	gate := newNthCallGate(identity.NewMemoryStore(), errStoreUnreachable)
	buf := &bytes.Buffer{}
	svc, err := NewService(Config{
		Store:      gate,
		Sessions:   NewMemorySessionStore(),
		Signer:     newTestSigner(t),
		SignupMode: mode,
		RefreshTTL: 24 * time.Hour,
		Logger:     iterlog.New(iterlog.LevelWarn, buf),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, gate, buf
}

// TestRegisterOpen_aFailedLandingStampStillAnswersTheLogin: the open signup
// wrote the account, its personal org and team and both owner rows, and then
// refused on the cosmetic patch that points the account at them — a 500 whose
// retry answers 409 email_already_taken for as long as the account stands,
// while the password the user just chose signs them in.
func TestRegisterOpen_aFailedLandingStampStillAnswersTheLogin(t *testing.T) {
	ctx := context.Background()
	svc, gate, logs := newLoggingGatedService(t, SignupOpen)
	gate.fail("PatchUser", 0)

	res, err := svc.Register(ctx, "alice@example.com", "correcthorse", "Alice", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v — the account, its space and its owner rows are written: the landing defaults failing is not a reason to answer an error over them", err)
	}
	if res.ActiveTeamID == "" {
		t.Fatalf("the login answered no active team: %+v", res)
	}
	// The session lands on the team the registration wrote, read from the
	// rows and not from the row the patch could not write.
	u, err := gate.raw().GetUserByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("the account the registration answered for is not in the store: %v", err)
	}
	mems, err := gate.raw().ListMembershipsByUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("read the memberships: %v", err)
	}
	if len(mems) != 1 || mems[0].TeamID != res.ActiveTeamID {
		t.Fatalf("memberships = %+v, want the owner grant on the answered team %s", mems, res.ActiveTeamID)
	}
	// Tolerated is not silent: the operator gets the account and the cause.
	logged := logs.String()
	if !strings.Contains(logged, u.ID) || !strings.Contains(logged, "PatchUser") {
		t.Errorf("the unwritten landing defaults were not logged with their cause: %q, want the account id %s and the failed call", logged, u.ID)
	}
	if !strings.Contains(logged, "landing defaults") {
		t.Errorf("the log does not name what could not be written: %q", logged)
	}
	// And the account is whole: signing in lands on the same team, resolved
	// from the membership instead of the default the patch never wrote.
	gate.disarm()
	login, err := svc.Login(ctx, "alice@example.com", "correcthorse", "ua", "ip")
	if err != nil {
		t.Fatalf("the account whose landing defaults failed cannot sign in: %v", err)
	}
	if login.ActiveTeamID != res.ActiveTeamID {
		t.Errorf("the next sign-in landed on %q, want the registration's team %q", login.ActiveTeamID, res.ActiveTeamID)
	}
}

// TestRegisterOpen_theLandingDefaultsAreWrittenWhenTheStoreAnswers is the
// mirror: the tolerance above must not hide a writer that stopped writing.
func TestRegisterOpen_theLandingDefaultsAreWrittenWhenTheStoreAnswers(t *testing.T) {
	ctx := context.Background()
	svc, gate, logs := newLoggingGatedService(t, SignupOpen)

	res, err := svc.Register(ctx, "alice@example.com", "correcthorse", "Alice", "", "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	u, err := gate.raw().GetUserByEmail(ctx, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.DefaultTeamID != res.ActiveTeamID || u.DefaultOrgID == "" {
		t.Fatalf("stored defaults = team %q org %q, want the answered team %q and its org", u.DefaultTeamID, u.DefaultOrgID, res.ActiveTeamID)
	}
	if logged := logs.String(); strings.Contains(logged, "landing defaults") {
		t.Errorf("a registration that wrote its defaults logged a failure: %q", logged)
	}
}

// TestRegisterWithInvitation_aFailedLandingStampStillAnswersTheLogin: the
// invitation path pays the harshest price for refusing — the token is already
// spent (claimInvitation's fail-closed order), so an error over a complete
// account cannot be retried at all, by anyone, without a new invitation.
func TestRegisterWithInvitation_aFailedLandingStampStillAnswersTheLogin(t *testing.T) {
	ctx := context.Background()
	svc, gate, logs := newLoggingGatedService(t, SignupInviteOnly)
	seed := gate.raw()
	if _, err := seed.CreateOrg(ctx, identity.Org{ID: "o1", Name: "Org One", Slug: "org-one", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateTeam(ctx, identity.Team{ID: "tb", OrgID: "o1", Name: "Team B", Slug: "team-b"}); err != nil {
		t.Fatal(err)
	}
	token, inv, err := svc.CreateInvitation(mintVerdictCtx(ctx), "tb", "invitee@example.org", identity.RoleMember, "inviter")
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	gate.fail("PatchUser", 0)

	res, err := svc.Register(ctx, "invitee@example.org", "correcthorse", "Zoe", token, "ua", "ip")
	if err != nil {
		t.Fatalf("Register: %v — the invitation is claimed and the grants are written: answering an error here spends the token for nothing", err)
	}
	if res.ActiveTeamID != "tb" {
		t.Fatalf("active team = %q, want the invited team tb", res.ActiveTeamID)
	}
	u, err := seed.GetUserByEmail(ctx, "invitee@example.org")
	if err != nil {
		t.Fatalf("the invited account is not in the store: %v", err)
	}
	if _, err := seed.GetMembership(ctx, u.ID, "tb"); err != nil {
		t.Fatalf("the invited grant is not in the store: %v", err)
	}
	// The claim stands: the price of the fail-closed order is NOT paid back
	// by re-opening the row (claimInvitation says why).
	stored, err := seed.GetInvitation(ctx, inv.ID)
	if err != nil {
		t.Fatalf("read the invitation: %v", err)
	}
	if stored.AcceptedAt == nil {
		t.Errorf("the invitation was re-opened by the tolerant stamp: %+v", stored)
	}
	if logged := logs.String(); !strings.Contains(logged, u.ID) || !strings.Contains(logged, "landing defaults") {
		t.Errorf("the unwritten landing defaults of the invited account were not logged: %q", logged)
	}
}

// TestCreateUserAndPersonalTeam_answersTheTeamItWroteWhateverTheStoreReads:
// the bootstrap used to refuse on the same cosmetic patch, and to answer a
// ZERO team when the read-back of the team it had just written failed. The
// team it answers is the row the provisioning wrote, so there is no read
// after the first irreversible write left to fail.
func TestCreateUserAndPersonalTeam_answersTheTeamItWroteWhateverTheStoreReads(t *testing.T) {
	ctx := context.Background()
	svc, gate, logs := newLoggingGatedService(t, SignupOpen)
	gate.fail("PatchUser", 0).fail("GetTeam", 0)

	u, team, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive)
	if err != nil {
		t.Fatalf("CreateUserAndPersonalTeam: %v", err)
	}
	if u.ID == "" {
		t.Fatalf("the bootstrap answered no account: %+v", u)
	}
	if team.ID == "" || team.Slug == "" || !team.Personal {
		t.Fatalf("team = %+v, want the personal team the provisioning wrote", team)
	}
	stored, err := gate.raw().GetTeam(ctx, team.ID)
	if err != nil {
		t.Fatalf("the team it answered is not in the store: %v", err)
	}
	if stored.Slug != team.Slug || stored.OrgID != team.OrgID {
		t.Errorf("answered %+v, stored %+v: the answer is not the row that was written", team, stored)
	}
	if logged := logs.String(); !strings.Contains(logged, u.ID) || !strings.Contains(logged, "landing defaults") {
		t.Errorf("the unwritten landing defaults of the bootstrapped account were not logged: %q", logged)
	}
}

// TestCreateUserAndPersonalTeam_aFailedReadNeverAnswersAFact: the idempotent
// branch read three rows and dropped every error — a store blip answered
// "this email is free" (and provisioned a second account over the row it
// could not see), "this account has no teams", or "this membership is not the
// personal team".
func TestCreateUserAndPersonalTeam_aFailedReadNeverAnswersAFact(t *testing.T) {
	ctx := context.Background()

	t.Run("the email read", func(t *testing.T) {
		svc, gate, _ := newLoggingGatedService(t, SignupOpen)
		if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive); err != nil {
			t.Fatalf("seed the account: %v", err)
		}
		gate.fail("GetUserByEmail", 0)

		_, _, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive)
		if !errors.Is(err, errStoreUnreachable) {
			t.Fatalf("err = %v, want the failure of the email read", err)
		}
		if !strings.Contains(err.Error(), "ops@example.com") {
			t.Errorf("the failure does not name what could not be read: %v", err)
		}
		// The damage the fall-through did: a second provisioning over the
		// account the read could not see.
		gate.disarm()
		if _, gerr := gate.raw().GetOrgBySlug(ctx, identity.SlugifyTeamName("ops@example.com")+"-2"); !errors.Is(gerr, identity.ErrNotFound) {
			t.Fatalf("a failed email read provisioned a second personal space")
		}
	})

	t.Run("the membership list of an account already there", func(t *testing.T) {
		svc, gate, _ := newLoggingGatedService(t, SignupOpen)
		if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive); err != nil {
			t.Fatalf("seed the account: %v", err)
		}
		gate.fail("ListMembershipsByUser", 0)

		_, team, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive)
		if !errors.Is(err, errStoreUnreachable) {
			t.Fatalf("err = %v team = %+v, want the failure of the membership read, not an account with no personal team", err, team)
		}
	})

	t.Run("the team of a membership", func(t *testing.T) {
		svc, gate, _ := newLoggingGatedService(t, SignupOpen)
		if _, _, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive); err != nil {
			t.Fatalf("seed the account: %v", err)
		}
		gate.fail("GetTeam", 0)

		_, team, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive)
		if !errors.Is(err, errStoreUnreachable) {
			t.Fatalf("err = %v team = %+v, want the failure of the team read, not 'not the personal team'", err, team)
		}
		if !strings.Contains(err.Error(), "GetTeam") {
			t.Errorf("the failure does not carry the read's own cause: %v", err)
		}
	})
}

// TestCreateUserAndPersonalTeam_anAnsweredReadStillAnswersItsFact is the
// mirror of all three: a store that ANSWERS keeps the idempotent behaviour —
// including the one absence that is a fact, a membership whose team is gone.
func TestCreateUserAndPersonalTeam_anAnsweredReadStillAnswersItsFact(t *testing.T) {
	ctx := context.Background()

	t.Run("an email nobody holds is provisioned", func(t *testing.T) {
		svc, _, _ := newLoggingGatedService(t, SignupOpen)
		u, team, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive)
		if err != nil || u.ID == "" || !team.Personal {
			t.Fatalf("u = %+v team = %+v err = %v, want a provisioned account and its personal team", u, team, err)
		}
	})

	t.Run("the personal team of an account already there", func(t *testing.T) {
		svc, _, _ := newLoggingGatedService(t, SignupOpen)
		first, team, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive)
		if err != nil {
			t.Fatal(err)
		}
		again, sameTeam, err := svc.CreateUserAndPersonalTeam(ctx, "ops@example.com", "Ops", "correcthorse", true, identity.UserStatusActive)
		if err != nil {
			t.Fatalf("the idempotent call failed: %v", err)
		}
		if again.ID != first.ID || sameTeam.ID != team.ID {
			t.Fatalf("second call answered account %q team %q, want %q / %q", again.ID, sameTeam.ID, first.ID, team.ID)
		}
	})

	t.Run("a membership whose team the store says is gone", func(t *testing.T) {
		svc, gate, _ := newLoggingGatedService(t, SignupOpen)
		seed := gate.raw()
		u, err := seed.CreateUser(ctx, identity.User{ID: "u-dangling", Email: "ghost@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.UpsertMembership(ctx, identity.Membership{UserID: u.ID, TeamID: "t-gone", Role: identity.RoleOwner, JoinedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}

		existing, team, err := svc.CreateUserAndPersonalTeam(ctx, "ghost@example.com", "Ghost", "correcthorse", false, identity.UserStatusActive)
		if err != nil {
			t.Fatalf("a membership whose team is absent answered an error: %v — the store ANSWERED that the row is gone", err)
		}
		if existing.ID != u.ID {
			t.Fatalf("answered account %q, want the existing %q", existing.ID, u.ID)
		}
		if team.ID != "" {
			t.Fatalf("team = %+v, want the zero team: the account holds no personal team", team)
		}
	})
}
