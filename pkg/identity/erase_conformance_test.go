package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// runEraseSuite pins the identity half of an account erasure on both twins:
// EraseUser anonymizes the row under its marker and nothing else, twice is
// once, and DeleteInvitationsByEmail reads an address the way AddressedTo
// does. The two twins reach the result by opposite means (a copied struct
// against a `$set`/`$unset` document), so only a suite that runs against
// both can see a key that writes nothing.
func runEraseSuite(t *testing.T, s Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	locked := now.Add(time.Hour)
	seed := User{
		ID: "erase-me", Email: "Erase.Me@Example.org", Name: "Erase Me", PasswordHash: "hash",
		Status: UserStatusActive, IsSuperAdmin: true, CreatedAt: now, UpdatedAt: now,
		LastLoginAt: &now, FailedLogins: 3, LockedUntil: &locked,
		DefaultTeamID: "t1", DefaultOrgID: "o1", GitHubOrgs: []string{"acme"},
	}
	if _, err := s.CreateUser(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("the row keeps its id and creation instant, and nothing that names the person", func(t *testing.T) {
		got, err := s.EraseUser(ctx, "erase-me")
		if err != nil {
			t.Fatalf("EraseUser: %v", err)
		}
		if got.UpdatedAt.Before(now) {
			t.Fatalf("updated_at %v predates the erasure", got.UpdatedAt)
		}
		want := erasedUser(seed, ErasedEmail("erase-me"), got.UpdatedAt)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("erased row = %+v, want %+v", got, want)
		}
		stored, err := s.GetUser(ctx, "erase-me")
		if err != nil {
			t.Fatalf("GetUser after: %v", err)
		}
		if !reflect.DeepEqual(stored, want) {
			t.Fatalf("stored row = %+v, want %+v", stored, want)
		}
		if _, err := s.GetUserByEmail(ctx, "erase.me@example.org"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the old address still resolves: %v", err)
		}
		if byMarker, err := s.GetUserByEmail(ctx, ErasedEmail("erase-me")); err != nil || byMarker.ID != "erase-me" {
			t.Fatalf("the marker resolves to %+v (%v), want the row", byMarker, err)
		}
	})

	t.Run("erasing twice is once", func(t *testing.T) {
		first, err := s.GetUser(ctx, "erase-me")
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.EraseUser(ctx, "erase-me")
		if err != nil {
			t.Fatalf("second EraseUser: %v", err)
		}
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("a second erasure changed the row: %+v, was %+v", again, first)
		}
	})

	t.Run("no patch reaches an erased row, preconditions or not", func(t *testing.T) {
		before, err := s.GetUser(ctx, "erase-me")
		if err != nil {
			t.Fatal(err)
		}
		active := UserStatusActive
		name := "Revived"
		for label, p := range map[string]UserPatch{
			"a bare re-enable":        {Status: &active},
			"a rename":                {Name: &name},
			"an empty patch":          {},
			"a guarded sign-in stamp": {LastLoginAt: &now, IfNotStatus: []UserStatus{UserStatusDisabled}},
		} {
			if _, err := s.PatchUser(ctx, "erase-me", p); !errors.Is(err, ErrUserStatusChanged) {
				t.Fatalf("%s on an erased row = %v, want ErrUserStatusChanged", label, err)
			}
		}
		after, err := s.GetUser(ctx, "erase-me")
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("a refused patch changed the erased row: %+v (%v), was %+v", after, err, before)
		}
	})

	t.Run("no patch writes the erased status", func(t *testing.T) {
		if _, err := s.CreateUser(ctx, User{ID: "bystander", Email: "bystander@example.org", Status: UserStatusActive, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		erased := UserStatusErased
		if _, err := s.PatchUser(ctx, "bystander", UserPatch{Status: &erased}); !errors.Is(err, ErrErasedStatusReserved) {
			t.Fatalf("a patch writing the erased status = %v, want ErrErasedStatusReserved", err)
		}
		if u, err := s.GetUser(ctx, "bystander"); err != nil || u.Status != UserStatusActive || u.Email != "bystander@example.org" {
			t.Fatalf("the refused patch changed the row: %+v (%v)", u, err)
		}
	})

	t.Run("a missing row is ErrNotFound", func(t *testing.T) {
		if _, err := s.EraseUser(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("EraseUser(nobody) = %v, want ErrNotFound", err)
		}
	})

	t.Run("the marker domain is reserved at creation", func(t *testing.T) {
		if _, err := s.CreateUser(ctx, User{ID: "squatter", Email: "erased-victim@" + ErasedEmailDomain, Status: UserStatusActive, CreatedAt: now}); !errors.Is(err, ErrReservedEmail) {
			t.Fatalf("CreateUser under the marker domain = %v, want ErrReservedEmail", err)
		}
		if _, err := s.CreateUser(ctx, User{ID: "squatter", Email: ErasedEmail("erase-me"), Status: UserStatusActive, CreatedAt: now}); !errors.Is(err, ErrReservedEmail) {
			t.Fatalf("CreateUser with a marker = %v, want ErrReservedEmail", err)
		}
	})

	t.Run("the invitations addressed to the account go, in every team, however the address was spelled", func(t *testing.T) {
		mk := func(id, team, email, hash string) {
			t.Helper()
			if err := s.CreateInvitation(ctx, Invitation{ID: id, TeamID: team, Email: email, Role: RoleMember, TokenHash: hash, InvitedBy: "x", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				t.Fatalf("seed invitation %s: %v", id, err)
			}
		}
		mk("i1", "ta", "Invited@Example.org", "h1")
		mk("i2", "tb", " invited@example.org ", "h2")
		mk("i3", "ta", "invited@example.org.uk", "h3")
		mk("i4", "ta", "other@example.org", "h4")
		n, err := s.DeleteInvitationsByEmail(ctx, "INVITED@example.org")
		if err != nil || n != 2 {
			t.Fatalf("DeleteInvitationsByEmail = %d (%v), want 2", n, err)
		}
		for _, gone := range []string{"i1", "i2"} {
			if _, err := s.GetInvitation(ctx, gone); !errors.Is(err, ErrNotFound) {
				t.Fatalf("invitation %s survived: %v", gone, err)
			}
		}
		if _, err := s.GetInvitationByTokenHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the token of a deleted invitation still resolves: %v", err)
		}
		for _, kept := range []string{"i3", "i4"} {
			if _, err := s.GetInvitation(ctx, kept); err != nil {
				t.Fatalf("invitation %s went too: %v", kept, err)
			}
		}
		if n, err := s.DeleteInvitationsByEmail(ctx, "invited@example.org"); err != nil || n != 0 {
			t.Fatalf("a second sweep answered %d (%v), want 0", n, err)
		}
		if _, err := s.DeleteInvitationsByEmail(ctx, "  "); err == nil {
			t.Fatal("a sweep naming no address answered no error")
		}
	})

	t.Run("the invitations the account created go, in every team", func(t *testing.T) {
		mk := func(id, team, inviter, hash string) {
			t.Helper()
			if err := s.CreateInvitation(ctx, Invitation{ID: id, TeamID: team, Email: id + "@example.org", Role: RoleMember, TokenHash: hash, InvitedBy: inviter, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				t.Fatalf("seed invitation %s: %v", id, err)
			}
		}
		mk("m1", "ta", "erased-inviter", "mh1")
		mk("m2", "tb", "erased-inviter", "mh2")
		mk("m3", "ta", "other-inviter", "mh3")
		n, err := s.DeleteInvitationsByInviter(ctx, "erased-inviter")
		if err != nil || n != 2 {
			t.Fatalf("DeleteInvitationsByInviter = %d (%v), want 2", n, err)
		}
		for _, gone := range []string{"m1", "m2"} {
			if _, err := s.GetInvitation(ctx, gone); !errors.Is(err, ErrNotFound) {
				t.Fatalf("invitation %s survived: %v", gone, err)
			}
		}
		if _, err := s.GetInvitationByTokenHash(ctx, "mh2"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the token of a deleted invitation still resolves: %v", err)
		}
		if _, err := s.GetInvitation(ctx, "m3"); err != nil {
			t.Fatalf("another inviter's invitation went too: %v", err)
		}
		if n, err := s.DeleteInvitationsByInviter(ctx, "erased-inviter"); err != nil || n != 0 {
			t.Fatalf("a second sweep answered %d (%v), want 0", n, err)
		}
		if _, err := s.DeleteInvitationsByInviter(ctx, ""); err == nil {
			t.Fatal("a sweep naming no inviter answered no error")
		}
	})

	t.Run("the mark refuses every sign-in and patch first, keeps the address, and the anonymization follows it", func(t *testing.T) {
		marked := User{ID: "mark-me", Email: "mark.me@example.org", Name: "Mark Me", PasswordHash: "hash",
			Status: UserStatusActive, CreatedAt: now, UpdatedAt: now, GitHubOrgs: []string{"acme"}}
		if _, err := s.CreateUser(ctx, marked); err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := s.MarkUserErased(ctx, "mark-me")
		if err != nil {
			t.Fatalf("MarkUserErased: %v", err)
		}
		if got.Status != UserStatusErased || got.Email != "mark.me@example.org" || got.Name != "Mark Me" || got.PasswordHash != "hash" {
			t.Fatalf("marked row = %+v, want the erased status and every other field kept", got)
		}
		if !got.Status.SignInRefused() {
			t.Fatal("the marked status admits a sign-in")
		}
		if byAddr, err := s.GetUserByEmail(ctx, "mark.me@example.org"); err != nil || byAddr.ID != "mark-me" {
			t.Fatalf("the address no longer resolves after the mark: %+v (%v)", byAddr, err)
		}
		active := UserStatusActive
		if _, err := s.PatchUser(ctx, "mark-me", UserPatch{Status: &active}); !errors.Is(err, ErrUserStatusChanged) {
			t.Fatalf("a re-enable of a marked row = %v, want ErrUserStatusChanged", err)
		}
		again, err := s.MarkUserErased(ctx, "mark-me")
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("a second mark = %+v (%v), want the row unchanged %+v", again, err, got)
		}
		erased, err := s.EraseUser(ctx, "mark-me")
		if err != nil {
			t.Fatalf("EraseUser on a marked row: %v", err)
		}
		if want := erasedUser(marked, ErasedEmail("mark-me"), erased.UpdatedAt); !reflect.DeepEqual(erased, want) {
			t.Fatalf("a marked row anonymized = %+v, want %+v", erased, want)
		}
		if _, err := s.MarkUserErased(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("MarkUserErased(nobody) = %v, want ErrNotFound", err)
		}
	})
}

func TestMemoryStore_Erase(t *testing.T) { runEraseSuite(t, NewMemoryStore()) }

// TestMongoStore_Erase runs the erasure suite against a real Mongo, gated on
// ITERION_TEST_MONGO_URI like the sibling suites.
func TestMongoStore_Erase(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo identity erase suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_identity_erase_" + hex.EncodeToString(nonce))
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
	runEraseSuite(t, st)
}

// TestEraseUserUpdateKeysMatchTheBSONTags is the Mongo leg without a Mongo:
// the `$set`/`$unset` document applied through the driver's codec to a row
// with every field at a distinguishable value must give the memory twin's
// erasedUser. A key that does not match a bson tag would leave the field
// standing — a name or a password hash surviving an erasure, silently.
func TestEraseUserUpdateKeysMatchTheBSONTags(t *testing.T) {
	seed := userPatchSeed()
	// The `$unset` is a list of keys: a User field the seed leaves at its
	// zero value would survive an erasure on Mongo while this comparison
	// stayed green. Every field of the seed is set, so a new field reddens
	// here until the seed — and then the erasure — names it.
	sv := reflect.ValueOf(seed)
	for i := range sv.NumField() {
		if sv.Field(i).IsZero() {
			t.Errorf("userPatchSeed leaves User.%s at its zero value: set it, so the erasure's $unset is checked against it", sv.Type().Field(i).Name)
		}
	}
	raw, err := bson.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal seed: %v", err)
	}
	update := eraseUserUpdate(seed.ID, patchAfterTime)
	for k, v := range update["$set"].(bson.M) {
		doc[k] = v
	}
	for k := range update["$unset"].(bson.M) {
		if _, present := doc[k]; !present {
			t.Errorf("$unset names %q, which the seed row does not carry: a key that matches no bson tag", k)
		}
		delete(doc, k)
	}
	out, err := bson.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal erased document: %v", err)
	}
	var got User
	if err := bson.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal erased document: %v", err)
	}
	want := erasedUser(seed, ErasedEmail(seed.ID), patchAfterTime)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the update applied through bson gives %+v, want %+v", got, want)
	}
}
