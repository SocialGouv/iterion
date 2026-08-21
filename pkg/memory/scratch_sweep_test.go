package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mkScratch builds a scratch root whose entries carry the given ages,
// writing the timestamp on a nested FILE rather than the entry root —
// which is the case the sweep has to get right.
func mkScratch(t *testing.T, ages map[string]time.Duration, now time.Time) string {
	t.Helper()
	root := t.TempDir()
	for name, age := range ages {
		deep := filepath.Join(root, name, "nested", "deeper")
		if err := os.MkdirAll(deep, 0o755); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(deep, "state.bin")
		if err := os.WriteFile(f, make([]byte, 1024), 0o644); err != nil {
			t.Fatal(err)
		}
		when := now.Add(-age)
		for _, p := range []string{f, deep, filepath.Join(root, name, "nested"), filepath.Join(root, name)} {
			if err := os.Chtimes(p, when, when); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

// The concurrency guarantee, and the whole reason age is the criterion:
// scratch is SHARED between runs (a subbot child writes into its parent's
// scratch — that is how fan-in works), so an entry a live run is still
// filling must survive the sweep even though no run "owns" it.
//
// Mutation coverage: sweep on the entry root's own mtime instead of the
// newest mtime in its subtree → the freshly-written nested file no longer
// protects its entry and this deletes it.
func TestSweepScratchSparesEntriesALiveRunIsWriting(t *testing.T) {
	now := time.Now()
	root := mkScratch(t, map[string]time.Duration{
		"stale-from-a-finished-run": 30 * 24 * time.Hour,
		"a-live-run-is-writing":     0,
		"written-an-hour-ago":       time.Hour,
	}, now)

	swept, freed, err := SweepScratch(root, DefaultScratchRetention, now, false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].Name != "stale-from-a-finished-run" {
		t.Fatalf("swept %v, want only the stale entry", names(swept))
	}
	if freed == 0 {
		t.Error("reclaimed 0 bytes for a 1 KiB entry")
	}
	for _, keep := range []string{"a-live-run-is-writing", "written-an-hour-ago"} {
		if _, err := os.Stat(filepath.Join(root, keep)); err != nil {
			t.Errorf("%s was deleted — a run still writing to it just lost its working state", keep)
		}
	}
}

// Writing INTO a subdirectory does not touch its ancestors, so an entry
// whose root mtime is ancient can still be under active use.
func TestScanScratchUsesTheNewestMtimeInTheSubtree(t *testing.T) {
	now := time.Now()
	root := t.TempDir()
	deep := filepath.Join(root, "busy", "chunks")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "busy"), old, old); err != nil {
		t.Fatal(err)
	}
	// …but a file inside was written seconds ago.
	if err := os.WriteFile(filepath.Join(deep, "part-7.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := ScanScratch(root, DefaultScratchRetention, now)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Stale {
		t.Error("an entry written to seconds ago reads as stale — its ancestors' mtime was believed over its contents")
	}
}

// Dot-prefixed entries hold state shared across runs (the same rule
// `iterion clean` applies to the worktree root), so the sweep leaves them
// alone whatever their age.
func TestScanScratchSkipsDotEntries(t *testing.T) {
	now := time.Now()
	root := mkScratch(t, map[string]time.Duration{
		".state":   365 * 24 * time.Hour,
		"ordinary": 365 * 24 * time.Hour,
	}, now)

	swept, _, err := SweepScratch(root, DefaultScratchRetention, now, false)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || swept[0].Name != "ordinary" {
		t.Fatalf("swept %v, want only the non-dot entry", names(swept))
	}
	if _, err := os.Stat(filepath.Join(root, ".state")); err != nil {
		t.Error(".state was deleted — it holds state shared across runs")
	}
}

func TestSweepScratchDryRunDeletesNothing(t *testing.T) {
	now := time.Now()
	root := mkScratch(t, map[string]time.Duration{"old": 30 * 24 * time.Hour}, now)

	swept, freed, err := SweepScratch(root, DefaultScratchRetention, now, true)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(swept) != 1 || freed == 0 {
		t.Fatalf("dry run reported %v / %d bytes, want the entry it would take", names(swept), freed)
	}
	if _, err := os.Stat(filepath.Join(root, "old")); err != nil {
		t.Error("dry run deleted the entry")
	}
}

// A missing scratch root is the common case — most workspaces never write
// one — and must not be an error a caller has to special-case.
func TestSweepScratchMissingRootIsNotAnError(t *testing.T) {
	swept, freed, err := SweepScratch(filepath.Join(t.TempDir(), "never-created"), time.Hour, time.Now(), false)
	if err != nil || len(swept) != 0 || freed != 0 {
		t.Errorf("missing root → (%v, %d, %v), want (nil, 0, nil)", names(swept), freed, err)
	}
}

func TestScratchRetentionFromEnv(t *testing.T) {
	t.Setenv(EnvScratchRetention, "")
	if got, err := ScratchRetentionFromEnv(DefaultScratchRetention); err != nil || got != DefaultScratchRetention {
		t.Errorf("unset → (%v, %v), want the default", got, err)
	}
	for _, off := range []string{"off", "none", "0"} {
		t.Setenv(EnvScratchRetention, off)
		if got, err := ScratchRetentionFromEnv(DefaultScratchRetention); err != nil || got != 0 {
			t.Errorf("%q → (%v, %v), want (0, nil) — the operator turned the sweep off", off, got, err)
		}
	}
	t.Setenv(EnvScratchRetention, "24h")
	if got, err := ScratchRetentionFromEnv(DefaultScratchRetention); err != nil || got != 24*time.Hour {
		t.Errorf("24h → (%v, %v), want 24h", got, err)
	}
	// A typo must surface. Silently falling back would either keep
	// deleting on a retention the operator thought they had changed, or
	// stop deleting on one they thought they had set.
	t.Setenv(EnvScratchRetention, "7days")
	if _, err := ScratchRetentionFromEnv(DefaultScratchRetention); err == nil {
		t.Error(`"7days" accepted, want an error`)
	}
}

func names(es []ScratchEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}
