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

	"github.com/SocialGouv/iterion/pkg/store"
)

func testRing(t *testing.T, ids ...string) map[string][]byte {
	t.Helper()
	ring := map[string][]byte{}
	for _, id := range ids {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(id[0]) // deterministic per-id material
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
	ring, err := NewKeyRingSealer(testRing(t, "k1"), "k1")
	if err != nil {
		t.Fatal(err)
	}
	b := RunBundle{APIKeys: map[Provider]string{"anthropic": "sk-test"}}
	sealed, keyID := sealBundle(t, ring, "tenant-a", "honorabilite", "run-1", b)
	if keyID != "k1" {
		t.Fatalf("sealed under %q, want k1", keyID)
	}

	for _, tc := range []struct{ tenant, pool, run string }{
		{"tenant-b", "honorabilite", "run-1"},
		{"tenant-a", "other-pool", "run-1"},
		{"tenant-a", "honorabilite", "run-2"},
	} {
		if _, err := OpenRunBundle(ring, tc.tenant, tc.pool, tc.run, keyID, sealed); err == nil {
			t.Fatalf("bundle opened under foreign identity (%s/%s/%s) — the AAD binding is off", tc.tenant, tc.pool, tc.run)
		}
	}
	got, err := OpenRunBundle(ring, "tenant-a", "honorabilite", "run-1", keyID, sealed)
	if err != nil {
		t.Fatalf("own identity refused: %v", err)
	}
	if got.APIKeys["anthropic"] != "sk-test" {
		t.Fatalf("round-trip lost the key: %v", got.APIKeys)
	}
}

// Rotation: seals move to the new current key, bundles sealed under a
// retired key stay openable through their recorded key id, and a key
// gone from the ring is named by the error.
func TestRunBundleRotation(t *testing.T) {
	b := RunBundle{GenericSecrets: map[string]string{"tok": "v1"}}

	old, err := NewKeyRingSealer(testRing(t, "a"), "a")
	if err != nil {
		t.Fatal(err)
	}
	sealedA, keyIDA := sealBundle(t, old, "t", "p", "r", b)
	if keyIDA != "a" {
		t.Fatalf("pre-rotation key id %q, want a", keyIDA)
	}

	rotated, err := NewKeyRingSealer(testRing(t, "a", "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	sealedB, keyIDB := sealBundle(t, rotated, "t", "p", "r", b)
	if keyIDB != "b" {
		t.Fatalf("post-rotation key id %q, want b", keyIDB)
	}
	if _, err := OpenRunBundle(rotated, "t", "p", "r", keyIDA, sealedA); err != nil {
		t.Fatalf("a bundle sealed under the retired key must stay openable while the key is in the ring: %v", err)
	}
	if _, err := OpenRunBundle(rotated, "t", "p", "r", keyIDB, sealedB); err != nil {
		t.Fatalf("current-key bundle refused: %v", err)
	}

	// The operator retires "a": its bundles refuse, naming the key.
	retired, err := NewKeyRingSealer(testRing(t, "b"), "b")
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenRunBundle(retired, "t", "p", "r", keyIDA, sealedA)
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

// A bare Sealer is typed-refused at both ends: the key id is the
// contract, and a sealer that cannot name a key cannot honor it.
func TestRunBundleNeedsKeyedSealer(t *testing.T) {
	plain, err := NewAESGCMSealer(testRing(t, "x")["x"])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := SealRunBundle(plain, "t", "p", "r", RunBundle{}); err == nil || !strings.Contains(err.Error(), "keyed sealer") {
		t.Fatalf("plain sealer seal: err = %v, want the keyed-sealer refusal", err)
	}
	ring, err := NewKeyRingSealer(testRing(t, "x"), "x")
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID := sealBundle(t, ring, "t", "p", "r", RunBundle{GenericSecrets: map[string]string{"k": "v"}})
	if _, err := OpenRunBundle(plain, "t", "p", "r", keyID, sealed); err == nil || !strings.Contains(err.Error(), "keyed sealer") {
		t.Fatalf("plain sealer open: err = %v, want the keyed-sealer refusal", err)
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
