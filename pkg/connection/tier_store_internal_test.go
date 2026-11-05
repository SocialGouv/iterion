package connection

import (
	"testing"
)

// The baked floor is ONE instance per process (#2757 round-1 review): the
// FSCatalog's load cache is per instance, so a composition that rebuilt the
// floor per call re-parsed its ~790 KiB of YAML per resolution. The witness
// reads the composition's own tiers — not the shared var — so a composition
// that stopped USING it reddens this test.
func TestCloudCatalogsShareTheOneBakedFloor(t *testing.T) {
	a := CloudCatalogs(NewMemoryPackageStore(), "t1")
	b := CloudCatalogs(NewMemoryPackageStore(), "t2")
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("the composition is three tiers: %d, %d", len(a), len(b))
	}
	floorA, ok := a[2].(*FSCatalog)
	if !ok {
		t.Fatalf("the third tier is the baked floor, got %T", a[2])
	}
	if b[2] != a[2] {
		t.Fatal("two compositions must share the ONE baked floor instance")
	}
	// And the sharing is what the cache is for: a package loaded through one
	// composition is warm in the other.
	if _, _, err := a[2].Package("forgejo"); err != nil {
		t.Fatal(err)
	}
	floorA.mu.RLock()
	warm := len(floorA.loaded)
	floorA.mu.RUnlock()
	if warm == 0 {
		t.Fatal("the load did not land in the shared floor's cache")
	}
}

// checkPackagePath refuses the backslash: a separator elsewhere, an ordinary
// byte here — durable content must not carry the ambiguity.
func TestCheckPackagePathRefusesTheBackslash(t *testing.T) {
	for _, p := range []string{`a\..\b`, `a\b`, `ops\issue.yaml`} {
		if err := checkPackagePath(p); err == nil {
			t.Errorf("path %q must be refused", p)
		}
	}
	for _, ok := range []string{"connector.yaml", "ops/issue.yaml", "identity.lock.yaml"} {
		if err := checkPackagePath(ok); err != nil {
			t.Errorf("path %q must pass: %v", ok, err)
		}
	}
}

// BenchmarkBakedFloorResolution measures one facade-resolution's cost over a
// fresh composition: with the shared floor it is a map lookup after the first
// call; with a per-composition floor (the round-1 defect) it re-parsed the
// shipped packages every time.
func BenchmarkBakedFloorResolution(b *testing.B) {
	store := NewMemoryPackageStore()
	for b.Loop() {
		cat := NewLayeredCatalog(CloudCatalogs(store, "t1")...)
		if _, _, err := cat.Package("forgejo"); err != nil {
			b.Fatal(err)
		}
	}
}
