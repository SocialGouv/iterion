package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDirForEntry: a root-level .bot beside the bundle's main.bot is an
// entry of the bundle — under either marker — while a file under its
// subdirectories is a fragment of a unit, a .bot with no main.bot beside
// it is loose (the directory form refuses that tree too), and a non-.bot
// file is nobody's entry.
func TestDirForEntry(t *testing.T) {
	for _, marker := range []string{DirSkills, ManifestFile} {
		t.Run("a root-level sibling, "+marker, func(t *testing.T) {
			b := filepath.Join(t.TempDir(), "b")
			writeBundleMarker(t, b, marker)
			write := func(rel, body string) string {
				t.Helper()
				p := filepath.Join(b, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			}
			write(MainBotFile, "workflow main_w:\n  entry: done\n")
			if got := DirForEntry(write("extend.bot", "workflow extend_w:\n  entry: done\n")); got != b {
				t.Errorf("DirForEntry(extend.bot) = %q, want the bundle %q", got, b)
			}
			if got := DirForEntry(filepath.Join(b, MainBotFile)); got != b {
				t.Errorf("DirForEntry(main.bot) = %q, want the bundle %q", got, b)
			}
			if got := DirForEntry(write("lib/frag.bot", "agent frag:\n  description: \"x\"\n")); got != "" {
				t.Errorf("DirForEntry(lib/frag.bot) = %q, want \"\": a fragment is no entry", got)
			}
			if got := DirForEntry(write("sync-harness.py", "# not a workflow\n")); got != "" {
				t.Errorf("DirForEntry(sync-harness.py) = %q, want \"\": a non-.bot file is nobody's entry", got)
			}
			// A DIRECTORY named *.bot is no entry either.
			dirBot := filepath.Join(b, "assets.bot")
			if err := os.MkdirAll(dirBot, 0o755); err != nil {
				t.Fatal(err)
			}
			if got := DirForEntry(dirBot); got != "" {
				t.Errorf("DirForEntry(assets.bot/) = %q, want \"\": a directory is no entry", got)
			}
		})
	}
	t.Run("no main.bot beside", func(t *testing.T) {
		b := filepath.Join(t.TempDir(), "b")
		writeBundleMarker(t, b, ManifestFile)
		sib := filepath.Join(b, "extend.bot")
		if err := os.WriteFile(sib, []byte("workflow extend_w:\n  entry: done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := DirForEntry(sib); got != "" {
			t.Errorf("DirForEntry = %q beside a manifest with no main.bot, want \"\": the directory form refuses that tree too", got)
		}
	})
	t.Run("a loose file", func(t *testing.T) {
		loose := filepath.Join(t.TempDir(), "x.bot")
		if err := os.WriteFile(loose, []byte("workflow x:\n  entry: done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := DirForEntry(loose); got != "" {
			t.Errorf("DirForEntry = %q for a loose file, want \"\"", got)
		}
		if got := DirForEntry(""); got != "" {
			t.Errorf("DirForEntry(\"\") = %q, want \"\"", got)
		}
	})
}

// TestEntryForCopy: the store's materialised copy of a bundle workflow
// (`<12 hex>-<entry basename>`, outside the bundle) resolves to the entry
// it was made of; anything else — a path inside the bundle, a basename the
// pattern does not shape, an entry that is gone — resolves to "".
func TestEntryForCopy(t *testing.T) {
	b := filepath.Join(t.TempDir(), "b")
	writeBundleMarker(t, b, ManifestFile)
	write := func(base string) string {
		t.Helper()
		p := filepath.Join(b, base)
		if err := os.WriteFile(p, []byte("workflow x:\n  entry: done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	main := write(MainBotFile)
	worker := write("worker.bot")
	cache := t.TempDir()
	copyOf := func(base string) string {
		t.Helper()
		p := filepath.Join(cache, "a1b2c3d4e5f6-"+base)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := EntryForCopy(copyOf("worker.bot"), b); got != worker {
		t.Errorf("EntryForCopy(copy of worker.bot) = %q, want %q", got, worker)
	}
	if got := EntryForCopy(copyOf("main.bot"), b); got != main {
		t.Errorf("EntryForCopy(copy of main.bot) = %q, want %q", got, main)
	}
	for _, p := range []string{
		worker,                             // inside the bundle: the file itself, not a copy
		copyOf("ghost.bot"),                // the entry is gone
		filepath.Join(cache, "worker.bot"), // no hash prefix
		filepath.Join(cache, "zzzzzzzzzzzz-worker.bot"), // not hex
		filepath.Join(cache, "a1b2c3d4e5f6-notes.txt"),  // not a workflow file
		"",
	} {
		if got := EntryForCopy(p, b); got != "" {
			t.Errorf("EntryForCopy(%q) = %q, want \"\"", p, got)
		}
	}
	if got := EntryForCopy(copyOf("worker.bot"), ""); got != "" {
		t.Errorf("EntryForCopy(copy, \"\") = %q, want \"\"", got)
	}
}
