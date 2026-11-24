package connection

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/connectors"
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

// BenchmarkStoreTierResolution measures one resolution served by a
// store-backed tier over a fresh composition — the shipped Forgejo package
// published to the platform tier: a document read and a hash over its files,
// the parse reused while the bytes hold.
func BenchmarkStoreTierResolution(b *testing.B) {
	files := map[string]string{}
	err := fs.WalkDir(connectors.FS(), "forgejo", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(connectors.FS(), p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, "forgejo/")] = string(body)
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	store := NewMemoryPackageStore()
	if err := store.Publish(context.Background(), StoredPackage{TenantID: PlatformTenantID, ConnectorID: "forgejo", Files: files}); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		cat := NewLayeredCatalog(CloudCatalogs(store, "t1")...)
		if _, tier, err := cat.Package("forgejo"); err != nil || tier.Kind != TierPlatform {
			b.Fatalf("tier=%v err=%v", tier.Kind, err)
		}
	}
}

// unreadablePackages is a package store that cannot be read.
type unreadablePackages struct{ *MemoryPackageStore }

func (unreadablePackages) Get(context.Context, string, string) (StoredPackage, error) {
	return StoredPackage{}, errors.New("server selection error: context deadline exceeded")
}

// A store that cannot be read is an outage, typed apart from the two
// verdicts about the catalog's content: an absent connector, and a package
// held that does not load.
func TestAnUnreadableStoreTierIsAnOutageNotAVerdict(t *testing.T) {
	_, _, err := TeamCatalog(unreadablePackages{NewMemoryPackageStore()}, "t1").Package("forge")
	if !IsCatalogUnavailable(err) || IsNoSuchConnector(err) {
		t.Fatalf("an unreadable store = %v, want a CatalogUnavailableError", err)
	}
	_, _, err = TeamCatalog(NewMemoryPackageStore(), "t1").Package("forge")
	if IsCatalogUnavailable(err) || !IsNoSuchConnector(err) {
		t.Fatalf("an absent connector = %v, want no-such-connector", err)
	}
}

// The cloud composition — every tier layered — still says "no such
// connector" when no tier holds it, and keeps each tier's reason in its text.
func TestTheLayeredCatalogKeepsTheNoSuchConnectorVerdict(t *testing.T) {
	_, _, err := NewLayeredCatalog(CloudCatalogs(NewMemoryPackageStore(), "t1")...).Package("nosuchvendor")
	if !IsNoSuchConnector(err) || IsCatalogUnavailable(err) {
		t.Fatalf("a connector no tier holds = %v, want the no-such-connector verdict", err)
	}
	if !strings.Contains(err.Error(), "team") {
		t.Fatalf("the verdict lost the tiers' reasons: %v", err)
	}
	_, _, err = NewLayeredCatalog(CloudCatalogs(unreadablePackages{NewMemoryPackageStore()}, "t1")...).Package("nosuchvendor")
	if !IsCatalogUnavailable(err) {
		t.Fatalf("an unreadable team tier through the composition = %v, want an outage", err)
	}
}
