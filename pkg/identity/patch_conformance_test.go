package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runPatchSuite exercises the PatchUser/PatchTeam/PatchOrg contract both
// Store implementations must satisfy. It matters more than it looks: the two
// twins reach the same result by opposite means — the memory store
// mutates a copied struct, Mongo builds a `$set` of hand-written keys
// — so a field name that does not match its bson tag would write a NEW
// document key and update nothing, silently and without an error. Only
// a suite that runs against BOTH can see that.
func runPatchSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	seedOrg := func(id, slug string) {
		t.Helper()
		if _, err := s.CreateOrg(ctx, Org{ID: id, Name: id, Slug: slug, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed org %s: %v", id, err)
		}
	}
	seedTeam := func(id, orgID, slug string) {
		t.Helper()
		if _, err := s.CreateTeam(ctx, Team{ID: id, OrgID: orgID, Name: id, Slug: slug, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed team %s: %v", id, err)
		}
	}
	seedUser := func(id, email string, u User) User {
		t.Helper()
		u.ID, u.Email, u.CreatedAt, u.UpdatedAt = id, email, now, now
		if u.Status == "" {
			u.Status = UserStatusActive
		}
		got, err := s.CreateUser(ctx, u)
		if err != nil {
			t.Fatalf("seed user %s: %v", id, err)
		}
		return got
	}

	seedOrg("po1", "patch-org-one")
	seedOrg("po2", "patch-org-two")
	seedTeam("pt1", "po1", "patch-team-one")
	seedTeam("pt2", "po1", "patch-team-two")

	t.Run("user: every patchable field round-trips under its bson tag", func(t *testing.T) {
		seedUser("pu1", "pu1@example.org", User{Name: "Before", PasswordHash: "hash-before"})
		// One patch per field would pass even if two shared a key; naming
		// them all at once and reading every one back is what catches a
		// `$set` key that does not exist on the document.
		name, hash, team, org := "After", "hash-after", "pt1", "po1"
		orgs := []string{"socialgouv", "betagouv"}
		super := true
		pending := UserStatusPendingPasswordChange
		until := now.Add(time.Hour)
		got, err := s.PatchUser(ctx, "pu1", UserPatch{
			Name: &name, PasswordHash: &hash, Status: &pending, IsSuperAdmin: &super,
			DefaultTeamID: &team, DefaultOrgID: &org, GitHubOrgs: &orgs,
			LastLoginAt: &now,
			Lockout:     &UserLockout{FailedLogins: 3, LockedUntil: &until},
		})
		if err != nil {
			t.Fatalf("PatchUser: %v", err)
		}
		switch {
		case got.Name != "After":
			t.Errorf("name not applied: %+v", got)
		case got.PasswordHash != "hash-after":
			t.Errorf("password_hash not applied: %+v", got)
		case got.Status != UserStatusPendingPasswordChange:
			t.Errorf("status not applied: %q", got.Status)
		case !got.IsSuperAdmin:
			t.Errorf("is_super_admin not applied: %+v", got)
		case got.DefaultTeamID != "pt1" || got.DefaultOrgID != "po1":
			t.Errorf("defaults not applied: %+v", got)
		case len(got.GitHubOrgs) != 2 || got.GitHubOrgs[0] != "socialgouv":
			t.Errorf("github_orgs not applied: %+v", got.GitHubOrgs)
		case got.LastLoginAt == nil || !got.LastLoginAt.Equal(now):
			t.Errorf("last_login_at not applied: %v", got.LastLoginAt)
		case got.FailedLogins != 3:
			t.Errorf("failed_logins not applied: %d", got.FailedLogins)
		case got.LockedUntil == nil || !got.LockedUntil.Equal(until):
			t.Errorf("locked_until not applied: %v", got.LockedUntil)
		}
		// A patch that names one field leaves every other standing — the
		// whole point, and the hazard a whole-row write is.
		back := UserStatusActive
		got, err = s.PatchUser(ctx, "pu1", UserPatch{Status: &back})
		if err != nil {
			t.Fatalf("PatchUser(status): %v", err)
		}
		if got.PasswordHash != "hash-after" || got.Name != "After" || !got.IsSuperAdmin {
			t.Errorf("a status patch clobbered an unnamed field: %+v", got)
		}
		// Clearing the lockout writes an explicit null through $set, which
		// must decode back to the zero value — the half a memory-only test
		// cannot see.
		got, err = s.PatchUser(ctx, "pu1", UserPatch{Lockout: &UserLockout{}})
		if err != nil {
			t.Fatalf("PatchUser(clear lockout): %v", err)
		}
		if got.FailedLogins != 0 || got.LockedUntil != nil {
			t.Errorf("stale lockout survived the clear: failed=%d until=%v", got.FailedLogins, got.LockedUntil)
		}
		// The store keeps a snapshot, not the caller's slice: Mongo marshals
		// one, and the memory twin must copy — else a caller editing the
		// slice it passed edits the store from outside.
		mine := []string{"socialgouv"}
		if _, err := s.PatchUser(ctx, "pu1", UserPatch{GitHubOrgs: &mine}); err != nil {
			t.Fatalf("PatchUser(github_orgs): %v", err)
		}
		mine[0] = "mutated-after-the-write"
		if reread, err := s.GetUser(ctx, "pu1"); err != nil {
			t.Fatalf("GetUser: %v", err)
		} else if len(reread.GitHubOrgs) != 1 || reread.GitHubOrgs[0] != "socialgouv" {
			t.Errorf("the store aliased the caller's slice: %+v", reread.GitHubOrgs)
		}
		// An empty list is a meaningful write (the user left every org),
		// distinct from "leave untouched".
		empty := []string{}
		got, err = s.PatchUser(ctx, "pu1", UserPatch{GitHubOrgs: &empty})
		if err != nil {
			t.Fatalf("PatchUser(clear github_orgs): %v", err)
		}
		if len(got.GitHubOrgs) != 0 {
			t.Errorf("explicit clear left the org list standing: %+v", got.GitHubOrgs)
		}
	})

	t.Run("user: the precondition refuses a disabled row and writes nothing", func(t *testing.T) {
		before := seedUser("pu2", "pu2@example.org", User{PasswordHash: "hash-keep", Status: UserStatusDisabled})
		hash, active := "hash-new", UserStatusActive
		if _, err := s.PatchUser(ctx, "pu2", UserPatch{
			PasswordHash: &hash, Status: &active,
			IfNotStatus: []UserStatus{UserStatusDisabled},
		}); !errors.Is(err, ErrUserStatusChanged) {
			t.Fatalf("want ErrUserStatusChanged, got %v", err)
		}
		after, err := s.GetUser(ctx, "pu2")
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if after.Status != UserStatusDisabled || after.PasswordHash != before.PasswordHash {
			t.Fatalf("the refused patch still landed: %+v", after)
		}
		// The same patch without the precondition is the explicit
		// reactivation path, which must stay available.
		got, err := s.PatchUser(ctx, "pu2", UserPatch{PasswordHash: &hash, Status: &active})
		if err != nil {
			t.Fatalf("explicit reactivation: %v", err)
		}
		if got.Status != UserStatusActive || got.PasswordHash != "hash-new" {
			t.Fatalf("reactivation did not land: %+v", got)
		}
		// And a precondition listing SEVERAL statuses refuses on any of
		// them — the list is read, not just its first entry.
		pending := UserStatusPendingPasswordChange
		if _, err := s.PatchUser(ctx, "pu2", UserPatch{
			Status:      &pending,
			IfNotStatus: []UserStatus{UserStatusDisabled, UserStatusActive},
		}); !errors.Is(err, ErrUserStatusChanged) {
			t.Fatalf("want ErrUserStatusChanged on the second listed status, got %v", err)
		}
	})

	t.Run("user: the password-hash precondition refuses a replaced hash and writes nothing", func(t *testing.T) {
		seedUser("pu4", "pu4@example.org", User{PasswordHash: "hash-current", Status: UserStatusActive})
		stale, now := "hash-verified-earlier", time.Now().UTC().Truncate(time.Millisecond)
		if _, err := s.PatchUser(ctx, "pu4", UserPatch{
			LastLoginAt: &now, IfPasswordHash: &stale,
		}); !errors.Is(err, ErrUserPasswordChanged) {
			t.Fatalf("want ErrUserPasswordChanged, got %v", err)
		}
		after, err := s.GetUser(ctx, "pu4")
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if after.LastLoginAt != nil {
			t.Fatalf("the refused patch still landed: %+v", after)
		}
		// The empty patch answers the precondition too.
		if _, err := s.PatchUser(ctx, "pu4", UserPatch{IfPasswordHash: &stale}); !errors.Is(err, ErrUserPasswordChanged) {
			t.Fatalf("an empty patch reported success on a replaced hash: %v", err)
		}
		// The hash the row holds admits the write.
		current := "hash-current"
		got, err := s.PatchUser(ctx, "pu4", UserPatch{LastLoginAt: &now, IfPasswordHash: &current})
		if err != nil {
			t.Fatalf("PatchUser under the current hash: %v", err)
		}
		if got.LastLoginAt == nil || !got.LastLoginAt.Equal(now) {
			t.Fatalf("the admitted patch did not land: %+v", got)
		}
		// The status arm still answers first when both refuse.
		disabled := UserStatusDisabled
		if _, err := s.PatchUser(ctx, "pu4", UserPatch{Status: &disabled}); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if _, err := s.PatchUser(ctx, "pu4", UserPatch{
			LastLoginAt: &now, IfPasswordHash: &stale, IfNotStatus: []UserStatus{UserStatusDisabled},
		}); !errors.Is(err, ErrUserStatusChanged) {
			t.Fatalf("want ErrUserStatusChanged when both arms refuse, got %v", err)
		}
	})

	t.Run("user: a refusal is not a missing account", func(t *testing.T) {
		seedUser("pu3", "pu3@example.org", User{Status: UserStatusDisabled})
		name := "ghost"
		// Unknown id → ErrNotFound, even under a precondition: a caller must
		// not read "no such user" as "an admin disabled it under me".
		if _, err := s.PatchUser(ctx, "nobody", UserPatch{
			Name: &name, IfNotStatus: []UserStatus{UserStatusDisabled},
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound for an unknown id, got %v", err)
		}
		// An empty patch reads the row back — but still answers the
		// precondition rather than reporting success on a refused status.
		got, err := s.PatchUser(ctx, "pu3", UserPatch{})
		if err != nil {
			t.Fatalf("empty PatchUser: %v", err)
		}
		if got.ID != "pu3" {
			t.Fatalf("empty patch did not read back the user: %+v", got)
		}
		if _, err := s.PatchUser(ctx, "pu3", UserPatch{
			IfNotStatus: []UserStatus{UserStatusDisabled},
		}); !errors.Is(err, ErrUserStatusChanged) {
			t.Fatalf("an empty patch reported success on a refused status: %v", err)
		}
	})

	t.Run("team: a named field moves and the others do not", func(t *testing.T) {
		name := "Renamed"
		got, err := s.PatchTeam(ctx, "pt1", TeamPatch{Name: &name})
		if err != nil {
			t.Fatalf("PatchTeam: %v", err)
		}
		if got.Name != "Renamed" {
			t.Fatalf("name not applied: %+v", got)
		}
		// The whole point of a patch: an untouched field survives.
		if got.Slug != "patch-team-one" || got.OrgID != "po1" {
			t.Fatalf("patch clobbered an unnamed field: %+v", got)
		}
	})

	t.Run("team: suspending stamps the trio, restoring clears it", func(t *testing.T) {
		susp := TeamStatusSuspended
		got, err := s.PatchTeam(ctx, "pt1", TeamPatch{
			Status: &susp, SuspendedBy: "admin-1", SuspendReason: "unpaid",
		})
		if err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if got.Status != TeamStatusSuspended {
			t.Fatalf("status not applied: %+v", got)
		}
		if got.SuspendedAt == nil || got.SuspendedBy != "admin-1" || got.SuspendReason != "unpaid" {
			t.Fatalf("suspension trio not stamped: at=%v by=%q reason=%q",
				got.SuspendedAt, got.SuspendedBy, got.SuspendReason)
		}
		// The subtle half, and the one a memory-only test cannot see:
		// Mongo clears these by writing an explicit null / "" through
		// $set, which must decode back to the zero value.
		active := TeamStatusActive
		got, err = s.PatchTeam(ctx, "pt1", TeamPatch{Status: &active})
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		if got.Status != TeamStatusActive {
			t.Fatalf("status not restored: %+v", got)
		}
		if got.SuspendedAt != nil || got.SuspendedBy != "" || got.SuspendReason != "" {
			t.Fatalf("stale suspension survived the restore: at=%v by=%q reason=%q",
				got.SuspendedAt, got.SuspendedBy, got.SuspendReason)
		}
	})

	t.Run("team: the runner pool moves alone and survives an unrelated patch", func(t *testing.T) {
		pool := "honorabilite"
		got, err := s.PatchTeam(ctx, "pt1", TeamPatch{RunnerPool: &pool})
		if err != nil {
			t.Fatalf("map pool: %v", err)
		}
		if got.RunnerPool != "honorabilite" {
			t.Fatalf("pool not applied: %+v", got)
		}
		// The F2 core: a whole-document replace from a stale read would
		// erase the field. An unrelated patch must leave it standing.
		name := "Renamed Again"
		got, err = s.PatchTeam(ctx, "pt1", TeamPatch{Name: &name})
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		if got.RunnerPool != "honorabilite" {
			t.Fatalf("an unrelated patch erased the pool mapping: %+v", got)
		}
		// "" is a meaningful write (unmap), distinct from "leave untouched".
		empty := ""
		got, err = s.PatchTeam(ctx, "pt1", TeamPatch{RunnerPool: &empty})
		if err != nil {
			t.Fatalf("unmap: %v", err)
		}
		if got.RunnerPool != "" {
			t.Fatalf("explicit clear did not unmap: %+v", got)
		}
	})

	t.Run("team: a taken slug is refused, not silently applied", func(t *testing.T) {
		taken := "patch-team-two"
		if _, err := s.PatchTeam(ctx, "pt1", TeamPatch{Slug: &taken}); !errors.Is(err, ErrSlugAlreadyTaken) {
			t.Fatalf("want ErrSlugAlreadyTaken, got %v", err)
		}
		got, err := s.GetTeam(ctx, "pt1")
		if err != nil {
			t.Fatalf("GetTeam: %v", err)
		}
		if got.Slug != "patch-team-one" {
			t.Fatalf("refused patch still moved the slug: %q", got.Slug)
		}
	})

	t.Run("team: unknown id is ErrNotFound", func(t *testing.T) {
		name := "ghost"
		if _, err := s.PatchTeam(ctx, "nope", TeamPatch{Name: &name}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("team: an empty patch reads back rather than erroring", func(t *testing.T) {
		got, err := s.PatchTeam(ctx, "pt2", TeamPatch{})
		if err != nil {
			t.Fatalf("empty PatchTeam: %v", err)
		}
		if got.ID != "pt2" || got.Slug != "patch-team-two" {
			t.Fatalf("empty patch did not read back the team: %+v", got)
		}
	})

	t.Run("org: the governance fields round-trip", func(t *testing.T) {
		req := true
		scope := ProvisionApprovalSharedCredentials
		// A nested struct through $set: the driver marshals it, and
		// nothing else in the suite would notice if it came back empty.
		aud := CredentialAudience{Teams: []string{"pt1", "pt2"}}
		got, err := s.PatchOrg(ctx, "po1", OrgPatch{
			RequireProvisionApproval: &req,
			ProvisionApprovalScope:   &scope,
			CredentialAudience:       &aud,
		})
		if err != nil {
			t.Fatalf("PatchOrg: %v", err)
		}
		if !got.RequireProvisionApproval {
			t.Fatalf("require_provision_approval not applied: %+v", got)
		}
		if got.ProvisionApprovalScope != ProvisionApprovalSharedCredentials {
			t.Fatalf("scope not applied: %q", got.ProvisionApprovalScope)
		}
		if len(got.CredentialAudience.Teams) != 2 ||
			!got.CredentialAudience.Allows("pt1") || !got.CredentialAudience.Allows("pt2") {
			t.Fatalf("audience did not round-trip: %+v", got.CredentialAudience)
		}
		// An audience that admits nobody must persist as such — the zero
		// value is load-bearing (lending is an explicit act), so it must
		// not be indistinguishable from "never written".
		empty := CredentialAudience{}
		got, err = s.PatchOrg(ctx, "po1", OrgPatch{CredentialAudience: &empty})
		if err != nil {
			t.Fatalf("PatchOrg(empty audience): %v", err)
		}
		if got.CredentialAudience.Allows("pt1") {
			t.Fatalf("cleared audience still admits a team: %+v", got.CredentialAudience)
		}
		// And the neighbouring field it did not name stayed put.
		if !got.RequireProvisionApproval {
			t.Fatalf("audience patch clobbered require_provision_approval: %+v", got)
		}
	})

	t.Run("org: a taken slug is refused", func(t *testing.T) {
		taken := "patch-org-two"
		if _, err := s.PatchOrg(ctx, "po1", OrgPatch{Slug: &taken}); !errors.Is(err, ErrOrgSlugAlreadyTaken) {
			t.Fatalf("want ErrOrgSlugAlreadyTaken, got %v", err)
		}
	})

	t.Run("org: unknown id is ErrNotFound", func(t *testing.T) {
		name := "ghost"
		if _, err := s.PatchOrg(ctx, "nope", OrgPatch{Name: &name}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})
}

func TestMemoryStore_Patch(t *testing.T) {
	runPatchSuite(t, NewMemoryStore())
}

// TestMongoStore_Patch runs the shared patch suite against a real Mongo,
// gated on ITERION_TEST_MONGO_URI like the sibling bulk-get suite. The
// mongo-conformance CI job already runs ./pkg/identity/..., so this is
// the half that makes the Mongo $set paths executable at all.
func TestMongoStore_Patch(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo identity patch suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_identity_patch_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	st := NewMongoStore(db)
	if err := st.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	runPatchSuite(t, st)
}
