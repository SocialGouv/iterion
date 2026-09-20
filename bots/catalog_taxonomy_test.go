package bots

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestCatalogBotsDeclareKnownCategory is the anti-absence gate for the
// navigation spine: every shipped bot manifest declares a category from
// the closed six-slug set, and every declared tag is in the curated seed.
// The bundlelint consistency gate next door already reddens UNKNOWN
// values (C240/C241 surface as warnings there); this test closes the
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
// hand-synced copies without a guard drift in silence.
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

	tags := regexp.MustCompile(`"([a-z0-9-]+)"`).FindAllStringSubmatch(tagsBlock(t, src), -1)
	if len(tags) != len(bundle.KnownBotTags) {
		t.Fatalf("studio mirror declares %d tags, Go declares %d", len(tags), len(bundle.KnownBotTags))
	}
	for i, m := range tags {
		if m[1] != bundle.KnownBotTags[i] {
			t.Errorf("mirror tag[%d] = %q, want %q", i, m[1], bundle.KnownBotTags[i])
		}
	}
}

// tagsBlock extracts the KNOWN_BOT_TAGS array literal so the tag regex
// cannot match slugs or titles elsewhere in the file.
func tagsBlock(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "KNOWN_BOT_TAGS")
	if start < 0 {
		t.Fatal("mirror does not declare KNOWN_BOT_TAGS")
	}
	// The type annotation (`readonly string[]`) also brackets — start at
	// the `=` so the located `[ … ]` is the literal, not the type.
	eq := strings.Index(src[start:], "=")
	if eq < 0 {
		t.Fatal("KNOWN_BOT_TAGS declaration has no initializer")
	}
	rest := src[start+eq:]
	open := strings.Index(rest, "[")
	close := strings.Index(rest, "]")
	if open < 0 || close < open {
		t.Fatal("KNOWN_BOT_TAGS array literal not found")
	}
	return rest[open : close+1]
}
