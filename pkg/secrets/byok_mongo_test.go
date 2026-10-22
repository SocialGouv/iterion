package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The tenant filter on the key store is invisible to MemoryApiKeyStore,
// which keys on id alone. That divergence is precisely what let a
// cross-tenant read ship: every in-memory test of the credential pool
// passed while the Mongo store found nothing. These run against a real
// Mongo (same gating as the other conformance suites — CI's
// mongo-conformance job sets ITERION_TEST_MONGO_URI).
func mongoKeyStore(t *testing.T) (*MongoApiKeyStore, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo api-key suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_byok_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	return NewMongoApiKeyStore(db), ctx
}

// A pooled key is read from the BORROWER's context — another tenant than
// the donor's. Get cannot serve that read, and a caller using it would
// mistake "wrong tenant" for "key deleted" and park an innocent donor's
// contribution. GetOwned is the deliberate cross-tenant door, bounded by
// ownership instead.
func TestMongoApiKey_GetOwnedCrossesTenantsWhereGetCannot(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	donorCtx := store.WithTenant(ctx, "team-donor")
	if err := s.Create(donorCtx, ApiKey{
		ID: "key-1", TenantID: "team-donor", ScopeTeamID: "team-donor",
		ScopeUserID: "alice", Provider: ProviderAnthropic, Name: "lent",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	borrowerCtx := store.WithTenant(ctx, "team-borrower")

	// The tenant-scoped read is blind to it — the defect's mechanism.
	if _, err := s.Get(borrowerCtx, "key-1"); !errors.Is(err, ErrApiKeyNotFound) {
		t.Errorf("Get from another tenant = %v, want ErrApiKeyNotFound", err)
	}

	// The owner-scoped read serves it, which is what makes lending work.
	got, err := s.GetOwned(borrowerCtx, "key-1", "alice")
	if err != nil {
		t.Fatalf("GetOwned from the borrower's tenant: %v", err)
	}
	if got.ID != "key-1" || got.ScopeUserID != "alice" {
		t.Errorf("got %+v, want alice's key-1", got)
	}
}

// Ownership is the whole boundary GetOwned offers: it must refuse a key
// that is not the named user's, and a team-wide key (no owner) outright —
// a team key is the team's to spend, never one member's to lend.
func TestMongoApiKey_GetOwnedRefusesWhatIsNotYours(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	tctx := store.WithTenant(ctx, "team-1")
	if err := s.Create(tctx, ApiKey{
		ID: "key-personal", TenantID: "team-1", ScopeTeamID: "team-1",
		ScopeUserID: "alice", Provider: ProviderAnthropic, Name: "alice's",
	}); err != nil {
		t.Fatalf("create personal: %v", err)
	}
	if err := s.Create(tctx, ApiKey{
		ID: "key-team", TenantID: "team-1", ScopeTeamID: "team-1",
		Provider: ProviderAnthropic, Name: "the team's",
	}); err != nil {
		t.Fatalf("create team: %v", err)
	}

	for _, tc := range []struct{ name, id, owner string }{
		{"another user's key", "key-personal", "mallory"},
		{"a team-wide key has no owner to lend it", "key-team", "alice"},
		{"no owner named at all", "key-personal", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.GetOwned(tctx, tc.id, tc.owner); !errors.Is(err, ErrApiKeyNotFound) {
				t.Errorf("GetOwned = %v, want ErrApiKeyNotFound", err)
			}
		})
	}
}

// The store stamps tenant_id from the CONTEXT and filters reads by it, so a
// key created under one tenant's context but scoped to another team is
// unreachable by the team it was meant to fund — a run there resolves nothing
// and falls back to the platform credential, silently. This pins the contract
// the api-keys HTTP layer must honour: create under the context of the team
// the key is FOR.
func TestMongoApiKey_ScopeWithoutMatchingTenantIsUnreachable(t *testing.T) {
	s, ctx := mongoKeyStore(t)

	// The defect shape: written under team-a's context, scoped to team-b.
	if err := s.Create(store.WithTenant(ctx, "team-a"), ApiKey{
		ID: "mis-stamped", ScopeTeamID: "team-b",
		Provider: ProviderAnthropic, Name: "meant for team-b", IsDefault: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.ListByTeam(store.WithTenant(ctx, "team-b"), "team-b", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a key stamped with another tenant must NOT be reachable as team-b's, got %d", len(got))
	}

	// The correct shape: written under the context of the team it is for.
	if err := s.Create(store.WithTenant(ctx, "team-b"), ApiKey{
		ID: "well-stamped", ScopeTeamID: "team-b",
		Provider: ProviderAnthropic, Name: "for team-b", IsDefault: true,
	}); err != nil {
		t.Fatalf("create scoped: %v", err)
	}
	got, err = s.ListByTeam(store.WithTenant(ctx, "team-b"), "team-b", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "well-stamped" {
		t.Fatalf("team-b must see exactly its own key, got %+v", got)
	}
	// From team-a's context the WELL-stamped key is out of reach — while the
	// mis-stamped one still answers, which is the whole asymmetry: the row is
	// visible to whoever created it and not to whoever it belongs to.
	other, err := s.ListByTeam(store.WithTenant(ctx, "team-a"), "team-b", "")
	if err != nil {
		t.Fatalf("list from team-a: %v", err)
	}
	for _, k := range other {
		if k.ID == "well-stamped" {
			t.Fatalf("team-b's key must not be reachable from team-a's context")
		}
	}
	if len(other) != 1 || other[0].ID != "mis-stamped" {
		t.Fatalf("the mis-stamped row is the one that answers here, got %+v", other)
	}
}

// The Mongo twin of TestMemoryApiKey_MarkFingerprintUsed: the metering
// bump must land in prod on the real store, not only in unit-test
// memory. Skipped without ITERION_TEST_MONGO_URI (mongo-conformance CI
// job gates it, like the other Mongo suites in this file).
func TestMongoApiKey_MarkFingerprintUsed(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	// The bump runs at every attempt start and end with no tenant filter;
	// its predicate must be indexed or it is a collection scan per
	// attempt (#659). The schema is the contract, so it is asserted here.
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	cur, err := s.coll.Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var indexes []struct {
		Name string `bson:"name"`
	}
	if err := cur.All(ctx, &indexes); err != nil {
		t.Fatalf("decode indexes: %v", err)
	}
	hasFP := false
	for _, ix := range indexes {
		if ix.Name == "fingerprint" {
			hasFP = true
		}
	}
	if !hasFP {
		t.Fatalf("no index on fingerprint after EnsureSchema; got %+v", indexes)
	}

	// Two rows sharing a fingerprint (an operator saved the same secret
	// twice on different tenants) — the update must land on BOTH, so a
	// run spending either row moves the meter on both.
	if err := s.Create(store.WithTenant(ctx, "team-a"), ApiKey{
		ID: "k1", TenantID: "team-a", ScopeTeamID: "team-a",
		ScopeUserID: "u1", Provider: ProviderAnthropic, Name: "k1", Fingerprint: "fp-shared",
	}); err != nil {
		t.Fatalf("create k1: %v", err)
	}
	if err := s.Create(store.WithTenant(ctx, "team-b"), ApiKey{
		ID: "k2", TenantID: "team-b", ScopeTeamID: "team-b",
		ScopeUserID: "u2", Provider: ProviderAnthropic, Name: "k2", Fingerprint: "fp-shared",
	}); err != nil {
		t.Fatalf("create k2: %v", err)
	}
	if err := s.Create(store.WithTenant(ctx, "team-c"), ApiKey{
		ID: "k3", TenantID: "team-c", ScopeTeamID: "team-c",
		ScopeUserID: "u3", Provider: ProviderAnthropic, Name: "k3", Fingerprint: "fp-untouched",
	}); err != nil {
		t.Fatalf("create k3: %v", err)
	}

	if err := s.MarkFingerprintUsed(ctx, "", now); err != nil {
		t.Errorf("empty fp errored: %v", err)
	}
	if err := s.MarkFingerprintUsed(ctx, "fp-shared", now); err != nil {
		t.Fatalf("MarkFingerprintUsed: %v", err)
	}
	k1, err := s.Get(store.WithTenant(ctx, "team-a"), "k1")
	if err != nil {
		t.Fatalf("read k1: %v", err)
	}
	if k1.LastUsedAt == nil || !k1.LastUsedAt.Equal(now) {
		t.Errorf("k1.last_used_at = %v, want %v", k1.LastUsedAt, now)
	}
	k2, err := s.Get(store.WithTenant(ctx, "team-b"), "k2")
	if err != nil {
		t.Fatalf("read k2: %v", err)
	}
	if k2.LastUsedAt == nil || !k2.LastUsedAt.Equal(now) {
		t.Errorf("k2.last_used_at = %v, want %v (a shared fingerprint must land on every matching row)", k2.LastUsedAt, now)
	}
	k3, err := s.Get(store.WithTenant(ctx, "team-c"), "k3")
	if err != nil {
		t.Fatalf("read k3: %v", err)
	}
	if k3.LastUsedAt != nil {
		t.Errorf("k3.last_used_at = %v, want nil (its fingerprint was untouched)", k3.LastUsedAt)
	}
}

// The row may carry columns this build does not know — a newer server in
// the same rolling update, or the one this pod was rolled back from, wrote
// them — and the caller's copy is stale on everything it does not name. A
// patch is one $set of the named fields: the unknown columns and the
// sealed blob survive byte for byte, the named clears persist as writes,
// and last_used_at (the runner's field) is never touched.
func TestMongoApiKey_PatchKeepsTheColumnsThisBuildDoesNotKnow(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	tctx := store.WithTenant(ctx, "team-a")
	used := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	raw := bson.M{
		"_id": "key-gw", "tenant_id": "team-a", "scope_team": "team-a",
		"provider": "openai_compatible", "name": "pliage",
		"base_url":      "https://gateway.example.test",
		"future_field":  "kept",
		"sealed_secret": []byte("sealed-under-newer-rules"),
		"is_default":    true, "bots": []string{"review-pr"}, "max_concurrent_runs": 3,
		"created_at": used, "last_used_at": used,
	}
	if _, err := s.coll.InsertOne(ctx, raw); err != nil {
		t.Fatalf("insert raw row: %v", err)
	}

	name, off, zero := "renamed", false, 0
	got, err := s.Patch(tctx, "key-gw", ApiKeyPatch{Name: &name, IsDefault: &off, MaxConcurrentRuns: &zero, Bots: &[]string{}})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got.Name != "renamed" || got.IsDefault || got.MaxConcurrentRuns != 0 || len(got.Bots) != 0 {
		t.Fatalf("named fields did not land: %+v", got)
	}

	var back bson.M
	if err := s.coll.FindOne(ctx, bson.M{"_id": "key-gw"}).Decode(&back); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back["base_url"] != "https://gateway.example.test" || back["future_field"] != "kept" {
		t.Fatalf("a column this build does not know was dropped: %v", back)
	}
	if sealed, _ := back["sealed_secret"].(bson.Binary); string(sealed.Data) != "sealed-under-newer-rules" {
		t.Fatalf("the sealed blob moved: %v", back["sealed_secret"])
	}
	if back["name"] != "renamed" || back["is_default"] != false || back["max_concurrent_runs"] != int32(0) {
		t.Fatalf("the clears did not persist as writes: %v", back)
	}
	if bots, _ := back["bots"].(bson.A); len(bots) != 0 {
		t.Fatalf("bots was not cleared to an empty array: %v", back["bots"])
	}
	if lu, _ := back["last_used_at"].(bson.DateTime); lu.Time().UTC() != used {
		t.Fatalf("last_used_at is the runner's field and must not move: %v", back["last_used_at"])
	}

	// Tenancy: the same patch from another tenant finds nothing.
	if _, err := s.Patch(store.WithTenant(ctx, "team-b"), "key-gw", ApiKeyPatch{Name: &name}); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("patch from another tenant = %v, want ErrApiKeyNotFound", err)
	}
	// And a tenant-less context is refused outright by Patch.
	if _, err := s.Patch(ctx, "key-gw", ApiKeyPatch{Name: &name}); !errors.Is(err, ErrApiKeyTenantMissing) {
		t.Fatalf("tenant-less patch = %v, want ErrApiKeyTenantMissing", err)
	}
}

// Every field a patch names lands on the REAL store in the setting
// direction too, one field per step, re-read through Get rather than
// trusted from the returned value — the witness that holds setDoc to the
// field list applyTo keeps for the memory twin.
func TestMongoApiKey_PatchSetsEachNamedField(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	tctx := store.WithTenant(ctx, "team-a")
	if err := s.Create(tctx, ApiKey{ID: "k", ScopeTeamID: "team-a", Provider: ProviderAnthropic, Name: "n",
		SealedSecret: []byte("s1"), Last4: "1111", Fingerprint: "fp1"}); err != nil {
		t.Fatal(err)
	}
	name, on, seven, gw := "renamed", true, 7, "https://gw.example.test"
	steps := []struct {
		p     ApiKeyPatch
		check func(ApiKey) bool
	}{
		{ApiKeyPatch{Name: &name}, func(k ApiKey) bool { return k.Name == "renamed" }},
		{ApiKeyPatch{IsDefault: &on}, func(k ApiKey) bool { return k.IsDefault }},
		{ApiKeyPatch{MaxConcurrentRuns: &seven}, func(k ApiKey) bool { return k.MaxConcurrentRuns == 7 }},
		{ApiKeyPatch{Bots: &[]string{"review-pr"}}, func(k ApiKey) bool { return len(k.Bots) == 1 && k.Bots[0] == "review-pr" }},
		{ApiKeyPatch{Repos: &[]string{"acme/api"}}, func(k ApiKey) bool { return len(k.Repos) == 1 && k.Repos[0] == "acme/api" }},
		{ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("s2"), Last4: "2222", Fingerprint: "fp2"}},
			func(k ApiKey) bool {
				return string(k.SealedSecret) == "s2" && k.Last4 == "2222" && k.Fingerprint == "fp2"
			}},
		// The URL lands in its own column, beside the blob sealed under it.
		{ApiKeyPatch{BaseURL: &gw, Secret: &ApiKeySecretPatch{Sealed: []byte("s3"), Last4: "3333", Fingerprint: "fp3"}},
			func(k ApiKey) bool {
				return k.BaseURL == gw && string(k.SealedSecret) == "s3" && k.Fingerprint == "fp3"
			}},
	}
	for i, st := range steps {
		if _, err := s.Patch(tctx, "k", st.p); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		back, err := s.Get(tctx, "k")
		if err != nil || !st.check(back) {
			t.Fatalf("step %d did not land on the real store: %+v (%v)", i, back, err)
		}
	}
	// An incomplete rotation never reaches the store, nor a URL without
	// the blob sealed under it.
	if _, err := s.Patch(tctx, "k", ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("s4")}}); !errors.Is(err, ErrApiKeyPatchInvalid) {
		t.Fatalf("incomplete rotation = %v, want ErrApiKeyPatchInvalid", err)
	}
	moved := "https://gw2.example.test"
	if _, err := s.Patch(tctx, "k", ApiKeyPatch{BaseURL: &moved}); !errors.Is(err, ErrApiKeyPatchInvalid) {
		t.Fatalf("a base URL without its secret = %v, want ErrApiKeyPatchInvalid", err)
	}
	if back, _ := s.Get(tctx, "k"); string(back.SealedSecret) != "s3" || back.Fingerprint != "fp3" || back.BaseURL != gw {
		t.Fatalf("a refused patch landed anyway: %+v", back)
	}
}

// The compare-and-swap holds on the real store, and a miss on a row that
// still exists is reported as "changed", not "gone".
func TestMongoApiKey_PatchWithExpectSealedIsACompareAndSwap(t *testing.T) {
	s, ctx := mongoKeyStore(t)
	tctx := store.WithTenant(ctx, "team-a")
	if err := s.Create(tctx, ApiKey{ID: "cas", ScopeTeamID: "team-a", Provider: ProviderAnthropic, Name: "k", SealedSecret: []byte("v1"), Fingerprint: "fp1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Patch(tctx, "cas", ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("v2"), Last4: "2222", Fingerprint: "fp2"}}); err != nil {
		t.Fatal(err)
	}
	stale := ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("v1b"), Last4: "1111", Fingerprint: "fp1b"}, ExpectSealed: []byte("v1")}
	if _, err := s.Patch(tctx, "cas", stale); !errors.Is(err, ErrApiKeyChanged) {
		t.Fatalf("stale re-seal = %v, want ErrApiKeyChanged", err)
	}
	if back, _ := s.Get(tctx, "cas"); string(back.SealedSecret) != "v2" {
		t.Fatalf("the stale re-seal landed: %+v", back)
	}
	if _, err := s.Patch(tctx, "cas", ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("v3"), Last4: "3333", Fingerprint: "fp3"}, ExpectSealed: []byte("v2")}); err != nil {
		t.Fatalf("a current re-seal must land: %v", err)
	}
	// An empty patch still answers the guard, as the memory twin does.
	if _, err := s.Patch(tctx, "cas", ApiKeyPatch{ExpectSealed: []byte("v2")}); !errors.Is(err, ErrApiKeyChanged) {
		t.Fatalf("an empty patch with a stale guard = %v, want ErrApiKeyChanged", err)
	}
	if got, err := s.Patch(tctx, "cas", ApiKeyPatch{ExpectSealed: []byte("v3")}); err != nil || string(got.SealedSecret) != "v3" {
		t.Fatalf("an empty patch with the current guard must answer the row: %+v %v", got, err)
	}
	if _, err := s.Patch(tctx, "missing", ApiKeyPatch{Secret: &ApiKeySecretPatch{Sealed: []byte("x"), Last4: "xxxx", Fingerprint: "fpx"}, ExpectSealed: []byte("v3")}); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("a missing row under CAS = %v, want ErrApiKeyNotFound", err)
	}
}
