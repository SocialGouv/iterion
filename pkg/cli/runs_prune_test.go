package cli

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/workspacetrack"
)

// pruneTestFixture seeds a fresh filesystem store with a set of runs
// at controlled timestamps and returns the store dir + a store handle.
type pruneTestFixture struct {
	dir   string
	store *store.FilesystemRunStore
	now   time.Time
}

func newPruneFixture(t *testing.T) *pruneTestFixture {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return &pruneTestFixture{
		dir:   dir,
		store: s,
		now:   time.Now().UTC(),
	}
}

// seedRun creates a run with the given id, status, and age relative to
// f.now (how far in the past its UpdatedAt should sit).
func (f *pruneTestFixture) seedRun(t *testing.T, id string, status store.RunStatus, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.CreateRun(ctx, id, "wf-"+id, nil); err != nil {
		t.Fatalf("CreateRun(%s): %v", id, err)
	}
	if err := f.store.UpdateRunStatus(ctx, id, status, ""); err != nil {
		t.Fatalf("UpdateRunStatus(%s): %v", id, err)
	}
	// Back-date UpdatedAt (and FinishedAt) so age filtering has
	// deterministic input. Write directly through SaveRun.
	r, err := f.store.LoadRun(ctx, id)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", id, err)
	}
	past := f.now.Add(-age)
	r.UpdatedAt = past
	if r.FinishedAt != nil {
		r.FinishedAt = &past
	}
	// Sanity — CreatedAt is <= UpdatedAt for realism.
	if r.CreatedAt.After(past) {
		r.CreatedAt = past
	}
	if err := f.store.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun(%s): %v", id, err)
	}
}

func (f *pruneTestFixture) run(t *testing.T, opts PruneOptions) PruneResult {
	t.Helper()
	if opts.StoreDir == "" {
		opts.StoreDir = f.dir
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return f.now }
	}
	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	if err := RunPrune(opts, p); err != nil {
		t.Fatalf("RunPrune: %v", err)
	}
	// Return a synthetic result by re-listing what survived — the
	// on-disk state is the source of truth for the deletion assertions.
	survived, err := f.store.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	// Emit the human output so a failing test can trace what the
	// operator would have seen.
	t.Logf("prune output:\n%s", buf.String())
	return PruneResult{Pruned: nil, Scanned: len(survived) /* survived */}
}

// listRunIDs returns the current run IDs on disk (sorted).
func listRunIDs(t *testing.T, s *store.FilesystemRunStore) []string {
	t.Helper()
	ids, err := s.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	sort.Strings(ids)
	return ids
}

// -----------------------------------------------------------------------
// Missing / empty store dir is not an error
// -----------------------------------------------------------------------

func TestRunPrune_MissingStoreDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	if err := RunPrune(PruneOptions{StoreDir: missing, OlderThan: 24 * time.Hour}, p); err != nil {
		t.Fatalf("RunPrune on missing store: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("prune must NOT create the missing store dir; got err=%v", err)
	}
	if !strings.Contains(buf.String(), "nothing to prune") {
		t.Fatalf("expected 'nothing to prune' in output, got %q", buf.String())
	}
}

func TestRunPrune_StoreExistsButNoRunsSubdir(t *testing.T) {
	dir := t.TempDir()
	// dir exists but has no runs/ subdir yet.
	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	if err := RunPrune(PruneOptions{StoreDir: dir, OlderThan: 24 * time.Hour}, p); err != nil {
		t.Fatalf("RunPrune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); !os.IsNotExist(err) {
		t.Fatalf("prune must NOT create runs/ when it did not exist; got err=%v", err)
	}
}

// -----------------------------------------------------------------------
// Age filtering
// -----------------------------------------------------------------------

func TestRunPrune_AgeFiltering(t *testing.T) {
	f := newPruneFixture(t)
	f.seedRun(t, "old", store.RunStatusFinished, 40*24*time.Hour)   // 40 days old
	f.seedRun(t, "young", store.RunStatusFinished, 10*24*time.Hour) // 10 days old
	f.run(t, PruneOptions{OlderThan: 30 * 24 * time.Hour})

	remaining := listRunIDs(t, f.store)
	if len(remaining) != 1 || remaining[0] != "young" {
		t.Fatalf("expected only 'young' to survive, got %v", remaining)
	}
}

// -----------------------------------------------------------------------
// Status filtering (default excludes failed_resumable)
// -----------------------------------------------------------------------

func TestRunPrune_DefaultStatusExcludesFailedResumable(t *testing.T) {
	f := newPruneFixture(t)
	// All 40 days old — only status differs.
	f.seedRun(t, "finished", store.RunStatusFinished, 40*24*time.Hour)
	f.seedRun(t, "failed", store.RunStatusFailed, 40*24*time.Hour)
	f.seedRun(t, "cancelled", store.RunStatusCancelled, 40*24*time.Hour)
	f.seedRun(t, "resumable", store.RunStatusFailedResumable, 40*24*time.Hour)

	f.run(t, PruneOptions{OlderThan: 30 * 24 * time.Hour})

	remaining := listRunIDs(t, f.store)
	if len(remaining) != 1 || remaining[0] != "resumable" {
		t.Fatalf("failed_resumable must survive the default prune; got remaining=%v", remaining)
	}
}

func TestRunPrune_ExplicitFailedResumable(t *testing.T) {
	f := newPruneFixture(t)
	f.seedRun(t, "resumable", store.RunStatusFailedResumable, 40*24*time.Hour)
	f.seedRun(t, "finished", store.RunStatusFinished, 40*24*time.Hour)

	f.run(t, PruneOptions{
		OlderThan: 30 * 24 * time.Hour,
		Statuses:  []string{"failed_resumable"},
	})

	remaining := listRunIDs(t, f.store)
	if len(remaining) != 1 || remaining[0] != "finished" {
		t.Fatalf("only 'finished' should survive when --status=failed_resumable; got %v", remaining)
	}
}

// -----------------------------------------------------------------------
// Invalid status → error
// -----------------------------------------------------------------------

func TestRunPrune_InvalidStatusIsError(t *testing.T) {
	dir := t.TempDir()
	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	err := RunPrune(PruneOptions{
		StoreDir: dir,
		Statuses: []string{"running"},
	}, p)
	if err == nil {
		t.Fatal("expected error for --status=running (non-terminal), got nil")
	}
	if !strings.Contains(err.Error(), "running") || !strings.Contains(err.Error(), "not prunable") {
		t.Fatalf("error should name the rejected status and say not prunable; got %q", err.Error())
	}
}

func TestRunPrune_UnknownStatusIsError(t *testing.T) {
	dir := t.TempDir()
	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	err := RunPrune(PruneOptions{
		StoreDir: dir,
		Statuses: []string{"totally-made-up"},
	}, p)
	if err == nil {
		t.Fatal("expected error for unknown status, got nil")
	}
}

// -----------------------------------------------------------------------
// --keep-last preserves the N most recent matching runs
// -----------------------------------------------------------------------

func TestRunPrune_KeepLast(t *testing.T) {
	f := newPruneFixture(t)
	// Five finished runs, all older than the age gate — ages 40..44 days.
	// Newest of the batch = "r0" (40d), oldest = "r4" (44d).
	f.seedRun(t, "r0", store.RunStatusFinished, 40*24*time.Hour)
	f.seedRun(t, "r1", store.RunStatusFinished, 41*24*time.Hour)
	f.seedRun(t, "r2", store.RunStatusFinished, 42*24*time.Hour)
	f.seedRun(t, "r3", store.RunStatusFinished, 43*24*time.Hour)
	f.seedRun(t, "r4", store.RunStatusFinished, 44*24*time.Hour)

	f.run(t, PruneOptions{OlderThan: 30 * 24 * time.Hour, KeepLast: 2})

	remaining := listRunIDs(t, f.store)
	sort.Strings(remaining)
	want := []string{"r0", "r1"}
	if len(remaining) != len(want) || remaining[0] != want[0] || remaining[1] != want[1] {
		t.Fatalf("expected 2 newest matching runs to survive, got %v", remaining)
	}
}

// -----------------------------------------------------------------------
// --dry-run deletes nothing
// -----------------------------------------------------------------------

func TestRunPrune_DryRunDeletesNothing(t *testing.T) {
	f := newPruneFixture(t)
	f.seedRun(t, "old", store.RunStatusFinished, 40*24*time.Hour)
	f.seedRun(t, "old2", store.RunStatusFailed, 45*24*time.Hour)

	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	err := RunPrune(PruneOptions{
		StoreDir:  f.dir,
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    true,
		Now:       func() time.Time { return f.now },
	}, p)
	if err != nil {
		t.Fatalf("RunPrune: %v", err)
	}

	remaining := listRunIDs(t, f.store)
	sort.Strings(remaining)
	if len(remaining) != 2 || remaining[0] != "old" || remaining[1] != "old2" {
		t.Fatalf("dry-run must not delete; got remaining=%v", remaining)
	}
	if !strings.Contains(buf.String(), "would prune") {
		t.Fatalf("dry-run output should say 'would prune', got %q", buf.String())
	}
}

// -----------------------------------------------------------------------
// worktrees/ directory is never touched
// -----------------------------------------------------------------------

func TestRunPrune_WorktreesUntouched(t *testing.T) {
	f := newPruneFixture(t)
	// Seed both a run and a matching worktree dir.
	f.seedRun(t, "old", store.RunStatusFinished, 40*24*time.Hour)
	wtDir := filepath.Join(f.dir, "worktrees", "old")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatalf("mkdir worktrees: %v", err)
	}
	sentinel := filepath.Join(wtDir, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Add an unrelated worktree that has no matching run — the prune
	// must leave it alone too.
	orphan := filepath.Join(f.dir, "worktrees", "unrelated-run")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	orphanFile := filepath.Join(orphan, "keep.txt")
	if err := os.WriteFile(orphanFile, []byte("still here"), 0o644); err != nil {
		t.Fatalf("write orphan: %v", err)
	}

	f.run(t, PruneOptions{OlderThan: 30 * 24 * time.Hour})

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("prune deleted the run's worktree sentinel: %v", err)
	}
	if _, err := os.Stat(orphanFile); err != nil {
		t.Fatalf("prune deleted an unrelated worktree: %v", err)
	}
	// The run itself must be gone.
	remaining := listRunIDs(t, f.store)
	if len(remaining) != 0 {
		t.Fatalf("expected the run to be pruned, got %v", remaining)
	}
}

// -----------------------------------------------------------------------
// Negative flags → user-input errors
// -----------------------------------------------------------------------

func TestRunPrune_NegativeOlderThan(t *testing.T) {
	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	err := RunPrune(PruneOptions{OlderThan: -1 * time.Hour}, p)
	if err == nil {
		t.Fatal("expected error for negative --older-than")
	}
}

func TestRunPrune_NegativeKeepLast(t *testing.T) {
	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	err := RunPrune(PruneOptions{OlderThan: time.Hour, KeepLast: -3}, p)
	if err == nil {
		t.Fatal("expected error for negative --keep-last")
	}
}

// -----------------------------------------------------------------------
// JSON output shape
// -----------------------------------------------------------------------

func TestRunPrune_JSONOutput(t *testing.T) {
	f := newPruneFixture(t)
	f.seedRun(t, "old", store.RunStatusFinished, 40*24*time.Hour)
	f.seedRun(t, "young", store.RunStatusFinished, 5*24*time.Hour)

	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputJSON}
	err := RunPrune(PruneOptions{
		StoreDir:  f.dir,
		OlderThan: 30 * 24 * time.Hour,
		DryRun:    true,
		Now:       func() time.Time { return f.now },
	}, p)
	if err != nil {
		t.Fatalf("RunPrune: %v", err)
	}

	// Sanity-check the top-level shape without over-fitting to fields
	// that might legitimately be added later.
	out := buf.String()
	for _, want := range []string{
		`"store_dir"`, `"age_field"`, `"older_than"`, `"statuses"`,
		`"pruned"`, `"pruned_count": 1`, `"dry_run": true`, `"id": "old"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("JSON output missing %q; got:\n%s", want, out)
		}
	}
	// Dry-run must not have deleted the run.
	remaining := listRunIDs(t, f.store)
	if len(remaining) != 2 {
		t.Fatalf("dry-run must not delete; got remaining=%v", remaining)
	}
}

// -----------------------------------------------------------------------
// Unreadable run dirs (no loadable run.json) are surfaced, not fatal
// -----------------------------------------------------------------------

func TestRunPrune_UnreadableRunDirSurfacedNotFatal(t *testing.T) {
	f := newPruneFixture(t)
	f.seedRun(t, "old", store.RunStatusFinished, 40*24*time.Hour)
	orphan := filepath.Join(f.dir, "runs", "orphan-no-runjson")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}

	buf := &bytes.Buffer{}
	p := &Printer{W: buf, Format: OutputHuman}
	if err := RunPrune(PruneOptions{
		StoreDir:  f.dir,
		OlderThan: 30 * 24 * time.Hour,
		Now:       func() time.Time { return f.now },
	}, p); err != nil {
		t.Fatalf("an unreadable run dir must not fail the sweep: %v", err)
	}

	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphan dir must be left untouched: %v", err)
	}
	remaining := listRunIDs(t, f.store)
	for _, id := range remaining {
		if id == "old" {
			t.Fatalf("readable matching run must still be pruned; remaining=%v", remaining)
		}
	}
	if !strings.Contains(buf.String(), "unreadable run dir") {
		t.Fatalf("unreadable dirs must be surfaced in the output, got %q", buf.String())
	}
}

// TestRunPrune_ReclaimsOrphanedWorkspaceObjects is Revi's R1eeed3.
//
// PruneObjects had no production caller, so the store-global pool grew
// for the life of the store — and `runs prune` made it strictly worse:
// deleting a run removed the manifests that referenced its content
// without reclaiming a byte, orphaning them permanently.
func TestRunPrune_ReclaimsOrphanedWorkspaceObjects(t *testing.T) {
	storeDir := t.TempDir()
	st, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ctx := context.Background()

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "produced.txt"), []byte("a node's output"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	tr := workspacetrack.NewNative(storeDir)

	old := time.Now().Add(-48 * time.Hour)
	r := &store.Run{ID: "old-run", Status: store.RunStatusFinished, CreatedAt: old, UpdatedAt: old}
	if err := st.SaveRun(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	if _, err := tr.Capture(r.ID, ws, "post:implement:0"); err != nil {
		t.Fatalf("capture: %v", err)
	}

	pool := filepath.Join(storeDir, "workspace-objects")
	before := countFiles(t, pool)
	if before == 0 {
		t.Fatal("fixture: the capture stored no objects")
	}

	p := &Printer{W: &bytes.Buffer{}, Format: OutputHuman}

	// Fresh objects are inside the grace window that protects a capture
	// in flight, so the sweep must spare them even though no manifest
	// survives — that guard is what makes a cron-scheduled prune safe.
	if err := RunPrune(PruneOptions{StoreDir: storeDir, OlderThan: time.Hour}, p); err != nil {
		t.Fatalf("prune (fresh): %v", err)
	}
	if countFiles(t, pool) != before {
		t.Errorf("the sweep deleted objects written moments ago — a capture whose manifest " +
			"has not landed yet would lose content it is still using")
	}

	// Age them past the grace window; now they are genuinely orphaned.
	aged := time.Now().Add(-2 * pruneObjectGrace)
	_ = filepath.WalkDir(pool, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort backdating
		}
		return os.Chtimes(path, aged, aged)
	})

	if err := RunPrune(PruneOptions{StoreDir: storeDir, OlderThan: time.Hour}, p); err != nil {
		t.Fatalf("prune (aged): %v", err)
	}
	if after := countFiles(t, pool); after != 0 {
		t.Errorf("%d object(s) survived a prune of the only run referencing them — "+
			"the pool is unreachable and never reclaimed", after)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // a missing pool counts as zero
		}
		n++
		return nil
	})
	return n
}
