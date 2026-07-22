package bots

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botregistry"
)

// TestCommittedCatalogIsFresh fails when the committed generated catalog
// (bots/whats-next/skills/iterion-bot-catalog.md) no longer matches what the
// current manifests render. A stale committed catalog is not cosmetic: the
// engine regenerates the file at EVERY run start, so in a worktree run the
// regen produces a real diff against HEAD and the finalize wip-banks an
// otherwise-clean run (observed on back-to-back smoke runs, 2026-07-07).
// When this fails, run `iterion bots regen-catalog` and commit the result
// alongside the manifest change.
func TestCommittedCatalogIsFresh(t *testing.T) {
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	// Every bundle shipping the static template owns a generated catalog
	// (whats-next, issue-triage, …) — snapshot each committed copy, regen
	// all of them, and compare in place.
	templates, err := filepath.Glob(filepath.Join(repoRoot, "bots", "*", "iterion-bot-catalog-static.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) == 0 {
		t.Skip("no catalog template discovered in this workspace")
	}
	committed := map[string][]byte{}
	for _, tmpl := range templates {
		genPath := filepath.Join(filepath.Dir(tmpl), "skills", "iterion-bot-catalog.md")
		b, err := os.ReadFile(genPath)
		if err != nil {
			t.Fatalf("read committed catalog %s: %v", genPath, err)
		}
		committed[genPath] = b
	}

	// RegenerateWhatsNextCatalog writes atomically to each SOURCE path, so
	// render the expected content by regenerating and comparing — restoring
	// the committed bytes if they differ, to keep the working tree untouched
	// by a failing test.
	if _, err := botregistry.RegenerateWhatsNextCatalog(repoRoot); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	for genPath, before := range committed {
		regenerated, err := os.ReadFile(genPath)
		if err != nil {
			t.Fatalf("read regenerated catalog %s: %v", genPath, err)
		}
		if string(regenerated) != string(before) {
			// Leave the tree as the test found it: a failing guard must not
			// itself dirty the checkout.
			if writeErr := os.WriteFile(genPath, before, 0o644); writeErr != nil {
				t.Logf("restore committed catalog: %v", writeErr)
			}
			t.Fatalf("committed catalog is STALE vs the current manifests.\n"+
				"Every run-start regen will dirty the worktree (and wip-bank clean runs).\n"+
				"Fix: run `iterion bots regen-catalog` and commit %s with your manifest change.", genPath)
		}
	}
}
