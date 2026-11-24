package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// C252 is owed by what the MANIFEST declares as well as by what the sources
// are written in: a bundle whose manifest carries `category`/`tags` with a
// floor below the release that reads them is told so here, with the
// consequence that floor actually has — a build below it decodes the
// manifest strictly and refuses the whole document, which is not the
// generic parse failure of a source syntax.
//
// Reddens on the mutation that drops the manifest half from the floors
// table's subject (FloorSubject.Keys), or on a consequence read from an arm
// of this package instead of from the entry that decided the floor.
func TestC252IsAskedForByAManifestKeyAndNotOnlyByTheSources(t *testing.T) {
	find := func(diags []Diag) *Diag {
		for i := range diags {
			if diags[i].Code == DiagProfileNeedsFloor {
				return &diags[i]
			}
		}
		return nil
	}
	decode := func(t *testing.T, floor string) *bundle.Manifest {
		t.Helper()
		body := "name: taxonomy-probe\ncategory: verify\ntags: [read-only]\n"
		if floor != "" {
			body += "requires:\n  iterion: \"" + floor + "\"\n"
		}
		m, err := bundle.DecodeManifest([]byte(body), "manifest.yaml")
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	build := "v" + bundle.CatalogTaxonomySince
	below := floortest.Below(t, bundle.CatalogTaxonomySince)

	// No floor at all: the sources ask for nothing, the keys do.
	d := find(CheckConsistency(Input{Manifest: decode(t, ""), Syntax: bundle.SyntaxRequirements{Profile: 1}, EngineBuild: build}))
	if d == nil {
		t.Fatal("a manifest declaring category/tags with no floor draws no C252")
	}
	if !strings.Contains(d.Message, "`category`/`tags`") {
		t.Errorf("C252 does not name the keys that ask for the floor: %q", d.Message)
	}
	if !strings.Contains(d.Message, "refuses the whole document") || strings.Contains(d.Message, "fails at that parse") {
		t.Errorf("C252 does not name what a build below this floor does with the manifest: %q", d.Message)
	}
	if !strings.Contains(d.Hint, `">= `+bundle.CatalogTaxonomySince+`"`) {
		t.Errorf("C252 does not name the release: %q", d.Hint)
	}

	// Declared, but below the release that reads the keys.
	if d = find(CheckConsistency(Input{Manifest: decode(t, ">= "+below), Syntax: bundle.SyntaxRequirements{Profile: 1}, EngineBuild: build})); d == nil || !strings.Contains(d.Message, bundle.CatalogTaxonomySince) {
		t.Fatalf("a floor of %s passed for the taxonomy keys: %v", below, d)
	}

	// Declared at the release: silent.
	if d = find(CheckConsistency(Input{Manifest: decode(t, ">= "+bundle.CatalogTaxonomySince), Syntax: bundle.SyntaxRequirements{Profile: 1}, EngineBuild: build})); d != nil {
		t.Errorf("C252 drawn with the taxonomy release declared: %+v", *d)
	}

	// A manifest that declares neither key owes nothing.
	plain, err := bundle.DecodeManifest([]byte("name: plain\n"), "manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if d = find(CheckConsistency(Input{Manifest: plain, Syntax: bundle.SyntaxRequirements{Profile: 1}, EngineBuild: build})); d != nil {
		t.Errorf("C252 drawn on a manifest that declares no floor-bearing key: %+v", *d)
	}
}
