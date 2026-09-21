package botregistry

import (
	"path/filepath"
	"testing"
)

func TestList_BundleCarriesCategoryAndTags(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "review-pr")
	writeFile(t, filepath.Join(bundleDir, "manifest.yaml"), `name: review-pr
display_name: Revi
description: Reviews diffs.
category: verify
tags: [Code-Review, " read-only ", code-review]
`)
	writeFile(t, filepath.Join(bundleDir, "main.bot"), `agent x:
  model: "test"
`)
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	e := entries[0]
	if e.Category != "verify" {
		t.Errorf("Category = %q, want verify (manifest category must survive discovery)", e.Category)
	}
	// Normalized form (lowercase, deduped) — the manifest loader already
	// normalized; discovery must carry it verbatim.
	if len(e.Tags) != 2 || e.Tags[0] != "code-review" || e.Tags[1] != "read-only" {
		t.Errorf("Tags = %v, want [code-review read-only]", e.Tags)
	}
}

func TestList_UncategorizedBundleStaysListed(t *testing.T) {
	// A bundle with no category (third-party, loose-era) must still be
	// discovered: Uncategorized is a visible group, never a filter.
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "bare-bot")
	writeFile(t, filepath.Join(bundleDir, "manifest.yaml"), "name: bare-bot\ndescription: no taxonomy.\n")
	writeFile(t, filepath.Join(bundleDir, "main.bot"), `agent x:
  model: "test"
`)
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Category != "" {
		t.Fatalf("entries = %+v, want one uncategorized bot", entries)
	}
}
