package secrets

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

// The memory store replaces the whole record on Upsert, so a cleared label
// clears in every in-memory test whatever the bson tags say. The Mongo
// store writes through $set, where an omitted key keeps the OLD value —
// the shape of bug only a real Mongo can show. Same gating as the other
// conformance suites (CI's mongo-conformance job sets ITERION_TEST_MONGO_URI).
func mongoOAuthStore(t *testing.T) (*MongoOAuthStore, context.Context) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo oauth suite")
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	db := client.Database("iterion_oauth_" + hex.EncodeToString(nonce))
	t.Cleanup(func() {
		drop, dropCancel := mongotest.TeardownCtx()
		defer dropCancel()
		_ = db.Drop(drop)
		_ = client.Disconnect(drop)
	})
	s := NewMongoOAuthStore(db)
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return s, ctx
}

// Clearing the label must clear it on the wire the production store uses:
// with a bson omitempty on the field, the $set body carried no key for an
// empty label and the API reported a clear that never happened.
func TestMongoOAuth_EmptyLabelClearsThroughUpsert(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	rec := OAuthRecord{UserID: "alice", Kind: OAuthKindClaudeCode, SealedPayload: []byte("sealed"), Fingerprint: "fp-1", AccountLabel: "old name"}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec.AccountLabel = ""
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "" {
		t.Fatalf("label after an empty-label upsert = %q, want cleared (bson omitempty would keep the stale name)", got.AccountLabel)
	}
}

// A rename touches two keys and nothing else: the sealed payload and the
// fingerprint stay whatever the last connect/refresh wrote, even when the
// caller's copy of the record is stale.
func TestMongoOAuth_SetAccountLabelIsMetadataOnly(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindCodex, "x"); !errors.Is(err, ErrOAuthNotFound) {
		t.Fatalf("SetAccountLabel on a missing record = %v, want ErrOAuthNotFound", err)
	}
	if err := s.Upsert(ctx, OAuthRecord{UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed-v1"), Fingerprint: "fp-1"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A refresh lands a new payload before the rename is written.
	if err := s.Upsert(ctx, OAuthRecord{UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed-v2"), Fingerprint: "fp-1"}); err != nil {
		t.Fatalf("refresh upsert: %v", err)
	}
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindCodex, "alice@openai"); err != nil {
		t.Fatalf("SetAccountLabel: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "alice@openai" {
		t.Fatalf("label = %q", got.AccountLabel)
	}
	if string(got.SealedPayload) != "sealed-v2" || got.Fingerprint != "fp-1" {
		t.Fatalf("rename disturbed the credential: payload=%q fp=%q", got.SealedPayload, got.Fingerprint)
	}
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindCodex, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ = s.Get(ctx, "alice", OAuthKindCodex); got.AccountLabel != "" {
		t.Fatalf("label after clear = %q, want empty", got.AccountLabel)
	}
}

// The refresh's half of the rename race, on the wire the production store
// uses: the persist must $set only the token keys, so a rename committed
// during the provider round trip is not reverted. (The memory store would
// pass this even through a whole-record write in the wrong direction, so
// only Mongo can prove the $set body.)
func TestMongoOAuth_UpdateTokensIsRefreshOnly(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	if err := s.UpdateTokens(ctx, "alice", OAuthKindClaudeCode, OAuthTokenUpdate{}); !errors.Is(err, ErrOAuthNotFound) {
		t.Fatalf("UpdateTokens on a missing record = %v, want ErrOAuthNotFound", err)
	}
	if err := s.Upsert(ctx, OAuthRecord{
		UserID: "alice", Kind: OAuthKindClaudeCode,
		SealedPayload: []byte("sealed-v1"), Fingerprint: "fp-1", AccountLabel: "old name",
		Scopes: []string{"read"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// The operator renames while the refresh is out at the provider.
	if err := s.SetAccountLabel(ctx, "alice", OAuthKindClaudeCode, "jothedev"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	exp := time.Now().Add(8 * time.Hour).UTC().Truncate(time.Millisecond)
	last := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.UpdateTokens(ctx, "alice", OAuthKindClaudeCode, OAuthTokenUpdate{
		SealedPayload:        []byte("sealed-v2"),
		AccessTokenExpiresAt: &exp,
		LastRefreshedAt:      &last,
		Scopes:               []string{"read", "write"},
		Fingerprint:          "fp-1",
	}); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "jothedev" {
		t.Fatalf("account label = %q, want the rename to survive the refresh", got.AccountLabel)
	}
	if string(got.SealedPayload) != "sealed-v2" {
		t.Fatalf("payload = %q, want the refreshed blob", got.SealedPayload)
	}
	if got.AccessTokenExpiresAt == nil || !got.AccessTokenExpiresAt.Equal(exp) {
		t.Fatalf("expiry = %v, want %v", got.AccessTokenExpiresAt, exp)
	}
	if got.LastRefreshedAt == nil || !got.LastRefreshedAt.Equal(last) {
		t.Fatalf("last_refreshed_at = %v, want %v", got.LastRefreshedAt, last)
	}
	if len(got.Scopes) != 2 {
		t.Fatalf("scopes = %v, want the refreshed pair", got.Scopes)
	}
	if got.NotRefreshable {
		t.Fatal("a successful refresh must leave not_refreshable false")
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at was cleared by the partial write")
	}

	// The self-heal shape: one flag, nothing else disturbed.
	if err := s.UpdateTokens(ctx, "alice", OAuthKindClaudeCode, OAuthTokenUpdate{NotRefreshable: true}); err != nil {
		t.Fatalf("self-heal: %v", err)
	}
	got, err = s.Get(ctx, "alice", OAuthKindClaudeCode)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NotRefreshable {
		t.Fatal("not_refreshable was not set")
	}
	if string(got.SealedPayload) != "sealed-v2" || got.Fingerprint != "fp-1" || got.AccountLabel != "jothedev" {
		t.Fatalf("self-heal disturbed the record: payload=%q fp=%q label=%q", got.SealedPayload, got.Fingerprint, got.AccountLabel)
	}
}

// The refresh claim, on the wire the production store uses. Three of its
// four properties are Mongo-only in nature: the CAS is a filtered update
// (the memory twin can only imitate it), and a claim is cleared by writing
// EXPLICIT empty/null keys — through $set, an omitted key leaves the old
// value in place, so a claim field carrying bson omitempty would strand the
// record on the first holder that crashed. That is the AccountLabel trap
// one field over, applied to a lock.
func TestMongoOAuth_RefreshClaimIsFencedOnTheWire(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	if err := s.Upsert(ctx, OAuthRecord{
		UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed-v1"), Fingerprint: "fp-1",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now().UTC()

	// A record nobody has claimed carries no cool-down, so the CAS matches.
	ok, err := s.ClaimRefresh(ctx, "alice", OAuthKindCodex, "owner-a", now, now.Add(2*time.Minute))
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v, want acquired", ok, err)
	}
	// While it stands, nobody else may exchange.
	ok, err = s.ClaimRefresh(ctx, "alice", OAuthKindCodex, "owner-b", now, now.Add(2*time.Minute))
	if err != nil || ok {
		t.Fatalf("competing claim: ok=%v err=%v, want refused", ok, err)
	}
	// Neither may they release it or commit through it.
	if err := s.ReleaseRefreshClaim(ctx, "alice", OAuthKindCodex, "owner-b", nil); !errors.Is(err, ErrRefreshClaimLost) {
		t.Fatalf("release by a non-owner = %v, want ErrRefreshClaimLost", err)
	}
	if err := s.UpdateTokens(ctx, "alice", OAuthKindCodex,
		OAuthTokenUpdate{SealedPayload: []byte("sealed-from-b")}.WithClaim("owner-b")); !errors.Is(err, ErrRefreshClaimLost) {
		t.Fatalf("commit by a non-owner = %v, want ErrRefreshClaimLost", err)
	}
	if got, _ := s.Get(ctx, "alice", OAuthKindCodex); string(got.SealedPayload) != "sealed-v1" {
		t.Fatalf("payload = %q: a refused commit must write nothing", got.SealedPayload)
	}

	// A RE-CONNECT supersedes the claim: Upsert writes the claim keys as
	// explicit empty/null, so the holder mid-exchange loses its fence and
	// its tokens are discarded instead of overwriting what was just
	// uploaded. With bson omitempty on those fields this is the assertion
	// that would fail — the $set would carry no key and the stale claim
	// would survive the operator's upload.
	if err := s.Upsert(ctx, OAuthRecord{
		UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed-reconnected"), Fingerprint: "fp-2",
	}); err != nil {
		t.Fatalf("re-connect upsert: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshClaimOwner != "" || got.RefreshNotBefore != nil {
		t.Fatalf("re-connect left claim owner=%q not_before=%v, want both cleared on the wire",
			got.RefreshClaimOwner, got.RefreshNotBefore)
	}
	if err := s.UpdateTokens(ctx, "alice", OAuthKindCodex,
		OAuthTokenUpdate{SealedPayload: []byte("sealed-from-a")}.WithClaim("owner-a")); !errors.Is(err, ErrRefreshClaimLost) {
		t.Fatalf("commit after a re-connect = %v, want ErrRefreshClaimLost", err)
	}
	if got, _ := s.Get(ctx, "alice", OAuthKindCodex); string(got.SealedPayload) != "sealed-reconnected" {
		t.Fatalf("payload = %q, want the credential the operator just connected", got.SealedPayload)
	}

	// A holder that dies costs one sweep: the lease expiring is what makes
	// the record claimable again, so nothing has to reap it.
	ok, err = s.ClaimRefresh(ctx, "alice", OAuthKindCodex, "owner-c", now, now.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("claim after the re-connect cleared it: ok=%v err=%v", ok, err)
	}
	later := now.Add(2 * time.Minute)
	ok, err = s.ClaimRefresh(ctx, "alice", OAuthKindCodex, "owner-d", later, later.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("claim once the lease expired: ok=%v err=%v, want acquired", ok, err)
	}
	// The holder's own commit lands AND releases the claim in one write.
	if err := s.UpdateTokens(ctx, "alice", OAuthKindCodex,
		OAuthTokenUpdate{SealedPayload: []byte("sealed-from-d"), Fingerprint: "fp-2"}.WithClaim("owner-d")); err != nil {
		t.Fatalf("commit by the holder: %v", err)
	}
	got, err = s.Get(ctx, "alice", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.SealedPayload) != "sealed-from-d" {
		t.Fatalf("payload = %q, want the holder's write", got.SealedPayload)
	}
	if got.RefreshClaimOwner != "" || got.RefreshNotBefore != nil {
		t.Fatalf("commit left the claim held: owner=%q not_before=%v", got.RefreshClaimOwner, got.RefreshNotBefore)
	}
}

// A cool-down is scheduling state, so it must survive on the wire exactly
// as written — and an UNFENCED write (the self-heal partial update, which
// takes no claim) must not touch either claim key.
func TestMongoOAuth_CoolDownSurvivesAndUnfencedWritesLeaveItAlone(t *testing.T) {
	s, ctx := mongoOAuthStore(t)
	if err := s.Upsert(ctx, OAuthRecord{
		UserID: "alice", Kind: OAuthKindCodex, SealedPayload: []byte("sealed-v1"),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now().UTC()
	if ok, err := s.ClaimRefresh(ctx, "alice", OAuthKindCodex, "owner-a", now, now.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	cool := now.Add(time.Hour).Truncate(time.Millisecond)
	if err := s.UpdateTokens(ctx, "alice", OAuthKindCodex, OAuthTokenUpdate{
		SealedPayload: []byte("sealed-v2"), RefreshNotBefore: &cool,
	}.WithClaim("owner-a")); err != nil {
		t.Fatalf("commit with a cool-down: %v", err)
	}
	got, err := s.Get(ctx, "alice", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshNotBefore == nil || !got.RefreshNotBefore.Equal(cool) {
		t.Fatalf("cool-down = %v, want %v — the sweep re-runs the exchange every tick without it", got.RefreshNotBefore, cool)
	}
	if got.RefreshClaimOwner != "" {
		t.Fatalf("claim owner = %q, want released beside the cool-down", got.RefreshClaimOwner)
	}
	// The cool-down holds the CAS off until it passes.
	if ok, err := s.ClaimRefresh(ctx, "alice", OAuthKindCodex, "owner-b", now, now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("claim during the cool-down: ok=%v err=%v, want refused", ok, err)
	}
	// The self-heal writes no claim, so it must leave the cool-down as it is.
	if err := s.UpdateTokens(ctx, "alice", OAuthKindCodex, OAuthTokenUpdate{NotRefreshable: true}); err != nil {
		t.Fatalf("unfenced self-heal: %v", err)
	}
	got, err = s.Get(ctx, "alice", OAuthKindCodex)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshNotBefore == nil || !got.RefreshNotBefore.Equal(cool) {
		t.Fatalf("cool-down after an unfenced write = %v, want it untouched", got.RefreshNotBefore)
	}
}
