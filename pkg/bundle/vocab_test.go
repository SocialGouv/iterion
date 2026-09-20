package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestWithTaxonomy = `# changelog comment that must survive a metadata write
name: testbot
display_name: Testy
description: |
  A test bot.
schema_version: 1
`

func decodeTaxonomy(t *testing.T, body string) *Manifest {
	t.Helper()
	m, err := DecodeManifest([]byte(body), "test")
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	return m
}

func TestDecodeManifest_NormalizesCategoryAndTagForm(t *testing.T) {
	m := decodeTaxonomy(t, manifestWithTaxonomy+`
category:  Verify
tags: [Security, " ships-code ", security, "", SHIPYARD]
`)
	// Form normalization only: trimmed + lowercased + deduped, empties gone.
	if m.Category != "verify" {
		t.Errorf("Category = %q, want verify (trimmed + lowercased)", m.Category)
	}
	want := []string{"security", "ships-code", "shipyard"}
	if len(m.Tags) != len(want) {
		t.Fatalf("Tags = %v, want %v", m.Tags, want)
	}
	for i := range want {
		if m.Tags[i] != want[i] {
			t.Errorf("Tags[%d] = %q, want %q", i, m.Tags[i], want[i])
		}
	}
}

func TestDecodeManifest_UnknownCategoryAndTagsStayDeclared(t *testing.T) {
	// The VALUE is never rewritten: an unknown slug stays declared so the
	// soft lint and the Uncategorized group can name it — a silently
	// blanked or replaced choice is the failure mode this field forbids.
	m := decodeTaxonomy(t, manifestWithTaxonomy+`
category: Pilot
tags: [security, brand-new-facet]
`)
	if m.Category != "pilot" {
		t.Errorf("Category = %q, want pilot preserved (never blanked)", m.Category)
	}
	if len(m.Tags) != 2 || m.Tags[1] != "brand-new-facet" {
		t.Errorf("Tags = %v, want [security brand-new-facet] preserved", m.Tags)
	}
}

func TestDecodeManifest_EmptyTaxonomyStaysAbsent(t *testing.T) {
	m := decodeTaxonomy(t, manifestWithTaxonomy+"tags: [\"\", \"  \"]\n")
	if m.Category != "" || m.Tags != nil {
		t.Errorf("Category = %q, Tags = %v, want both effectively absent", m.Category, m.Tags)
	}
}

func TestWriteManifest_CategoryAndTagsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(manifestWithTaxonomy), 0o644); err != nil {
		t.Fatal(err)
	}

	m, err := WriteManifest(path, ManifestPatch{
		Category: ptr("VERIFY"),
		Tags:     &[]string{" read-only ", "security", "security"},
	})
	if err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if m.Category != "verify" {
		t.Errorf("Category = %q, want verify", m.Category)
	}
	if len(m.Tags) != 2 || m.Tags[0] != "read-only" || m.Tags[1] != "security" {
		t.Errorf("Tags = %v, want [read-only security] (deduped)", m.Tags)
	}

	// The round-trip must not cost the hand-authored changelog comment.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# changelog comment that must survive a metadata write") {
		t.Errorf("metadata write lost the file's comments\n---\n%s", raw)
	}

	// A second load sees the same values (strict decoder accepts the v3
	// writer's output).
	reloaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Category != "verify" || len(reloaded.Tags) != 2 {
		t.Errorf("reloaded Category=%q Tags=%v, want verify + 2 tags", reloaded.Category, reloaded.Tags)
	}

	// Clearing keeps the key present with an empty value (the patch's
	// "empty string is a valid value" contract).
	if _, err := WriteManifest(path, ManifestPatch{Category: ptr("")}); err != nil {
		t.Fatalf("clear category: %v", err)
	}
	reloaded, err = LoadManifest(path)
	if err != nil {
		t.Fatalf("reload after clear: %v", err)
	}
	if reloaded.Category != "" {
		t.Errorf("Category = %q after clearing, want empty", reloaded.Category)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "category:") {
		t.Errorf("cleared category key vanished from the file\n---\n%s", out)
	}
}

func TestBotCategoryVocabulary(t *testing.T) {
	if len(BotCategories) != 6 {
		t.Fatalf("got %d categories, want the closed set of 6", len(BotCategories))
	}
	seen := map[string]bool{}
	for _, c := range BotCategories {
		if c.Slug == "" || c.Title == "" || c.Tagline == "" {
			t.Errorf("category %+v has an empty slug/title/tagline", c)
		}
		if seen[c.Slug] {
			t.Errorf("duplicate category slug %q", c.Slug)
		}
		seen[c.Slug] = true
		if got, ok := BotCategoryBySlug(c.Slug); !ok || got != c {
			t.Errorf("BotCategoryBySlug(%q) = %+v, %v; want %+v, true", c.Slug, got, ok, c)
		}
	}
	if _, ok := BotCategoryBySlug("nope"); ok {
		t.Error("BotCategoryBySlug(nope) resolved, want miss")
	}
	for _, tag := range KnownBotTags {
		if tag != strings.ToLower(tag) || strings.Contains(tag, " ") {
			t.Errorf("seed tag %q is not lowercase kebab", tag)
		}
		if !IsKnownBotTag(tag) {
			t.Errorf("IsKnownBotTag(%q) = false within its own seed", tag)
		}
	}
	if IsKnownBotTag("not-a-seed-tag") {
		t.Error("IsKnownBotTag matched an unseeded tag")
	}
}
