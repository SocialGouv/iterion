package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestCatalogBotsDeclareKnownCategory is the anti-absence gate for the
// navigation spine: every shipped bot manifest declares a category from
// the closed six-slug set, and every declared tag is in the curated seed.
// The bundlelint consistency gate next door already reddens UNKNOWN
// values (C270/C271 surface as warnings there); this test closes the
// ABSENT-category hole — a shipped bot silently drifting to Uncategorized
// is a catalog regression, not a legitimate state.
func TestCatalogBotsDeclareKnownCategory(t *testing.T) {
	manifests, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("no manifests found under bots/*/ — the discovery glob broke")
	}
	for _, path := range manifests {
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Errorf("%s: load: %v", path, err)
			continue
		}
		if m == nil {
			continue
		}
		if _, ok := bundle.BotCategoryBySlug(m.Category); !ok {
			t.Errorf("%s: category %q is not in the closed set (%s) — every shipped bot declares one",
				path, m.Category, bundle.KnownBotCategorySlugs())
		}
		for _, tag := range m.Tags {
			if !bundle.IsKnownBotTag(tag) {
				t.Errorf("%s: tag %q is outside the curated seed — seed it in pkg/bundle/vocab.go if intentional",
					path, tag)
			}
		}
	}
}

// TestBotTaxonomyVocabularyParity holds the studio's TS mirror of the
// navigation vocabulary to the Go source of truth (pkg/bundle/vocab.go).
// The mirror drives display order on every studio surface; two
// hand-synced copies without a guard drift in silence. Only the
// CATEGORIES are mirrored — the tag seed's consumers are Go-only, so a
// TS copy would be a third source with no reader.
func TestBotTaxonomyVocabularyParity(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "studio", "src", "lib", "botTaxonomy.ts"))
	if err != nil {
		t.Fatalf("read studio mirror: %v", err)
	}
	src := string(body)

	slugs := regexp.MustCompile(`slug: "([a-z]+)"`).FindAllStringSubmatch(src, -1)
	if len(slugs) != len(bundle.BotCategories) {
		t.Fatalf("studio mirror declares %d categories, Go declares %d", len(slugs), len(bundle.BotCategories))
	}
	for i, m := range slugs {
		if m[1] != bundle.BotCategories[i].Slug {
			t.Errorf("mirror category[%d] = %q, want %q (canonical ORDER is part of the contract)", i, m[1], bundle.BotCategories[i].Slug)
		}
	}
}
