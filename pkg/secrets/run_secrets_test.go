package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

func testRing(t *testing.T, ids ...string) map[string][]byte {
	t.Helper()
	ring := map[string][]byte{}
	for _, id := range ids {
		key := make([]byte, 32)
		for i := range key {
			key[i] = id[0] // deterministic per-id material
		}
		ring[id] = key
	}
	return ring
}

func sealBundle(t *testing.T, s Sealer, tenant, pool, run string, b RunBundle) ([]byte, string) {
	t.Helper()
	sealed, keyID, err := SealRunBundle(s, tenant, pool, run, b)
	if err != nil {
		t.Fatalf("SealRunBundle: %v", err)
	}
	return sealed, keyID
}

// The AAD binds a bundle to its full identity: sealed for one
// (tenant, pool, run), it refuses to open under any other identity's
// context — even with the right key. The binding is what makes a
// confused record store serve ciphertext that cannot decrypt, instead
// of another tenant's credentials.
func TestRunBundleAADBindsIdentity(t *testing.T) {
	dek, err := NewRunBundleDEK()
	if err != nil {
		t.Fatal(err)
	}
	dekSealer, err := NewAESGCMSealer(dek)
	if err != nil {
		t.Fatal(err)
	}
	b := RunBundle{APIKeys: map[Provider]string{"anthropic": "sk-test"}}
	sealed, keyID := sealBundle(t, dekSealer, "tenant-a", "honorabilite", "run-1", b)
	if keyID != DEKKeyID {
		t.Fatalf("sealed under %q, want the dek scheme", keyID)
	}

	for _, tc := range []struct{ tenant, pool, run string }{
		{"tenant-b", "honorabilite", "run-1"},
		{"tenant-a", "other-pool", "run-1"},
		{"tenant-a", "honorabilite", "run-2"},
	} {
		if _, err := OpenRunBundle(dekSealer, tc.tenant, tc.pool, tc.run, keyID, sealed); err == nil {
			t.Fatalf("bundle opened under foreign identity (%s/%s/%s) — the AAD binding is off", tc.tenant, tc.pool, tc.run)
		}
	}
	got, err := OpenRunBundle(dekSealer, "tenant-a", "honorabilite", "run-1", keyID, sealed)
	if err != nil {
		t.Fatalf("own identity refused: %v", err)
	}
	if got.APIKeys["anthropic"] != "sk-test" {
		t.Fatalf("round-trip lost the key: %v", got.APIKeys)
	}
}

// The P4a ring-id cohort (records stamped with a ring key id) stays
// openable through the transition ring: by recorded id while the key
// remains, with the key named once it leaves. Nothing produces these
// anymore — the publisher seals per-run DEKs — but the 24h bundle TTL
// is what retires the cohort, not this change.
func TestRunBundleRingIDTransition(t *testing.T) {
	b := RunBundle{GenericSecrets: map[string]string{"tok": "v1"}}
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}

	ring, err := NewKeyRingSealer(testRing(t, "a", "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	sealedA, err := ring.SealWith("a", body, RunBundleAAD("t", "p", "r"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRunBundle(ring, "t", "p", "r", "a", sealedA); err != nil {
		t.Fatalf("a ring-id bundle must open by recorded id while the key is in the ring: %v", err)
	}

	retired, err := NewKeyRingSealer(testRing(t, "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenRunBundle(retired, "t", "p", "r", "a", sealedA)
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("retired-key open: err = %v, want the key id named", err)
	}
}

// A record that predates key ids (the deploy window): run-only AAD,
// sealed under the then-single key, opens through the ring even after
// the ring gained keys — the id-less cohort resolves against whichever
// ring key authenticates.
func TestRunBundleLegacyCompat(t *testing.T) {
	legacyKey := testRing(t, "old")["old"]
	legacy, err := NewAESGCMSealer(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	b := RunBundle{GenericSecrets: map[string]string{"tok": "v1"}}
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := legacy.Seal(body, legacyRunBundleAAD("run-old"))
	if err != nil {
		t.Fatal(err)
	}

	ring, err := NewKeyRingSealer(testRing(t, "old", "new"), "new")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRunBundle(ring, "t", "p", "run-old", "", sealed); err != nil {
		t.Fatalf("legacy record refused after rotation: %v", err)
	}
}

// The DEK scheme's contract: the seal stamps the scheme constant, and
// a bundle opens only under its own DEK — a different per-run key,
// even with the right identity, refuses.
func TestRunBundleDEKScheme(t *testing.T) {
	dek, err := NewRunBundleDEK()
	if err != nil {
		t.Fatal(err)
	}
	dekSealer, err := NewAESGCMSealer(dek)
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID := sealBundle(t, dekSealer, "t", "p", "r", RunBundle{GenericSecrets: map[string]string{"k": "v"}})
	if _, err := OpenRunBundle(dekSealer, "t", "p", "r", keyID, sealed); err != nil {
		t.Fatalf("own dek refused: %v", err)
	}

	other, err := NewRunBundleDEK()
	if err != nil {
		t.Fatal(err)
	}
	otherSealer, err := NewAESGCMSealer(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRunBundle(otherSealer, "t", "p", "r", keyID, sealed); err == nil {
		t.Fatal("a bundle opened under a foreign per-run key — the DEK binding is off")
	}
}

func TestKeyRingFromConfig(t *testing.T) {
	b64 := func(id byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{id}, 32)) }

	// Bare single key: a one-entry ring under "default".
	s, err := NewKeyRingFromConfig(b64(1), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.CurrentKeyID() != "default" {
		t.Fatalf("single-key ring current %q, want default", s.CurrentKeyID())
	}

	// Ring spec: current must name an entry; ids must not repeat.
	if _, err := NewKeyRingFromConfig("", "a="+b64(1)+",b="+b64(2), "missing"); err == nil {
		t.Fatal("current id outside the ring accepted")
	}
	if _, err := NewKeyRingFromConfig("", "a="+b64(1)+",a="+b64(2), "a"); err == nil {
		t.Fatal("duplicate ring ids accepted")
	}
	if _, err := NewKeyRingFromConfig("", "novalue", "x"); err == nil {
		t.Fatal("entry without id=base64 shape accepted")
	}
	if _, err := NewKeyRingFromConfig("", "a="+b64(1)+",b="+b64(2), ""); err == nil {
		t.Fatal("ring without a current id accepted")
	}

	dual, err := NewKeyRingFromConfig("", "a="+b64(1)+",b="+b64(2), "b")
	if err != nil {
		t.Fatal(err)
	}
	if dual.CurrentKeyID() != "b" {
		t.Fatalf("dual ring current %q, want b", dual.CurrentKeyID())
	}
	if _, err := NewKeyRingFromConfig("", "", ""); err == nil {
		t.Fatal("no key material accepted")
	}
}
func TestMemoryRunSecretsStore_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryRunSecretsStore()
	rec := RunSecretsRecord{
		ID:           "ref-1",
		TenantID:     "t1",
		RunID:        "run-1",
		SealedBundle: []byte("sealed"),
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	if err := st.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Same tenant → returns the record.
	got, err := st.Get(store.WithTenant(ctx, "t1"), "ref-1")
	if err != nil || got.RunID != "run-1" {
		t.Fatalf("Get under owning tenant: got %+v, err %v", got, err)
	}

	// Different tenant → must look like a missing record (no cross-tenant read).
	if _, err := st.Get(store.WithTenant(ctx, "t2"), "ref-1"); !errors.Is(err, ErrRunSecretsNotFound) {
		t.Fatalf("Get under foreign tenant = %v, want ErrRunSecretsNotFound", err)
	}

	// No tenant in ctx → privileged runner-pickup path returns the record.
	if _, err := st.Get(ctx, "ref-1"); err != nil {
		t.Fatalf("Get under bare ctx (privileged): %v", err)
	}

	// Unknown id → not found.
	if _, err := st.Get(store.WithTenant(ctx, "t1"), "nope"); !errors.Is(err, ErrRunSecretsNotFound) {
		t.Fatalf("Get unknown id = %v, want ErrRunSecretsNotFound", err)
	}
}

func TestMemoryRunSecretsStore_DeleteTenantScoped(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryRunSecretsStore()
	rec := RunSecretsRecord{ID: "ref-1", TenantID: "t1", RunID: "run-1"}
	if err := st.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Foreign-tenant delete is a silent no-op: returns nil AND leaves the
	// record intact (must not reveal existence, must not delete).
	if err := st.Delete(store.WithTenant(ctx, "t2"), "ref-1"); err != nil {
		t.Fatalf("Delete under foreign tenant returned err %v, want nil no-op", err)
	}
	if _, err := st.Get(store.WithTenant(ctx, "t1"), "ref-1"); err != nil {
		t.Fatalf("record was deleted by a foreign-tenant Delete: %v", err)
	}

	// Owning-tenant delete actually removes it.
	if err := st.Delete(store.WithTenant(ctx, "t1"), "ref-1"); err != nil {
		t.Fatalf("Delete under owning tenant: %v", err)
	}
	if _, err := st.Get(store.WithTenant(ctx, "t1"), "ref-1"); !errors.Is(err, ErrRunSecretsNotFound) {
		t.Fatalf("record still present after owning-tenant Delete: %v", err)
	}
}

// The local pool-grammar copy is pinned to the queue package's — one
// grammar, two homes, the test is the guard against drift.
func TestValidPoolNameMatchesQueueGrammar(t *testing.T) {
	for _, s := range []string{"", "a", "-lead", "trail-", "a-", "UPPER", "with_underscore", strings.Repeat("a", 32), strings.Repeat("a", 31), "pool-1", "po-ol"} {
		if got, want := validPoolName(s), queue.ValidPoolName(s); got != want {
			t.Fatalf("validPoolName(%q) = %v, queue.ValidPoolName = %v — the copies drifted", s, got, want)
		}
	}
}

// At-rest records (api_keys, oauth, generic) carry no key id: rotation
// moves the ring's current key out from under them, and their opens
// must fall back across the ring while the old key remains — the
// reviewer's finding: a "non-destructive" rotation that bricks every
// stored credential is neither.
func TestKeyRingOpenFallsBackAfterRotation(t *testing.T) {
	atRest := []byte("sealed-vendor-credential")
	aad := []byte("api_key:k1")

	pre, err := NewKeyRingSealer(testRing(t, "a"), "a")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := pre.Seal(atRest, aad)
	if err != nil {
		t.Fatal(err)
	}

	rotated, err := NewKeyRingSealer(testRing(t, "a", "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Open(sealed, aad)
	if err != nil {
		t.Fatalf("at-rest record refused after rotation: %v", err)
	}
	if !bytes.Equal(got, atRest) {
		t.Fatal("round-trip lost the payload")
	}

	retired, err := NewKeyRingSealer(testRing(t, "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retired.Open(sealed, aad); err == nil {
		t.Fatal("a record whose key left the ring must refuse")
	}
}
