package dispatcher

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestWorkspaceCreateAndPath(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	path, created, err := w.Create("native:abc-123")
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
	_, created2, err := w.Create("native:abc-123")
	if err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	if created2 {
		t.Fatal("second create should report created=false")
	}
}

func TestWorkspaceSanitize(t *testing.T) {
	w, _ := NewWorkspaces(t.TempDir())
	path, _, err := w.Create("github:owner/repo#42")
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
	if first, second := w.Path(firstID), w.Path(secondID); first == second {
		t.Fatalf("colliding issue IDs resolved to the same workspace: %q", first)
	}

	first, firstCreated, err := w.Create(firstID)
	if err != nil {
		t.Fatalf("Create(%q): %v", firstID, err)
	}
	second, secondCreated, err := w.Create(secondID)
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
			path, created, err := w.Create("native:concurrent-authority")
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

	newPath, created, err := w.Create(issueID)
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
	target := w.Path(issueID)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("plant unowned target: %v", err)
	}
	sentinel := filepath.Join(target, "late-output")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o644); err != nil {
		t.Fatalf("write late output: %v", err)
	}

	if _, _, err := w.Create(issueID); err == nil {
		t.Fatal("Create adopted an existing target without an ownership marker")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
		t.Fatalf("unowned target was mutated: data=%q err=%v", got, err)
	}
}

func TestWorkspaceRetireBlocksReuseUntilAbsentAndReleased(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:retired"
	path, _, err := w.Create(issueID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "preserved-output"), []byte("kept"), 0o644); err != nil {
		t.Fatalf("write preserved output: %v", err)
	}
	if err := w.Retire(issueID); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if _, _, err := w.Create(issueID); err == nil {
		t.Fatal("Create reused a retired workspace")
	}
	if err := w.Release(issueID); err == nil {
		t.Fatal("Release succeeded while the retired target still existed")
	}

	recovery := path + ".recovery"
	if err := os.Rename(path, recovery); err != nil {
		t.Fatalf("quarantine fixture: %v", err)
	}
	if err := w.Release(issueID); err != nil {
		t.Fatalf("Release after quarantine: %v", err)
	}
	fresh, created, err := w.Create(issueID)
	if err != nil {
		t.Fatalf("Create after release: %v", err)
	}
	if !created || fresh != path {
		t.Fatalf("fresh workspace = (%q, created=%t), want (%q, true)", fresh, created, path)
	}
	if got, err := os.ReadFile(filepath.Join(recovery, "preserved-output")); err != nil || string(got) != "kept" {
		t.Fatalf("quarantined output was not preserved: data=%q err=%v", got, err)
	}
}

func TestWorkspaceRetiredMarkerBlocksLatePathRecreation(t *testing.T) {
	w, err := NewWorkspaces(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspaces: %v", err)
	}
	issueID := "native:absolute-late-writer"
	path, _, err := w.Create(issueID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := w.Retire(issueID); err != nil {
		t.Fatalf("Retire: %v", err)
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

	if err := w.Release(issueID); err == nil {
		t.Fatal("Release removed the tombstone despite a recreated path")
	}
	if _, _, err := w.Create(issueID); err == nil {
		t.Fatal("Create adopted a recreated path behind a retired tombstone")
	}
	if got, err := os.ReadFile(output); err != nil || string(got) != "late" {
		t.Fatalf("late writer output was lost: data=%q err=%v", got, err)
	}
}

func TestWorkspaceRejectsHiddenName(t *testing.T) {
	w, _ := NewWorkspaces(t.TempDir())
	path, _, err := w.Create(".hidden")
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
	planted := w.Path(id)
	if err := os.MkdirAll(filepath.Dir(planted), 0o755); err != nil {
		t.Fatalf("mkdir namespace: %v", err)
	}
	if err := os.Symlink(outside, planted); err != nil {
		t.Skipf("symlink unsupported on this fs: %v", err)
	}

	_, _, err := w.Create(id)
	if err == nil {
		t.Fatal("expected symlink/unowned-target rejection")
	}
	if info, statErr := os.Lstat(planted); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("planted symlink was followed or replaced: info=%v err=%v", info, statErr)
	}
}

func TestWorkspaceRemove(t *testing.T) {
	w, _ := NewWorkspaces(t.TempDir())
	path, _, err := w.Create("x")
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
