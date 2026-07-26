package dispatcher

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testWorkspaceRunID = "run-test"

func createTestWorkspace(w *Workspaces, issueID string) (string, bool, error) {
	return w.Create(issueID)
}

func testWorkspacePath(w *Workspaces, issueID string) string {
	return w.Path(issueID)
}

func TestWorkspaceCreateAndPath(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	path, created, err := createTestWorkspace(w, "native:abc-123")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created {
		t.Fatal("first create should report created=true")
	}
	if !strings.HasPrefix(path, w.Root()) {
		t.Fatalf("path %q should be under root %q", path, w.Root())
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("workspace not a directory: %v %v", info, err)
	}

	// Idempotent: re-create reports created=false.
	_, created2, err := createTestWorkspace(w, "native:abc-123")
	if err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	if created2 {
		t.Fatal("second create should report created=false")
	}
}

func TestWorkspaceSanitize(t *testing.T) {
	w, _ := NewWorkspaces(t.TempDir())
	path, _, err := createTestWorkspace(w, "github:owner/repo#42")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	base := filepath.Base(path)
	if strings.ContainsAny(base, ":/#") {
		t.Fatalf("sanitized base still contains hostile chars: %q", base)
	}
}

func TestWorkspaceKeysSeparateSanitizationCollisions(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	firstID := "github:owner/repo#42"
	secondID := "github/owner:repo#42"
	if sanitizeKey(firstID) != sanitizeKey(secondID) {
		t.Fatal("test fixture does not exercise a legacy sanitization collision")
	}
	if first, second := testWorkspacePath(w, firstID), testWorkspacePath(w, secondID); first == second {
		t.Fatalf("colliding issue IDs resolved to the same workspace: %q", first)
	}

	first, firstCreated, err := createTestWorkspace(w, firstID)
	if err != nil {
		t.Fatalf("Create(%q): %v", firstID, err)
	}
	second, secondCreated, err := createTestWorkspace(w, secondID)
	if err != nil {
		t.Fatalf("Create(%q): %v", secondID, err)
	}
	if !firstCreated || !secondCreated {
		t.Fatalf("each issue must own a fresh workspace; created=(%t, %t)", firstCreated, secondCreated)
	}
	if first == second {
		t.Fatalf("colliding issue IDs shared the created workspace %q", first)
	}
}

func TestWorkspaceRunGenerationsNeverReuseAbsolutePath(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:reopened-ticket"
	firstRun := "run-first"
	nextRun := "run-next"

	firstPath, created, err := w.CreateForRun(issueID, firstRun)
	if err != nil || !created {
		t.Fatalf("CreateForRun(first) = (%q, %t, %v)", firstPath, created, err)
	}
	if err := w.RetireForRun(issueID, firstRun); err != nil {
		t.Fatalf("RetireForRun(first): %v", err)
	}
	recovery := firstPath + ".recovery"
	if err := os.Rename(firstPath, recovery); err != nil {
		t.Fatalf("quarantine first generation: %v", err)
	}

	nextPath, created, err := w.CreateForRun(issueID, nextRun)
	if err != nil || !created {
		t.Fatalf("CreateForRun(next) = (%q, %t, %v)", nextPath, created, err)
	}
	if nextPath == firstPath {
		t.Fatalf("new run reused retired absolute path %q", firstPath)
	}

	// An old process may wake after the next dispatch has started and recreate
	// the only absolute path it knows. Its output must stay isolated from the
	// next generation.
	if err := os.MkdirAll(firstPath, 0o755); err != nil {
		t.Fatalf("late writer recreate first path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(firstPath, "late-output"), []byte("old run"), 0o644); err != nil {
		t.Fatalf("late writer output: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nextPath, "late-output")); !os.IsNotExist(err) {
		t.Fatalf("late output contaminated next generation: %v", err)
	}
	if _, _, err := w.CreateForRun(issueID, firstRun); err == nil {
		t.Fatal("retired first generation became authoritative again")
	}
}

func TestDispatchWorkspaceLifecycleFollowsPersistPolicy(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	d := &Dispatcher{workspaces: w}
	const runID = "run-policy"
	tests := []struct {
		name       string
		persist    WorkspacePersistPolicy
		generation string
		cleanup    bool
	}{
		{name: "keep", persist: WorkspacePersistKeep},
		{name: "cleanup_on_done", persist: WorkspacePersistCleanupOnDone, generation: runID, cleanup: true},
		{name: "cleanup_on_terminal", persist: WorkspacePersistCleanupOnTerminal, generation: runID, cleanup: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			generation, cleanup, err := d.dispatchWorkspaceLifecycle("native:policy", tc.persist, runID, false)
			if err != nil {
				t.Fatalf("dispatchWorkspaceLifecycle(%q): %v", tc.persist, err)
			}
			if generation != tc.generation || cleanup != tc.cleanup {
				t.Fatalf(
					"dispatchWorkspaceLifecycle(%q) = (%q, %t), want (%q, %t)",
					tc.persist, generation, cleanup, tc.generation, tc.cleanup,
				)
			}
		})
	}
}

func TestDispatchWorkspaceLifecyclePreservesResumeShapeAcrossPolicyChange(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	d := &Dispatcher{workspaces: w}

	stableIssue := "native:resume-stable"
	if _, _, err := w.Create(stableIssue); err != nil {
		t.Fatalf("Create(stable): %v", err)
	}
	if generation, cleanup, err := d.dispatchWorkspaceLifecycle(
		stableIssue,
		WorkspacePersistCleanupOnDone,
		"run-stable",
		true,
	); err != nil || generation != "" || !cleanup {
		t.Fatalf("stable resume after keep→cleanup = (%q, %t, %v), want (empty, true, nil)", generation, cleanup, err)
	}

	runIssue := "native:resume-run"
	const runID = "run-generation"
	if _, _, err := w.CreateForRun(runIssue, runID); err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}
	if generation, cleanup, err := d.dispatchWorkspaceLifecycle(
		runIssue,
		WorkspacePersistKeep,
		runID,
		true,
	); err != nil || generation != runID || cleanup {
		t.Fatalf("run resume after cleanup→keep = (%q, %t, %v), want (%q, false, nil)", generation, cleanup, err, runID)
	}
}

func TestWorkspaceResumeGenerationDoesNotAdoptLegacyPath(t *testing.T) {
	root := t.TempDir()
	w, err := NewWorkspaces(root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:legacy-resume"
	legacyPath := filepath.Join(root, sanitizeKey(issueID))
	if err := os.MkdirAll(legacyPath, 0o755); err != nil {
		t.Fatalf("mkdir legacy workspace: %v", err)
	}

	if generation, ok, err := w.resumeGeneration(issueID, "run-legacy"); err != nil || ok {
		t.Fatalf("legacy unowned path probe = (%q, %t, %v), want unmanaged without error", generation, ok, err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy workspace changed during ownership probe: %v", err)
	}
}

func TestWorkspaceResumeGenerationRejectsInvalidRunShapeBeforeStableFallback(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:invalid-resume-shape"
	if _, _, err := w.Create(issueID); err != nil {
		t.Fatalf("Create(stable): %v", err)
	}
	runID := "run-invalid"
	if err := os.MkdirAll(w.PathForRun(issueID, runID), 0o755); err != nil {
		t.Fatalf("mkdir unowned run target: %v", err)
	}

	generation, ok, err := w.resumeGeneration(issueID, runID)
	if err != nil || ok || generation != runID {
		t.Fatalf("invalid run shape fell back to stable workspace: generation=%q managed=%t err=%v", generation, ok, err)
	}

	retiredIssue := "native:retired-resume-shape"
	retiredRun := "run-retired"
	if _, _, err := w.CreateForRun(retiredIssue, retiredRun); err != nil {
		t.Fatalf("CreateForRun(retired): %v", err)
	}
	if err := w.RetireForRun(retiredIssue, retiredRun); err != nil {
		t.Fatalf("RetireForRun: %v", err)
	}
	if generation, ok, err := w.resumeGeneration(retiredIssue, retiredRun); err != nil || ok || generation != retiredRun {
		t.Fatalf("retired run shape probe = (%q, %t, %v), want unmanaged without error", generation, ok, err)
	}

	corruptIssue := "native:corrupt-resume-shape"
	corruptRun := "run-corrupt"
	if _, _, err := w.CreateForRun(corruptIssue, corruptRun); err != nil {
		t.Fatalf("CreateForRun(corrupt): %v", err)
	}
	if err := os.WriteFile(w.ownerPathForRun(corruptIssue, corruptRun), []byte("{broken"), 0o600); err != nil {
		t.Fatalf("corrupt marker: %v", err)
	}
	if generation, ok, err := w.resumeGeneration(corruptIssue, corruptRun); err != nil || ok || generation != corruptRun {
		t.Fatalf("corrupt run shape probe = (%q, %t, %v), want unmanaged without error", generation, ok, err)
	}
}

func TestWorkspaceGenerationProbePropagatesFilesystemError(t *testing.T) {
	root := t.TempDir()
	w, err := NewWorkspaces(root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	namespace := filepath.Join(root, workspaceKeyNamespace)
	if err := os.MkdirAll(namespace, 0o755); err != nil {
		t.Fatalf("mkdir namespace: %v", err)
	}
	// A regular file where .owners must be a directory makes Lstat on an
	// owner marker fail with a deterministic non-ENOENT error (typically
	// ENOTDIR). Unlike chmod-based EACCES fixtures, this works under root
	// and on platforms whose permission model differs.
	if err := os.WriteFile(filepath.Join(namespace, workspaceOwnersDir), []byte("broken"), 0o600); err != nil {
		t.Fatalf("plant invalid ownership namespace: %v", err)
	}

	const (
		issueID = "native:probe-io-error"
		runID   = "run-probe-io-error"
	)
	generation, managed, err := w.resumeGeneration(issueID, runID)
	if err == nil {
		t.Fatalf("resumeGeneration = (%q, %t, nil), want filesystem error", generation, managed)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resumeGeneration misclassified filesystem error as absent: %v", err)
	}
	if generation != runID || managed {
		t.Fatalf("resumeGeneration = (%q, %t, %v), want (%q, false, error)", generation, managed, err, runID)
	}

	d := &Dispatcher{workspaces: w}
	if _, _, err := d.dispatchWorkspaceLifecycle(issueID, WorkspacePersistKeep, runID, false); err == nil {
		t.Fatal("dispatchWorkspaceLifecycle swallowed ownership probe filesystem error")
	}
}

func TestDispatchWorkspaceLifecycleIsolatesPoisonedStableKeepPath(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:poisoned-stable"
	if err := os.MkdirAll(w.Path(issueID), 0o755); err != nil {
		t.Fatalf("mkdir unowned stable target: %v", err)
	}
	d := &Dispatcher{workspaces: w}
	const runID = "run-isolated"

	generation, cleanup, err := d.dispatchWorkspaceLifecycle(
		issueID,
		WorkspacePersistKeep,
		runID,
		false,
	)
	if err != nil || generation != runID || cleanup {
		t.Fatalf("poisoned keep lifecycle = (%q, %t, %v), want (%q, false, nil)", generation, cleanup, err, runID)
	}
}

func TestWorkspaceConcurrentCreateHasSingleAuthority(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}

	const callers = 32
	type result struct {
		path    string
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			ready.Done()
			<-start
			path, created, err := createTestWorkspace(w, "native:concurrent-authority")
			results <- result{path: path, created: created, err: err}
		}()
	}
	ready.Wait()
	close(start)

	var (
		authorities int
		wantPath    string
	)
	for i := 0; i < callers; i++ {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent Create: %v", got.err)
		}
		if wantPath == "" {
			wantPath = got.path
		} else if got.path != wantPath {
			t.Fatalf("same issue resolved to different paths: %q and %q", wantPath, got.path)
		}
		if got.created {
			authorities++
		}
	}
	if authorities != 1 {
		t.Fatalf("created=true authorities = %d, want exactly 1", authorities)
	}
}

func TestCreateWorkspaceOwnerRemovesMarkerOnPublishFailure(t *testing.T) {
	injectedErr := errors.New("injected owner publication failure")
	owner := workspaceOwnerRecord{
		FormatVersion: 1,
		IssueID:       "native:owner-publish",
		State:         workspaceOwnerActive,
	}

	tests := []struct {
		name     string
		writeErr error
		syncErr  error
		closeErr error
	}{
		{name: "write", writeErr: injectedErr},
		{name: "sync", syncErr: injectedErr},
		{name: "close", closeErr: injectedErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			workspace := filepath.Join(dir, "workspace")
			if err := os.Mkdir(workspace, 0o755); err != nil {
				t.Fatalf("mkdir workspace: %v", err)
			}
			sentinel := filepath.Join(workspace, "recoverable-output")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0o644); err != nil {
				t.Fatalf("write workspace sentinel: %v", err)
			}
			marker := filepath.Join(dir, "owner.json")

			err := createWorkspaceOwnerWithOpener(marker, owner, func(name string, flag int, perm os.FileMode) (workspaceOwnerFile, error) {
				f, err := os.OpenFile(name, flag, perm)
				if err != nil {
					return nil, err
				}
				return &failingWorkspaceOwnerFile{
					File:     f,
					writeErr: tc.writeErr,
					syncErr:  tc.syncErr,
					closeErr: tc.closeErr,
				}, nil
			})
			if !errors.Is(err, injectedErr) {
				t.Fatalf("createWorkspaceOwnerWithOpener error = %v, want injected failure", err)
			}
			if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed publication left ownership marker behind: %v", err)
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
				t.Fatalf("workspace changed while rolling back marker: data=%q err=%v", got, err)
			}

			// Once the failed marker has been removed, publishing it again is
			// recoverable without deleting the workspace.
			if err := createWorkspaceOwner(marker, owner); err != nil {
				t.Fatalf("retry createWorkspaceOwner: %v", err)
			}
			got, err := readWorkspaceOwner(marker)
			if err != nil {
				t.Fatalf("read owner after retry: %v", err)
			}
			if err := verifyWorkspaceOwner(got, owner.IssueID, owner.RunID, workspaceOwnerActive); err != nil {
				t.Fatalf("owner after retry: %v", err)
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
				t.Fatalf("workspace changed after marker retry: data=%q err=%v", got, err)
			}
		})
	}
}

type failingWorkspaceOwnerFile struct {
	*os.File
	writeErr error
	syncErr  error
	closeErr error
}

func (f *failingWorkspaceOwnerFile) Write(p []byte) (int, error) {
	if f.writeErr == nil {
		return f.File.Write(p)
	}
	// Leave a realistic truncated marker before reporting the write error.
	n, err := f.File.Write(p[:1])
	if err != nil {
		return n, err
	}
	return n, f.writeErr
}

func (f *failingWorkspaceOwnerFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *failingWorkspaceOwnerFile) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func TestWorkspaceLegacyCollisionPathIsPreservedNotAdopted(t *testing.T) {
	root := t.TempDir()
	w, err := NewWorkspaces(root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:legacy/42"
	legacyPath := filepath.Join(root, sanitizeKey(issueID))
	if err := os.MkdirAll(legacyPath, 0o755); err != nil {
		t.Fatalf("mkdir legacy workspace: %v", err)
	}
	sentinel := filepath.Join(legacyPath, "owner-data")
	if err := os.WriteFile(sentinel, []byte("must survive"), 0o644); err != nil {
		t.Fatalf("write legacy sentinel: %v", err)
	}

	newPath, created, err := createTestWorkspace(w, issueID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created {
		t.Fatal("legacy collision-prone path must not be adopted as the v2 workspace")
	}
	if newPath == legacyPath {
		t.Fatalf("v2 workspace unexpectedly reused legacy path %q", legacyPath)
	}
	if err := w.Remove(issueID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "must survive" {
		t.Fatalf("legacy workspace was changed during v2 create/remove: data=%q err=%v", got, err)
	}
}

func TestWorkspaceRefusesExistingUnownedV2Target(t *testing.T) {
	root := t.TempDir()
	w, err := NewWorkspaces(root)
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:late-writer"
	target := testWorkspacePath(w, issueID)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("plant unowned target: %v", err)
	}
	sentinel := filepath.Join(target, "late-output")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o644); err != nil {
		t.Fatalf("write late output: %v", err)
	}

	if _, _, err := createTestWorkspace(w, issueID); err == nil {
		t.Fatal("Create adopted an existing target without an ownership marker")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
		t.Fatalf("unowned target was mutated: data=%q err=%v", got, err)
	}
}

func TestWorkspaceRemoveRequiresRetirementAndIsIdempotent(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:retired"
	path, _, err := w.CreateForRun(issueID, testWorkspaceRunID)
	if err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}
	if err := w.RemoveForRun(issueID, testWorkspaceRunID); err == nil {
		t.Fatal("RemoveForRun deleted an active workspace")
	}
	if err := w.RetireForRun(issueID, testWorkspaceRunID); err != nil {
		t.Fatalf("RetireForRun: %v", err)
	}
	if _, _, err := w.CreateForRun(issueID, testWorkspaceRunID); err == nil {
		t.Fatal("CreateForRun reused a retired workspace")
	}
	if err := w.RemoveForRun(issueID, testWorkspaceRunID); err != nil {
		t.Fatalf("RemoveForRun: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed: %v", err)
	}
	if err := w.RemoveForRun(issueID, testWorkspaceRunID); err != nil {
		t.Fatalf("RemoveForRun(absent): %v", err)
	}
}

func TestWorkspaceInterruptedRemovalClearsRetiredMarker(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:interrupted-remove"
	oldRun := "run-old"
	path, _, err := w.CreateForRun(issueID, oldRun)
	if err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}
	if err := w.RetireForRun(issueID, oldRun); err != nil {
		t.Fatalf("RetireForRun: %v", err)
	}
	// Simulate a crash after directory removal but before marker removal.
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove target fixture: %v", err)
	}
	if err := w.RemoveForRun(issueID, oldRun); err != nil {
		t.Fatalf("RemoveForRun should finalize retired marker: %v", err)
	}

	nextPath, created, err := w.CreateForRun(issueID, "run-next")
	if err != nil || !created {
		t.Fatalf("CreateForRun(next) = (%q, %t, %v)", nextPath, created, err)
	}
	if nextPath == path {
		t.Fatalf("next generation reused interrupted path %q", path)
	}
}

func TestWorkspaceStableCreateRepairsInterruptedRetiredRemoval(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:stable-interrupted-remove"
	path, _, err := w.Create(issueID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.RetireForRun(issueID, ""); err != nil {
		t.Fatalf("RetireForRun(stable): %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove target fixture: %v", err)
	}

	recreated, created, err := w.Create(issueID)
	if err != nil || !created || recreated != path {
		t.Fatalf("Create after interrupted Remove = (%q, %t, %v), want (%q, true, nil)", recreated, created, err, path)
	}
}

func TestWorkspaceRetiredGenerationIsolatesLatePathRecreation(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:absolute-late-writer"
	oldRun := "run-old"
	path, _, err := w.CreateForRun(issueID, oldRun)
	if err != nil {
		t.Fatalf("CreateForRun(old): %v", err)
	}
	if err := w.RetireForRun(issueID, oldRun); err != nil {
		t.Fatalf("RetireForRun(old): %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove original target: %v", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("late writer recreate target: %v", err)
	}
	output := filepath.Join(path, "ignored-output")
	if err := os.WriteFile(output, []byte("late"), 0o644); err != nil {
		t.Fatalf("late writer output: %v", err)
	}

	if _, _, err := w.CreateForRun(issueID, oldRun); err == nil {
		t.Fatal("CreateForRun adopted a recreated path behind a retired tombstone")
	}
	nextPath, created, err := w.CreateForRun(issueID, "run-next")
	if err != nil || !created {
		t.Fatalf("CreateForRun(next) = (%q, %t, %v)", nextPath, created, err)
	}
	if _, err := os.Stat(filepath.Join(nextPath, "ignored-output")); !os.IsNotExist(err) {
		t.Fatalf("late output contaminated next generation: %v", err)
	}
	if got, err := os.ReadFile(output); err != nil || string(got) != "late" {
		t.Fatalf("late writer output was lost: data=%q err=%v", got, err)
	}
}

func TestWorkspaceActiveMarkerWithMissingTargetReportsRecoveryPath(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:orphan-active"
	path, _, err := w.CreateForRun(issueID, testWorkspaceRunID)
	if err != nil {
		t.Fatalf("CreateForRun: %v", err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove target fixture: %v", err)
	}
	ownerPath := w.ownerPathForRun(issueID, testWorkspaceRunID)

	err = w.RemoveForRun(issueID, testWorkspaceRunID)
	if err == nil {
		t.Fatal("RemoveForRun accepted an active marker with a missing target")
	}
	if !strings.Contains(err.Error(), ownerPath) || !strings.Contains(err.Error(), "manual recovery") {
		t.Fatalf("error %q does not identify recovery marker %q", err, ownerPath)
	}
}

func TestWorkspaceRejectsHiddenName(t *testing.T) {
	w, _ := NewWorkspaces(t.TempDir())
	path, _, err := createTestWorkspace(w, ".hidden")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if strings.HasPrefix(filepath.Base(path), ".") {
		t.Fatalf("workspace name should not be hidden: %q", filepath.Base(path))
	}
}

func TestWorkspaceRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // a separate directory
	w, _ := NewWorkspaces(root)

	// Pre-plant a symlink at the exact v2 target that points outside the root.
	id := "evil_link"
	planted := testWorkspacePath(w, id)
	if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
		t.Fatalf("mkdir namespace: %v", err)
	}
	if err := os.Symlink(outside, planted); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}

	_, _, err := createTestWorkspace(w, id)
	if err == nil {
		t.Fatal("expected symlink/unowned-target rejection")
	}
	if info, statErr := os.Lstat(planted); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("planted symlink was followed or replaced: info=%v err=%v", info, statErr)
	}
}

func TestWorkspaceRemove(t *testing.T) {
	w, _ := NewWorkspaces(t.TempDir())
	path, _, err := createTestWorkspace(w, "x")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.Remove("x"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed: %v", err)
	}
	// idempotent
	if err := w.Remove("x"); err != nil {
		t.Fatalf("Remove (absent): %v", err)
	}
}

func TestWorkspaceRemoveAbsentIsIdempotent(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	const issueID = "native:never-created"

	// The compatibility API intentionally treats a fully absent stable
	// workspace as already removed, even though RetireForRun cannot observe
	// an active→retired transition.
	if err := w.Remove(issueID); err != nil {
		t.Fatalf("Remove(absent): %v", err)
	}
	if _, err := os.Lstat(w.Path(issueID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent target changed: %v", err)
	}
	if _, err := os.Lstat(w.ownerPathForRun(issueID, "")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent ownership marker changed: %v", err)
	}
}
