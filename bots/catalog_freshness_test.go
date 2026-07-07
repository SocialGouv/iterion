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
	committedPath := filepath.Join(repoRoot, "bots", "whats-next", "skills", "iterion-bot-catalog.md")
	committed, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatalf("read committed catalog: %v", err)
	}

	// Regenerate into a scratch copy of the owning bundle's layout is not
	// needed: RegenerateWhatsNextCatalog writes atomically to the SOURCE
	// path, so render the expected content by regenerating and comparing —
	// restoring the committed bytes if they differ, to keep the working
	// tree untouched by a failing test.
	dest, err := botregistry.RegenerateWhatsNextCatalog(repoRoot)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if dest == "" {
		t.Skip("no catalog template discovered in this workspace")
	}
	regenerated, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read regenerated catalog: %v", err)
	}

	if string(regenerated) != string(committed) {
		// Leave the tree as the test found it: a failing guard must not
		// itself dirty the checkout.
		if writeErr := os.WriteFile(committedPath, committed, 0o644); writeErr != nil {
			t.Logf("restore committed catalog: %v", writeErr)
		}
		t.Fatalf("committed catalog is STALE vs the current manifests.\n"+
			"Every run-start regen will dirty the worktree (and wip-bank clean runs).\n"+
			"Fix: run `iterion bots regen-catalog` and commit %s with your manifest change.", committedPath)
	}
}
