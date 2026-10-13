package identity

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// The Mongo leg of PatchUser needs a server: TestMongoStore_Patch is gated on
// ITERION_TEST_MONGO_URI and only CI's mongo-conformance job provides one, so
// a developer's run leaves the `$set` and the guard filter unexecuted. The two
// tests below make that leg's riskiest halves executable anywhere, against the
// production builders themselves:
//
//   - the KEYS. A `$set` key that does not match the field's bson tag writes a
//     NEW document field and leaves the old value standing — silently, which is
//     the trap runPatchSuite's docstring names. Applying the real `$set` through
//     the real bson codec and comparing the row against the memory twin's
//     catches both a misspelled key and a key wired to the wrong field.
//   - the PRECONDITION. The `$nin` arm is the whole guard on this leg. Whether
//     Mongo honours it atomically is Mongo's to guarantee and mongo-conformance
//     to prove; whether PatchUser ASKS for it is decided here.

// userPatchFixtureTimes are millisecond-aligned: BSON stores a datetime as
// int64 milliseconds, so a nanosecond in a fixture would make the round-trip
// differ for a reason that has nothing to do with the keys under test.
var (
	patchSeedTime  = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	patchAfterTime = time.Date(2026, 4, 2, 11, 30, 0, 0, time.UTC)
)

// userPatchSeed is a row with every patchable field at a value distinguishable
// from what the patches below write, so an unwritten field is visible.
func userPatchSeed() User {
	locked := patchSeedTime.Add(time.Hour)
	login := patchSeedTime.Add(2 * time.Hour)
	return User{
		ID:            "patch-keys",
		Email:         "patch-keys@example.org",
		Name:          "Seed Name",
		PasswordHash:  "seed-hash",
		Status:        UserStatusActive,
		IsSuperAdmin:  true,
		CreatedAt:     patchSeedTime,
		UpdatedAt:     patchSeedTime,
		LastLoginAt:   &login,
		FailedLogins:  7,
		LockedUntil:   &locked,
		DefaultTeamID: "seed-team",
		DefaultOrgID:  "seed-org",
		GitHubOrgs:    []string{"seed-org-login"},
	}
}

// applyUserPatchSetThroughBSON is the Mongo leg without a Mongo: it encodes
// the seed row with the driver's own codec, applies PatchUser's `$set`
// document key by key the way the server would, and decodes the result back
// into a User. A key the User type does not carry survives in the document
// and changes nothing in the decoded row — exactly Mongo's behaviour.
func applyUserPatchSetThroughBSON(t *testing.T, seed User, p UserPatch) User {
	t.Helper()
	raw, err := bson.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal seed into a document: %v", err)
	}
	for k, v := range userPatchSet(p, patchAfterTime) {
		doc[k] = v
	}
	out, err := bson.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal patched document: %v", err)
	}
	var got User
	if err := bson.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode patched document: %v", err)
	}
	return got
}

// normalizeUserForComparison drops what the two legs are entitled to disagree
// on: UpdatedAt (each stamps its own clock) and the sub-millisecond precision
// BSON cannot carry.
func normalizeUserForComparison(u User) User {
	u.UpdatedAt = time.Time{}
	u.CreatedAt = u.CreatedAt.UTC().Truncate(time.Millisecond)
	if u.LastLoginAt != nil {
		at := u.LastLoginAt.UTC().Truncate(time.Millisecond)
		u.LastLoginAt = &at
	}
	if u.LockedUntil != nil {
		at := u.LockedUntil.UTC().Truncate(time.Millisecond)
		u.LockedUntil = &at
	}
	return u
}

// TestUserPatchSetWritesTheSameRowAsTheMemoryTwin drives one patch per field:
// a key wired to the wrong field reddens twice over — on the field it starved
// and on the field it stole — which a single all-fields patch cannot show.
func TestUserPatchSetWritesTheSameRowAsTheMemoryTwin(t *testing.T) {
	name, hash, team, org := "After Name", "after-hash", "after-team", "after-org"
	pending := UserStatusPendingPasswordChange
	notSuper := false
	orgs := []string{"after-one", "after-two"}
	noOrgs := []string{}
	// A non-nil pointer to a NIL slice: the shape a caller producing an empty
	// list with `var x []string` hands over. It marshals as BSON null unless
	// the $set copies it, which would read back as a nil list on one leg and
	// an empty one on the other.
	var nilOrgs []string
	lockedAt := patchAfterTime.Add(3 * time.Hour)

	cases := []struct {
		field string
		patch UserPatch
	}{
		{"name", UserPatch{Name: &name}},
		{"password_hash", UserPatch{PasswordHash: &hash}},
		{"status", UserPatch{Status: &pending}},
		{"is_super_admin", UserPatch{IsSuperAdmin: &notSuper}},
		{"default_team_id", UserPatch{DefaultTeamID: &team}},
		{"default_org_id", UserPatch{DefaultOrgID: &org}},
		{"github_orgs", UserPatch{GitHubOrgs: &orgs}},
		{"github_orgs cleared", UserPatch{GitHubOrgs: &noOrgs}},
		{"github_orgs cleared through a nil slice", UserPatch{GitHubOrgs: &nilOrgs}},
		{"last_login_at", UserPatch{LastLoginAt: &patchAfterTime}},
		{"lockout set", UserPatch{Lockout: &UserLockout{FailedLogins: 2, LockedUntil: &lockedAt}}},
		{"lockout cleared", UserPatch{Lockout: &UserLockout{}}},
		{"every field at once", UserPatch{
			Name: &name, PasswordHash: &hash, Status: &pending, IsSuperAdmin: &notSuper,
			DefaultTeamID: &team, DefaultOrgID: &org, GitHubOrgs: &orgs,
			LastLoginAt: &patchAfterTime,
			Lockout:     &UserLockout{FailedLogins: 2, LockedUntil: &lockedAt},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			seed := userPatchSeed()
			mem := NewMemoryStore()
			if _, err := mem.CreateUser(context.Background(), seed); err != nil {
				t.Fatalf("seed the memory twin: %v", err)
			}
			want, err := mem.PatchUser(context.Background(), seed.ID, tc.patch)
			if err != nil {
				t.Fatalf("memory PatchUser: %v", err)
			}
			got := applyUserPatchSetThroughBSON(t, seed, tc.patch)
			if !reflect.DeepEqual(normalizeUserForComparison(got), normalizeUserForComparison(want)) {
				t.Errorf("the $set and the memory twin disagree for %s:\n mongo = %+v\nmemory = %+v",
					tc.field, got, want)
			}
			// The stamp is the one key every non-empty patch carries.
			if !got.UpdatedAt.Equal(patchAfterTime) {
				t.Errorf("updated_at not written by the $set: %v", got.UpdatedAt)
			}
		})
	}
}

// TestUserPatchFilterCarriesThePrecondition pins the query PatchUser hands
// Mongo. It is the only local witness that the guard is ASKED for on this
// leg: drop the `$nin` arm and the write lands on a row a deactivation
// reached first, with nothing but the Mongo-only conformance run to notice.
func TestUserPatchFilterCarriesThePrecondition(t *testing.T) {
	t.Run("no precondition selects the row by _id alone", func(t *testing.T) {
		got := userPatchFilter("u1", UserPatch{Status: ptr(UserStatusActive)})
		if !reflect.DeepEqual(got, bson.M{"_id": "u1"}) {
			t.Errorf("filter = %+v, want {_id: u1}", got)
		}
	})

	t.Run("a precondition adds a $nin arm naming every refused status", func(t *testing.T) {
		got := userPatchFilter("u1", UserPatch{
			IfNotStatus: []UserStatus{UserStatusDisabled, UserStatusPendingPasswordChange},
		})
		want := bson.M{"_id": "u1", "status": bson.M{"$nin": []string{"disabled", "pending_password_change"}}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("filter = %+v, want %+v", got, want)
		}
	})
}

func ptr[T any](v T) *T { return &v }
