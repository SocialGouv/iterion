package server

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func authoringLocalFixture(t *testing.T, operation string) (*authoringLocalTransaction, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.txt")
	preview := authoringPreviewFile{Scope: "workspace", Path: "file.txt", Operation: operation, After: "candidate\n"}
	if operation != "create" {
		preview.Before = "original\n"
		if err := os.WriteFile(path, []byte(preview.Before), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeAuthoringLocalLocks(locks) })
	tx, err := prepareAuthoringLocal(locks[0], preview, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tx.close)
	return tx, path
}
func authoringBytes(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("%s = %q, want %q: %v", path, b, want, err)
	}
}
func authoringExternalReplace(t *testing.T, path, body string) {
	t.Helper()
	temp := filepath.Join(filepath.Dir(path), "external-new")
	if err := os.WriteFile(temp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
}
func TestAuthoringLocalPreservesReplacementAtDisplacement(t *testing.T) {
	tx, path := authoringLocalFixture(t, "replace")
	tx.beforeStep = func(stage string) error {
		if stage == "displace" {
			authoringExternalReplace(t, path, "newer editor text\n")
		}
		return nil
	}
	err := tx.publish()
	var conflict authoringConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	authoringBytes(t, path, "newer editor text\n")
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	authoringBytes(t, path, "newer editor text\n")
}
func TestAuthoringLocalNoReplaceLetsInterveningDestinationWin(t *testing.T) {
	for _, operation := range []string{"replace", "create"} {
		t.Run(operation, func(t *testing.T) {
			tx, path := authoringLocalFixture(t, operation)
			tx.beforeStep = func(stage string) error {
				if stage == "publish" {
					if err := os.WriteFile(path, []byte("intervening\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			if err := tx.publish(); err == nil {
				t.Fatal("publication overwrote a new destination")
			}
			_ = tx.rollback()
			authoringBytes(t, path, "intervening\n")
			if operation == "replace" {
				authoringBytes(t, filepath.Join(tx.lock.control.path, tx.name("before")), "original\n")
			}
		})
	}
}
func TestAuthoringLocalRetainsOldDescriptorWritesAfterSuccess(t *testing.T) {
	tx, path := authoringLocalFixture(t, "replace")
	old, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	initial, err := old.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.publish(); err != nil {
		t.Fatal(err)
	}
	if _, err := old.WriteAt([]byte("late edit"), 0); err != nil {
		t.Fatal(err)
	}
	retained := tx.recovery().Files
	if len(retained) != 1 {
		t.Fatalf("missing recovery location: %+v", tx.recovery())
	}
	authoringBytes(t, retained[0], "late edit")
	authoringBytes(t, path, "candidate\n")
	info, err := os.Stat(retained[0])
	if err != nil || !os.SameFile(initial, info) {
		t.Fatalf("old inode was not retained: %v", err)
	}
	live, err := os.Stat(path)
	if err != nil || live.Mode().Perm() != initial.Mode().Perm() {
		t.Fatalf("replacement mode changed: %v %v", live, err)
	}
	// Rollback restores that exact original inode, including the late write.
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	authoringBytes(t, path, "late edit")
	restored, _ := os.Stat(path)
	if !os.SameFile(initial, restored) {
		t.Fatal("rollback reconstructed bytes instead of restoring the original inode")
	}
}
func TestAuthoringLocalRollbackPreservesInterference(t *testing.T) {
	for _, operation := range []string{"replace", "create"} {
		for _, point := range []string{"undo-displace", "after-displace", "in-place"} {
			t.Run(operation+"/"+point, func(t *testing.T) {
				tx, path := authoringLocalFixture(t, operation)
				if err := tx.publish(); err != nil {
					t.Fatal(err)
				}
				if point == "in-place" {
					if err := os.WriteFile(path, []byte("foreign edit\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				tx.beforeStep = func(stage string) error {
					if point == "undo-displace" && stage == "undo-displace" {
						authoringExternalReplace(t, path, "foreign edit\n")
					}
					if point == "after-displace" && stage == "record:undo-displaced" {
						if err := os.WriteFile(path, []byte("foreign edit\n"), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					return nil
				}
				if err := tx.rollback(); err == nil {
					t.Fatal("interference reported as a clean rollback")
				}
				authoringBytes(t, path, "foreign edit\n")
				if operation == "replace" {
					authoringBytes(t, filepath.Join(tx.lock.control.path, tx.name("before")), "original\n")
				}
			})
		}
	}
}
func TestAuthoringLocalRollbackDoesNotTrustMatchingForeignBytes(t *testing.T) {
	tx, path := authoringLocalFixture(t, "create")
	if err := tx.publish(); err != nil {
		t.Fatal(err)
	}
	authoringExternalReplace(t, path, tx.preview.After)
	if err := tx.rollback(); err == nil {
		t.Fatal("rollback removed a foreign inode with matching bytes")
	}
	authoringBytes(t, path, tx.preview.After)
}
func TestAuthoringLocalRetainsDescriptorToRolledBackCreate(t *testing.T) {
	tx, path := authoringLocalFixture(t, "create")
	if err := tx.publish(); err != nil {
		t.Fatal(err)
	}
	candidate, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	if err := tx.rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("creation rollback left live file: %v", err)
	}
	if _, err := candidate.WriteAt([]byte("late write"), 0); err != nil {
		t.Fatal(err)
	}
	files := tx.recovery().Files
	if len(files) != 1 {
		t.Fatalf("rollback inode missing: %+v", files)
	}
	authoringBytes(t, files[0], "late write")
}
func TestAuthoringLocalFailureStagesRetainOriginal(t *testing.T) {
	for _, point := range []string{"record:preparing", "prepare", "record:prepared", "record:displacing", "displace", "record:displaced", "record:publishing", "publish", "record:published"} {
		t.Run(point, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
				t.Fatal(err)
			}
			locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
			if err != nil {
				t.Fatal(err)
			}
			defer closeAuthoringLocalLocks(locks)
			injected := errors.New("injected " + point)
			fired := false
			hook := func(stage string) error {
				if !fired && stage == point {
					fired = true
					return injected
				}
				return nil
			}
			tx, err := prepareAuthoringLocal(locks[0], authoringPreviewFile{Operation: "replace", Before: "before", After: "after"}, nil, hook)
			if tx != nil {
				defer tx.close()
			}
			if err == nil {
				err = tx.publish()
			}
			if !errors.Is(err, injected) {
				t.Fatalf("stage did not fail: %v", err)
			}
			if tx != nil {
				if err := tx.rollback(); err != nil {
					t.Fatal(err)
				}
			}
			authoringBytes(t, path, "before")
		})
	}
}
func TestAuthoringLocalRestoreFailureReportsRetainedPaths(t *testing.T) {
	tx, path := authoringLocalFixture(t, "replace")
	if err := tx.publish(); err != nil {
		t.Fatal(err)
	}
	tx.beforeStep = func(stage string) error {
		if stage == "restore" {
			return errors.New("restore refused")
		}
		return nil
	}
	if err := tx.rollback(); err == nil {
		t.Fatal("restore error disappeared")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected live path: %v", err)
	}
	recovery := tx.recovery()
	if len(recovery.Files) != 2 {
		t.Fatalf("retained locations=%+v", recovery)
	}
	authoringBytes(t, recovery.Files[0], "original\n")
	authoringBytes(t, recovery.Files[1], "candidate\n")
	if b, err := os.ReadFile(recovery.Record); err != nil || !strings.Contains(string(b), "restoring-original") {
		t.Fatalf("recovery record missing stage: %s %v", b, err)
	}
}
func TestAuthoringLocalParentReplacementCannotFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows directory handles prevent the parent rename used by this POSIX interference fixture")
	}
	tx, path := authoringLocalFixture(t, "replace")
	foreign := t.TempDir()
	foreignFile := filepath.Join(foreign, filepath.Base(path))
	if err := os.WriteFile(foreignFile, []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(path)
	moved := parent + "-moved"
	tx.beforeStep = func(stage string) error {
		if stage == "displace" {
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(foreign, parent); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	t.Cleanup(func() { _ = os.Remove(parent); _ = os.Rename(moved, parent) })
	if err := tx.publish(); err == nil {
		t.Fatal("parent replacement accepted")
	}
	authoringBytes(t, foreignFile, "foreign")
	authoringBytes(t, filepath.Join(moved, filepath.Base(path)), "original\n")
}
func TestAuthoringLocalInterruptedRecordsAreNotReplayed(t *testing.T) {
	tx, path := authoringLocalFixture(t, "replace")
	tx.beforeStep = func(stage string) error {
		if stage == "publish" {
			return errors.New("interrupted before publication")
		}
		return nil
	}
	if err := tx.publish(); err == nil {
		t.Fatal("missing interruption")
	}
	record := tx.recovery().Record
	original := tx.recovery().Files[0]
	journal, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	tx.close()
	tx.journal = nil
	// Release the lock as a terminated process would. Opening another request
	// must neither replay nor discard the interrupted transaction.
	if err := tx.lock.flock.Unlock(); err != nil {
		t.Fatal(err)
	}
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	authoringBytes(t, record, string(journal))
	authoringBytes(t, original, "original\n")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted operation replayed: %v", err)
	}
}
func TestAuthoringLocalCrossProcessLock(t *testing.T) {
	if path := os.Getenv("ITERION_AUTHORING_LOCK_CHILD"); path != "" {
		fmt.Println("ready")
		// The two child budgets carry DIFFERENT properties:
		// "blocked" — the acquire must NOT succeed within its bound. The
		//   parent holds the flock throughout; budget exhaustion IS the
		//   expected pass, so a tight budget cannot redden this branch and
		//   widening it does not close any flake. Keep the historical
		//   200 ms window.
		// "acquired" — the lock is FREE and must be acquired. This is a
		//   liveness property, not a latency one; a wall-clock ceiling
		//   here is a race between "acquires" and "child startup finished
		//   before 200 ms of clock elapsed", not a property of the code.
		//   Under -race on the shared runner that race dequeued #1436 with
		//   no reason written on the timeline. 30 s is the same order as
		//   the outer test's implicit test-timeout and makes runner
		//   slowdown irrelevant.
		budget := 30 * time.Second
		if os.Getenv("ITERION_AUTHORING_LOCK_EXPECT") == "blocked" {
			budget = 200 * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		locks, err := acquireAuthoringLocalLocks(ctx, []string{path})
		if os.Getenv("ITERION_AUTHORING_LOCK_EXPECT") == "blocked" {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("child acquired held lock: %v", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			closeAuthoringLocalLocks(locks)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "file.txt")
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	runChild := func(expect string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAuthoringLocalCrossProcessLock$")
		cmd.Env = append(os.Environ(), "ITERION_AUTHORING_LOCK_CHILD="+path, "ITERION_AUTHORING_LOCK_EXPECT="+expect)
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(pipe)
		if !scanner.Scan() || scanner.Text() != "ready" {
			t.Fatal("child did not start")
		}
		for scanner.Scan() {
			stderr.WriteString(scanner.Text())
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("subprocess: %v %s", err, stderr.String())
		}
	}
	runChild("blocked")
	if err := locks[0].flock.Unlock(); err != nil {
		t.Fatal(err)
	}
	runChild("acquired")
}
func TestAuthoringLocalPhysicalAliasesShareLock(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	path := filepath.Join(nested, "file.bot")
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	// A NEGATIVE window: the parent holds the flock throughout, so budget
	// exhaustion IS the expected pass and widening it does not close any
	// flake — a mutation where aliases DO NOT share the physical lock
	// makes the acquire succeed with err == nil regardless of window
	// size. Keep the historical 50 ms.
	for _, other := range []string{filepath.Join(alias, "nested", "file.bot"), filepath.Join(root, "nested", "file.bot")} {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		got, err := acquireAuthoringLocalLocks(ctx, []string{other})
		cancel()
		if got != nil {
			closeAuthoringLocalLocks(got)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("alias escaped physical lock: %s: %v", other, err)
		}
	}
}

func TestAuthoringLocalControlExcludedFromGitBundlesAndDiscovery(t *testing.T) {
	repo := gittest.SourceRepo(t)
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprintf("linked-worktree=%v", linked), func(t *testing.T) {
			root := repo
			if linked {
				root = filepath.Join(t.TempDir(), "run-worktree")
				gittest.Run(t, repo, "worktree", "add", "--detach", root, "HEAD")
				t.Cleanup(func() { gittest.RemoveWorktree(t, repo, root) })
			}
			botDir := filepath.Join(root, "nested", "bot")
			if err := os.MkdirAll(botDir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(botDir, "main.bot")
			before := "workflow original:\n  entry: done\n"
			after := "workflow changed:\n  entry: done\n"
			if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(botDir, "manifest.yaml"), []byte("name: preserved-bot\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			ignore := filepath.Join(root, ".gitignore")
			originalIgnore := "# operator settings\n*.scratch\n"
			if err := os.WriteFile(ignore, []byte(originalIgnore), 0o644); err != nil {
				t.Fatal(err)
			}
			opts := botregistry.ListOptions{Paths: []string{root}}
			initial, _, err := botregistry.ListWithDiagnostics(opts)
			if err != nil {
				t.Fatal(err)
			}
			locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
			if err != nil {
				t.Fatal(err)
			}
			defer closeAuthoringLocalLocks(locks)
			tx, err := prepareAuthoringLocal(locks[0], authoringPreviewFile{Operation: "replace", Before: before, After: after}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.close()
			if err := tx.publish(); err != nil {
				t.Fatal(err)
			}
			for _, rollback := range []bool{false, true} {
				if rollback {
					if err := tx.rollback(); err != nil {
						t.Fatal(err)
					}
				}
				gittest.Run(t, root, "add", "-A")
				index := gittest.Run(t, root, "ls-files")
				status := gittest.Run(t, root, "status", "--porcelain", "--untracked-files=all")
				if strings.Contains(index, ".iterion") || strings.Contains(status, ".iterion") {
					t.Fatalf("control leaked to Git:\n%s\n%s", index, status)
				}
				entries, _, err := botregistry.ListWithDiagnostics(opts)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(initial, entries) {
					t.Fatalf("recovery appeared in discovery: before=%+v after=%+v", initial, entries)
				}
				archive := filepath.Join(t.TempDir(), "packed.botz")
				if _, err := bundle.PackDir(botDir, archive); err != nil {
					t.Fatal(err)
				}
				z, err := zip.OpenReader(archive)
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range z.File {
					if strings.Contains(file.Name, ".iterion") {
						t.Fatalf("recovery packed: %s", file.Name)
					}
				}
				_ = z.Close()
				snapshot := bundle.Snapshot{Root: "bot"}
				if err := snapshot.AddDir("bot", botDir); err != nil {
					t.Fatal(err)
				}
				for name := range snapshot.Files {
					if strings.Contains(name, ".iterion") {
						t.Fatalf("recovery snapshotted: %s", name)
					}
				}
				materialized, cleanup, err := snapshot.Materialize()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(cleanup)
				if err := filepath.WalkDir(materialized, func(p string, d os.DirEntry, err error) error {
					if d != nil && d.Name() == ".iterion" {
						t.Fatalf("recovery materialized: %s", p)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			authoringBytes(t, ignore, originalIgnore)
		})
	}
}
func TestAuthoringLocalRefusesUnsafeControlLayout(t *testing.T) {
	for _, kind := range []string{"symlink", "unexpected", "wrong-ignore", "tracked"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if kind == "tracked" {
				root = gittest.SourceRepo(t)
			}
			path := filepath.Join(root, "file.txt")
			if err := os.WriteFile(path, []byte("untouched"), 0o644); err != nil {
				t.Fatal(err)
			}
			control := filepath.Join(root, ".iterion", "authoring")
			if err := os.MkdirAll(filepath.Dir(control), 0o700); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), control); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			} else {
				if err := os.Mkdir(control, 0o700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "unexpected":
					if err := os.WriteFile(filepath.Join(control, "main.bot"), []byte("user data"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "wrong-ignore":
					if err := os.WriteFile(filepath.Join(control, ".gitignore"), []byte("user rule\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "tracked":
					if err := os.WriteFile(filepath.Join(control, ".gitignore"), []byte("*\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					gittest.Run(t, root, "add", "-f", ".iterion/authoring/.gitignore")
				}
			}
			locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
			if locks != nil {
				closeAuthoringLocalLocks(locks)
			}
			if err == nil {
				t.Fatal("unsafe control accepted")
			}
			authoringBytes(t, path, "untouched")
			if kind == "wrong-ignore" {
				authoringBytes(t, filepath.Join(control, ".gitignore"), "user rule\n")
			}
		})
	}
}

func TestAuthoringLocalCommitAndEditorWritersParticipate(t *testing.T) {
	s, editor := authoringFixture(t)
	path := filepath.Join(s.cfg.WorkDir, filepath.FromSlash(editor))
	locks, err := acquireAuthoringLocalLocks(t.Context(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, _ := os.Stat(path)
	change := authoringChangeRequest{EditorPath: editor, Changes: []authoringFileChange{{Scope: "bundle", Path: "main.bot", ExpectedSHA256: contentSHA256(string(original)), Replacements: []authoringReplacement{{Before: "workflow main:", After: "workflow changed:"}}}}}
	document := json.RawMessage(`{"workflows":[{"name":"main","entry":"done"}]}`)
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run, err := rs.CreateRun(t.Context(), "authoring-alias", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.WorkDir = filepath.Dir(path)
	if err := rs.SaveRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	s.runs = newTestRunviewService(t, "", runview.WithStore(rs))
	for _, route := range []string{"authoring", "editor", "run"} {
		t.Run(route, func(t *testing.T) {
			var payload any
			var handler http.HandlerFunc
			switch route {
			case "authoring":
				payload = change
				handler = func(w http.ResponseWriter, r *http.Request) { s.handleAuthoringChange(w, r, true) }
			case "editor":
				payload = saveFileRequest{Path: editor, Document: document}
				handler = s.handleSaveFile
			case "run":
				payload = saveRunFileRequest{Path: "main.bot", Content: "run content"}
				handler = s.handleSaveRunFileContent
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/api/save", strings.NewReader(string(body))).WithContext(ctx)
			req.SetPathValue("id", run.ID)
			rec := httptest.NewRecorder()
			handler(rec, req)
			if ctx.Err() == nil || rec.Code < 400 {
				t.Fatalf("%s escaped held physical lock: %d %s", route, rec.Code, rec.Body.String())
			}
			authoringBytes(t, path, string(original))
		})
	}
	if err := locks[0].flock.Unlock(); err != nil {
		t.Fatal(err)
	}
	// Two same-preview commits have one winner; the waiter revalidates under
	// the same lock and returns an honest conflict rather than clobbering it.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Go(func() {
			body, _ := json.Marshal(change)
			req := httptest.NewRequest(http.MethodPost, "/api/authoring/commit", strings.NewReader(string(body)))
			rec := httptest.NewRecorder()
			s.handleAuthoringChange(rec, req, true)
			codes <- rec.Code
		})
	}
	wg.Wait()
	close(codes)
	seen := map[int]int{}
	for code := range codes {
		seen[code]++
	}
	if seen[200] != 1 || seen[409] != 1 {
		t.Fatalf("concurrent commits: %v", seen)
	}
	// Ordinary save keeps its established in-place behavior after publication.
	body, _ := json.Marshal(saveFileRequest{Path: editor, Document: document})
	req := httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	publishedInfo, _ := os.Stat(path)
	s.handleSaveFile(rec, req)
	afterInfo, _ := os.Stat(path)
	if rec.Code != 200 || !os.SameFile(publishedInfo, afterInfo) || os.SameFile(beforeInfo, afterInfo) {
		t.Fatalf("ordinary editor save changed inode or failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAuthoringLocalCaseSensitiveDestinationsCanShareConservativeLock(t *testing.T) {
	root := t.TempDir()
	paths := []string{filepath.Join(root, "Case.txt"), filepath.Join(root, "case.txt")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := os.Stat(paths[0])
	second, _ := os.Stat(paths[1])
	if os.SameFile(first, second) {
		t.Skip("case-insensitive filesystem")
	}
	locks, err := acquireAuthoringLocalLocks(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAuthoringLocalLocks(locks)
	for _, lock := range locks {
		tx, err := prepareAuthoringLocal(lock, authoringPreviewFile{Operation: "replace", Before: "before", After: "after"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.close()
		if err := tx.publish(); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range paths {
		authoringBytes(t, path, "after")
	}
}

func TestAuthoringLocalHTTPReceiptNamesRetainedOriginal(t *testing.T) {
	s, editor := authoringFixture(t)
	before := "def answer():\n    return 41\n"
	req := authoringChangeRequest{EditorPath: editor, Changes: []authoringFileChange{{Scope: "workspace", Path: "scripts/helper.py", ExpectedSHA256: contentSHA256(before), Replacements: []authoringReplacement{{Before: "return 41", After: "return 42"}}}}}
	rec := authoringCall(t, s, "/commit", req)
	if rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	var response authoringChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Saved || len(response.Recovery) != 1 || len(response.Recovery[0].Files) != 1 {
		t.Fatalf("receipt lacks recovery: %+v", response)
	}
	authoringBytes(t, response.Recovery[0].Files[0], before)
	record, err := os.ReadFile(response.Recovery[0].Record)
	if err != nil || !strings.Contains(string(record), `"stage":"published"`) {
		t.Fatalf("receipt does not locate the successful journal: %s %v", record, err)
	}
}
