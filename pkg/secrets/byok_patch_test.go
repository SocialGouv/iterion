package secrets

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A patch writes exactly the fields it names — zero values included, so a
// cleared audience and an uncapped key persist — and nothing else: the
// fields it does not name keep what the store holds, not what the
// caller's possibly stale copy holds.
func TestMemoryApiKey_PatchWritesOnlyTheNamedFields(t *testing.T) {
	st := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	ctx := store.WithTenant(t.Context(), "t")
	k := mkKey(t, st, sealer, "t", "", ProviderAnthropic, "prod", "sk-ant-original", true)
	three := 3
	if _, err := st.Patch(ctx, k.ID, ApiKeyPatch{MaxConcurrentRuns: &three, Bots: &[]string{"review-pr"}}); err != nil {
		t.Fatal(err)
	}

	name := "renamed"
	got, err := st.Patch(ctx, k.ID, ApiKeyPatch{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || !got.IsDefault || got.MaxConcurrentRuns != 3 || len(got.Bots) != 1 || !bytes.Equal(got.SealedSecret, k.SealedSecret) || got.Fingerprint != k.Fingerprint {
		t.Fatalf("a name-only patch moved another field: %+v", got)
	}

	off, zero := false, 0
	got, err = st.Patch(ctx, k.ID, ApiKeyPatch{IsDefault: &off, MaxConcurrentRuns: &zero, Bots: &[]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if got.IsDefault || got.MaxConcurrentRuns != 0 || len(got.Bots) != 0 || got.Name != "renamed" {
		t.Fatalf("zero values must persist as writes, not vanish as omissions: %+v", got)
	}

	rotated := ApiKeySecretPatch{Sealed: []byte("sealed-2"), Last4: "ted2", Fingerprint: "fp-2"}
	got, err = st.Patch(ctx, k.ID, ApiKeyPatch{Secret: &rotated})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.SealedSecret) != "sealed-2" || got.Last4 != "ted2" || got.Fingerprint != "fp-2" {
		t.Fatalf("the rotation triplet must land together: %+v", got)
	}

	if same, err := st.Patch(ctx, k.ID, ApiKeyPatch{}); err != nil || same.Fingerprint != "fp-2" || same.Name != "renamed" {
		t.Fatalf("an empty patch is a read: %+v %v", same, err)
	}
}

// The failure a whole-document replace produced: the handler reads the
// row, a concurrent ClearDefault lowers is_default, and the handler's
// write-back resurrects it. A patch that does not name is_default cannot.
func TestMemoryApiKey_PatchDoesNotRevertAConcurrentClearDefault(t *testing.T) {
	st := NewMemoryApiKeyStore()
	sealer := newSealer(t)
	ctx := store.WithTenant(t.Context(), "t")
	old := mkKey(t, st, sealer, "t", "", ProviderAnthropic, "old", "sk-ant-old", true)
	fresh := mkKey(t, st, sealer, "t", "", ProviderAnthropic, "fresh", "sk-ant-fresh", true)
	// The handler's stale read happens here (old is still the default)…
	stale, _ := st.Get(ctx, old.ID)
	if !stale.IsDefault {
		t.Fatal("fixture: old must start as the default")
	}
	// …then the promotion of the other key clears it…
	if err := st.ClearDefault(ctx, "t", "", ProviderAnthropic, fresh.ID); err != nil {
		t.Fatal(err)
	}
	// …and the handler's rename lands on the stale read.
	name := "old-renamed"
	got, err := st.Patch(ctx, old.ID, ApiKeyPatch{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if got.IsDefault {
		t.Fatal("a rename resurrected a default that ClearDefault had lowered")
	}
}

// Tenancy holds on the patch path exactly as on Get: a row written under
// one tenant is not reachable, let alone writable, under another.
func TestMemoryApiKey_PatchIsTenantScoped(t *testing.T) {
	st := NewMemoryApiKeyStore()
	// Created under team-a's context so the row carries the tenant the
	// Mongo twin filters on (mkKey writes tenant-less rows on purpose).
	k := ApiKey{ID: NewApiKeyID(), ScopeTeamID: "team-a", Provider: ProviderAnthropic, Name: "k", SealedSecret: []byte("sealed")}
	if err := st.Create(store.WithTenant(t.Context(), "team-a"), k); err != nil {
		t.Fatal(err)
	}
	name := "stolen"
	if _, err := st.Patch(store.WithTenant(t.Context(), "team-b"), k.ID, ApiKeyPatch{Name: &name}); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("patch from another tenant = %v, want ErrApiKeyNotFound", err)
	}
	if back, _ := st.Get(store.WithTenant(t.Context(), "team-a"), k.ID); back.Name != "k" {
		t.Fatalf("the foreign patch landed: %+v", back)
	}
}

// The Mongo twin's Patch refuses a context that carries no tenant; the
// memory twin must refuse the same, or a handler that drops the scoped context
// passes every memory test and fails in production. Rows with no tenant
// stay writable from a bare context — the bare-context fixtures need it.
func TestMemoryApiKey_PatchRefusesABareContextOnAStampedRow(t *testing.T) {
	st := NewMemoryApiKeyStore()
	stamped := ApiKey{ID: NewApiKeyID(), ScopeTeamID: "team-a", Provider: ProviderAnthropic, Name: "k", SealedSecret: []byte("sealed")}
	if err := st.Create(store.WithTenant(t.Context(), "team-a"), stamped); err != nil {
		t.Fatal(err)
	}
	name := "renamed"
	if _, err := st.Patch(t.Context(), stamped.ID, ApiKeyPatch{Name: &name}); !errors.Is(err, ErrApiKeyTenantMissing) {
		t.Fatalf("bare-context patch of a stamped row = %v, want ErrApiKeyTenantMissing", err)
	}
	if back, _ := st.Get(store.WithTenant(t.Context(), "team-a"), stamped.ID); back.Name != "k" {
		t.Fatalf("the bare-context patch landed: %+v", back)
	}
	bare := mkKey(t, st, newSealer(t), "team-a", "", ProviderAnthropic, "fixture", "sk-ant-fixture", false)
	if _, err := st.Patch(t.Context(), bare.ID, ApiKeyPatch{Name: &name}); err != nil {
		t.Fatalf("a tenant-less fixture row must stay writable from a bare context: %v", err)
	}
}

// A rotation is whole or refused, on both twins, before anything is written.
func TestMemoryApiKey_PatchRefusesAnIncompleteRotation(t *testing.T) {
	st := NewMemoryApiKeyStore()
	ctx := store.WithTenant(t.Context(), "t")
	k := mkKey(t, st, newSealer(t), "t", "", ProviderAnthropic, "k", "sk-ant-k", false)
	for _, p := range []ApiKeySecretPatch{{Sealed: []byte("s")}, {Last4: "x", Fingerprint: "y"}, {Sealed: []byte("s"), Last4: "x"}, {Sealed: []byte("s"), Fingerprint: "y"}} {
		if _, err := st.Patch(ctx, k.ID, ApiKeyPatch{Secret: &p}); !errors.Is(err, ErrApiKeyPatchInvalid) {
			t.Fatalf("incomplete rotation %+v = %v, want ErrApiKeyPatchInvalid", p, err)
		}
	}
	if back, _ := st.Get(ctx, k.ID); !bytes.Equal(back.SealedSecret, k.SealedSecret) || back.Fingerprint != k.Fingerprint {
		t.Fatalf("a refused rotation landed: %+v", back)
	}
}

// Every field of ApiKeyPatch must reach BOTH writers: a field setDoc does
// not write makes Mongo answer "patched" while writing nothing (IsEmpty
// reads setDoc, so the FindOne branch runs); a field applyTo does not write
// makes the memory twin lie in every handler test. The sample switch fails
// loudly on a type it does not know, so a new field cannot slip past.
func TestApiKeyPatch_EveryFieldReachesBothWriters(t *testing.T) {
	rt := reflect.TypeOf(ApiKeyPatch{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		var p ApiKeyPatch
		v := reflect.ValueOf(&p).Elem().Field(i)
		switch f.Type {
		case reflect.TypeOf((*string)(nil)):
			s := "sample"
			v.Set(reflect.ValueOf(&s))
		case reflect.TypeOf((*bool)(nil)):
			b := true
			v.Set(reflect.ValueOf(&b))
		case reflect.TypeOf((*int)(nil)):
			n := 7
			v.Set(reflect.ValueOf(&n))
		case reflect.TypeOf((*[]string)(nil)):
			l := []string{"sample"}
			v.Set(reflect.ValueOf(&l))
		case reflect.TypeOf((*ApiKeySecretPatch)(nil)):
			v.Set(reflect.ValueOf(&ApiKeySecretPatch{Sealed: []byte("s"), Last4: "l4", Fingerprint: "fp"}))
		default:
			t.Fatalf("ApiKeyPatch.%s: no sample for %s — add one here", f.Name, f.Type)
		}
		if p.IsEmpty() {
			t.Errorf("ApiKeyPatch.%s: setDoc does not write it", f.Name)
		}
		var k ApiKey
		p.applyTo(&k)
		if reflect.DeepEqual(k, ApiKey{}) {
			t.Errorf("ApiKeyPatch.%s: applyTo does not write it", f.Name)
		}
	}
}
