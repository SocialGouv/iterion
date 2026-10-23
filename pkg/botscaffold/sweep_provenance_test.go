package botscaffold

// Witnesses for SweepStale's deletion safety: the sweep removes by
// PROVENANCE — staging shape (IsStagingDir) AND the marker Scaffold
// writes as its staging's first file — never by name pattern alone. A
// lookalike a user made survives with its bytes; a leftover the scaffold
// itself wrote goes. Survivors are asserted BY BYTES — a silent
// truncation must fail the row, not just a vanished path — and nothing
// here removes anything itself: every tree lives in t.TempDir and is
// cleaned up by the framework.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pastAge puts each path's mtime beyond the sweep's one-hour cutoff.
func pastAge(t *testing.T, paths ...string) {
	t.Helper()
	old := time.Now().Add(-24 * time.Hour)
	for _, p := range paths {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

// markStaging writes the marker into dir exactly as Scaffold does — the
// staging's first file, the same fixed bytes.
func markStaging(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, StagingMarkerName), []byte(stagingMarkerBody), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertBytes reads path and holds it to want: the data-loss witnesses
// assert CONTENT, not existence.
func assertBytes(t *testing.T, path, want, what string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if string(data) != want {
		t.Fatalf("%s: content = %q, want %q", what, data, want)
	}
}

// writeTree lays files (slash-relative) under dir, as another writer would.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// sweep is the shape the server wires at start: one hour of staleness.
func sweep(t *testing.T, parent string) []string {
	t.Helper()
	return SweepStale(parent, time.Hour)
}

// markedStaging creates a staging directory the way Scaffold does — the
// same MkdirTemp pattern beside the target — marks it, fills it, and ages
// it past the cutoff.
func markedStaging(t *testing.T, parent, target string, files map[string]string) string {
	t.Helper()
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(target)+".scaffold-*")
	if err != nil {
		t.Fatal(err)
	}
	markStaging(t, stage)
	writeTree(t, stage, files)
	pastAge(t, stage)
	return stage
}

func TestSweepStaleRemovesOnlyItsOwnStaging(t *testing.T) {
	const userNotes = "quarterly report drafts — nobody's scaffold wrote this\n"
	const staleBot = "workflow w:\n  entry: done\n"

	t.Run("user lookalike dot-dir survives with its bytes", func(t *testing.T) {
		parent := t.TempDir()
		look := filepath.Join(parent, ".archive.scaffold-2024")
		writeTree(t, look, map[string]string{
			"notes.txt":    userNotes,
			"old/main.bot": staleBot,
		})
		pastAge(t, look)
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep removed %v — a directory nobody's scaffold created, matched by name only: DELETED USER DATA", removed)
		}
		assertBytes(t, filepath.Join(look, "notes.txt"), userNotes, "the lookalike's file")
		assertBytes(t, filepath.Join(look, "old", "main.bot"), staleBot, "the lookalike's nested file")
	})

	t.Run("markerless staging-shaped tree survives (a pre-marker leftover)", func(t *testing.T) {
		parent := t.TempDir()
		pre := filepath.Join(parent, ".old.scaffold-legacy")
		writeTree(t, pre, map[string]string{
			"main.bot":  staleBot,
			"README.md": "# left by a binary older than the marker\n",
		})
		pastAge(t, pre)
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep removed %v — a pre-marker leftover is no longer swept; an operator removes it by hand", removed)
		}
		assertBytes(t, filepath.Join(pre, "main.bot"), staleBot, "the pre-marker leftover")
	})

	t.Run("a directory named like the marker is no provenance", func(t *testing.T) {
		parent := t.TempDir()
		look := filepath.Join(parent, ".impostor.scaffold-1")
		writeTree(t, filepath.Join(look, StagingMarkerName), map[string]string{"placeholder": ""})
		writeTree(t, look, map[string]string{"notes.txt": userNotes})
		pastAge(t, look)
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep removed %v — a marker-named directory is not the marker file", removed)
		}
		assertBytes(t, filepath.Join(look, "notes.txt"), userNotes, "the impostor's file")
	})

	t.Run("a symlink named like the marker is no provenance", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "bots")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		look := filepath.Join(parent, ".impostor.scaffold-2")
		writeTree(t, look, map[string]string{"notes.txt": userNotes})
		if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(look, StagingMarkerName)); err != nil {
			t.Fatal(err)
		}
		pastAge(t, look)
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep removed %v — a symlink by the marker's name proves nothing", removed)
		}
		assertBytes(t, filepath.Join(look, "notes.txt"), userNotes, "the impostor's file")
	})

	t.Run("a marked stale staging holding files is removed", func(t *testing.T) {
		parent := t.TempDir()
		stage := markedStaging(t, parent, "my-bot", map[string]string{"main.bot": staleBot})
		// Assert the setup first: the files were there, with these bytes,
		// before the sweep is allowed to take them.
		assertBytes(t, filepath.Join(stage, "main.bot"), staleBot, "the staging before the sweep")
		removed := sweep(t, parent)
		if len(removed) != 1 || removed[0] != stage {
			t.Fatalf("removed = %v, want exactly [%s]", removed, stage)
		}
		if _, err := os.Lstat(stage); !os.IsNotExist(err) {
			t.Fatalf("the marked staging survived the sweep: %v", err)
		}
	})

	t.Run("a fresh marked staging is untouched", func(t *testing.T) {
		parent := t.TempDir()
		stage := filepath.Join(parent, ".fresh.scaffold-now")
		if err := os.MkdirAll(stage, 0o755); err != nil {
			t.Fatal(err)
		}
		markStaging(t, stage)
		writeTree(t, stage, map[string]string{"main.bot": staleBot})
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep took an in-flight create's staging: %v", removed)
		}
		assertBytes(t, filepath.Join(stage, "main.bot"), staleBot, "the fresh staging")
	})

	t.Run("a symlink with the staging shape is untouched, its target too", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "bots")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(root, "outside")
		const keep = "the symlink target's bytes\n"
		writeTree(t, outside, map[string]string{"keep.txt": keep})
		link := filepath.Join(parent, ".link.scaffold-q")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep touched a symlink: %v", removed)
		}
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("the symlink vanished: %v", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is no longer a symlink: %v", link, info.Mode())
		}
		assertBytes(t, filepath.Join(outside, "keep.txt"), keep, "the symlink target")
	})

	t.Run("a FILE with the staging shape is untouched", func(t *testing.T) {
		parent := t.TempDir()
		f := filepath.Join(parent, ".file.scaffold-x")
		const body = "just a file with an unfortunate name\n"
		if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		pastAge(t, f)
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep touched a file: %v", removed)
		}
		assertBytes(t, f, body, "the file with the staging shape")
	})

	t.Run("a staging-shaped dir that is not a direct child is untouched", func(t *testing.T) {
		parent := t.TempDir()
		nested := filepath.Join(parent, "real-bundle", ".deep.scaffold-1")
		writeTree(t, nested, map[string]string{"data.txt": userNotes})
		markStaging(t, nested) // marked AND stale: only its depth saves it
		pastAge(t, nested)
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep walked beyond the parent: %v", removed)
		}
		assertBytes(t, filepath.Join(nested, "data.txt"), userNotes, "the nested dir")
	})

	t.Run("a read-only parent is a no-op", func(t *testing.T) {
		parent := t.TempDir()
		stage := markedStaging(t, parent, "my-bot", map[string]string{"main.bot": staleBot})
		look := filepath.Join(parent, ".archive.scaffold-2024")
		writeTree(t, look, map[string]string{"notes.txt": userNotes})
		if err := os.Chmod(parent, 0o555); err != nil {
			t.Fatal(err)
		}
		if removed := sweep(t, parent); len(removed) != 0 {
			t.Fatalf("the sweep reported removals from a read-only parent: %v", removed)
		}
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		// The user's data is what must survive; the marked staging is the
		// sweep's own garbage — an unwritable parent can leave it (RemoveAll
		// fails on the parent's unlink) for the next sweep or the operator.
		if _, err := os.Lstat(stage); err != nil {
			t.Fatalf("the marked staging vanished from a read-only parent: %v", err)
		}
		assertBytes(t, filepath.Join(look, "notes.txt"), userNotes, "the lookalike under the read-only parent")
	})

	t.Run("a missing parent is nil", func(t *testing.T) {
		if removed := SweepStale(filepath.Join(t.TempDir(), "absent"), time.Hour); removed != nil {
			t.Fatalf("removed = %v, want nil", removed)
		}
	})

	t.Run("two sweeps are benign", func(t *testing.T) {
		parent := t.TempDir()
		stage := markedStaging(t, parent, "my-bot", map[string]string{"main.bot": staleBot})
		look := filepath.Join(parent, ".archive.scaffold-2024")
		writeTree(t, look, map[string]string{"notes.txt": userNotes})
		fresh := filepath.Join(parent, ".fresh.scaffold-now")
		if err := os.MkdirAll(fresh, 0o755); err != nil {
			t.Fatal(err)
		}
		markStaging(t, fresh)

		if first := sweep(t, parent); len(first) != 1 || first[0] != stage {
			t.Fatalf("first sweep = %v, want exactly [%s]", first, stage)
		}
		if second := sweep(t, parent); len(second) != 0 {
			t.Fatalf("second sweep = %v, want nothing left to take", second)
		}
		assertBytes(t, filepath.Join(look, "notes.txt"), userNotes, "the lookalike after both sweeps")
		assertBytes(t, filepath.Join(fresh, StagingMarkerName), stagingMarkerBody, "the fresh staging after both sweeps")
	})

	t.Run("a create that dies mid-way leaves a marked staging the sweep removes", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "my-bot")
		// Exactly Scaffold's staging — the same MkdirTemp pattern beside
		// the target, marker first, bundle files after — and no publish.
		stage, err := os.MkdirTemp(parent, "."+filepath.Base(target)+".scaffold-*")
		if err != nil {
			t.Fatal(err)
		}
		markStaging(t, stage)
		writeTree(t, stage, map[string]string{"main.bot": staleBot})
		pastAge(t, stage)
		removed := sweep(t, parent)
		if len(removed) != 1 || removed[0] != stage {
			t.Fatalf("removed = %v, want exactly [%s]", removed, stage)
		}
	})
}

// TestScaffoldPublishesNoMarker: the marker is staging-only provenance —
// it comes off before the publish rename, so the bundle an operator gets
// carries none of it, and a successful publish leaves no staging behind.
func TestScaffoldPublishesNoMarker(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "my-bot")
	res, err := Scaffold(dir, minimalSpec())
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	for _, f := range res.Files {
		if strings.HasSuffix(filepath.ToSlash(f), StagingMarkerName) {
			t.Errorf("Result.Files carries the marker: %s", f)
		}
	}
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == StagingMarkerName {
			t.Errorf("the published bundle carries the marker: %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.bot")); err != nil {
		t.Fatalf("the published bundle lost its main.bot: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if IsStagingDir(e.Name()) {
			t.Errorf("a staging directory survived the publish: %s", e.Name())
		}
	}
}
