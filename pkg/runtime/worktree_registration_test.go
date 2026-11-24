package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/store"
)

type registrationResult struct {
	cleanup func()
	err     error
}

func registrationAction(t *testing.T, operation, root, repo, hint, tip string) func() registrationResult {
	t.Helper()
	switch operation {
	case "restore":
		st := tmpStore(t) // The native watcher stays outside the synctest bubble.
		run := &store.Run{ID: "restored", Worktree: true, WorktreeReclaimed: true,
			RepoRoot: repo, BaseCommit: tip, WorkDir: filepath.Join(st.Root(), "worktrees", "restored")}
		gittest.Run(t, repo, "update-ref", pristineWorktreeRef(run.ID), tip)
		return func() registrationResult {
			err := RestoreReclaimedWorktree(context.Background(), st, run)
			return registrationResult{func() { _, _ = runWorktreeCommand(repo, "remove", "--force", run.WorkDir) }, err}
		}
	case "remove":
		wc, cleanup, err := setupWorktree(root, "removed", repo, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return func() registrationResult {
			cleanup()
			if _, err := os.Stat(wc.wtPath); !os.IsNotExist(err) {
				return registrationResult{cleanup, fmt.Errorf("cleanup retained worktree: %v", err)}
			}
			return registrationResult{}
		}
	default:
		return func() registrationResult {
			_, cleanup, err := setupWorktree(root, "sibling", hint, "", nil)
			return registrationResult{cleanup, err}
		}
	}
}

// Git creates commondir before writing its contents. Stage that real disk
// state while another registration owns the gate; unprotected Git fails
// with the exact "failed to read .../commondir: Success" from #2670.
func TestWorktreeRegistrationWaitsForTheCommonRepository(t *testing.T) {
	for _, variant := range []string{"setup/main", "setup/linked", "setup/symlink", "restore/main", "remove/main"} {
		t.Run(variant, func(t *testing.T) {
			operation, spelling, _ := strings.Cut(variant, "/")
			repo, tip := initBareishRepo(t)
			other, _ := initBareishRepo(t)
			hint := repo
			switch spelling {
			case "linked":
				hint = filepath.Join(t.TempDir(), "linked")
				gittest.Run(t, repo, "worktree", "add", "--detach", hint, tip)
				t.Cleanup(func() { gittest.RemoveWorktree(t, repo, hint) })
			case "symlink":
				hint = filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(repo, hint); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			root := t.TempDir()
			action := registrationAction(t, operation, root, repo, hint, tip)
			synctest.Test(t, func(t *testing.T) {
				release, err := lockWorktreeRegistration(repo)
				if err != nil {
					t.Fatal(err)
				}
				pending := filepath.Join(repo, ".git", "worktrees", "pending")
				if err := os.MkdirAll(pending, 0o755); err != nil {
					t.Fatal(err)
				}
				for name, content := range map[string]string{
					"commondir": "", "HEAD": tip + "\n", "gitdir": filepath.Join(root, "pending", ".git") + "\n",
				} {
					writeFile(t, filepath.Join(pending, name), content)
				}
				blocked, independent := make(chan registrationResult, 1), make(chan registrationResult, 1)
				go func() {
					blocked <- action()
				}()
				go func() {
					_, cleanup, err := setupWorktree(root, "unrelated", other, "", nil)
					independent <- registrationResult{cleanup, err}
				}()
				synctest.Wait()
				var early *registrationResult
				select {
				case got := <-blocked:
					early = &got
					t.Errorf("registration must wait for the common repository, got early result: %v", got.err)
				default:
				}
				var separate *registrationResult
				select {
				case got := <-independent:
					separate = &got
					if got.err != nil {
						t.Errorf("unrelated repository registration: %v", got.err)
					}
				default:
					t.Error("a different repository must register while this one is held")
				}
				for _, name := range []string{"commondir", "HEAD", "gitdir"} {
					if err := os.Remove(filepath.Join(pending, name)); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(pending); err != nil {
					t.Fatal(err)
				}
				release()
				synctest.Wait()
				if early == nil {
					got := <-blocked
					early = &got
				}
				if early.err != nil {
					t.Errorf("registration after the writer leaves: %v", early.err)
				}
				if early.cleanup != nil {
					early.cleanup()
				}
				if separate == nil {
					got := <-independent
					separate = &got
				}
				if separate.cleanup != nil {
					separate.cleanup()
				}
			})
		})
	}
}

func TestWorktreeRegistrationFailureReleasesTheGate(t *testing.T) {
	repo, _ := initBareishRepo(t)
	assertIdle := func() {
		t.Helper()
		workspaceResourceGates.Lock()
		defer workspaceResourceGates.Unlock()
		for key := range workspaceResourceGates.byPath {
			if strings.Contains(key, repo) {
				t.Fatalf("registration gate retained an idle repository: %q", key)
			}
		}
	}
	path := filepath.Join(t.TempDir(), "child")
	if _, err := runWorktreeCommand(repo, "add", path, "missing-commit-2670"); err == nil {
		t.Fatal("invalid commit must fail")
	}
	assertIdle() // Observe a leaked gate before another command would wait on it.
	if _, err := runWorktreeCommand(repo, "add", "--detach", path, "HEAD"); err != nil {
		t.Fatalf("failed registration retained its gate: %v", err)
	}
	if _, err := runWorktreeCommand(repo, "remove", "--force", path); err != nil {
		t.Fatal(err)
	}
	assertIdle()
}

func TestWorktreeRegistrationPreservesSeparateGitDirectoryWhitespace(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Windows does not preserve a trailing space in a directory name")
	}
	root := t.TempDir()
	repo, common := filepath.Join(root, "repo"), filepath.Join(root, "git registry ")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, repo, "init", "--separate-git-dir", common)
	writeFile(t, filepath.Join(repo, "README"), "registry fixture\n")
	gittest.Run(t, repo, "add", "README")
	gittest.Run(t, repo, "commit", "-m", "init")
	baseline := filepath.Join(root, "baseline")
	gittest.Run(t, repo, "worktree", "add", "--detach", baseline, "HEAD")
	gittest.Run(t, repo, "worktree", "remove", "--force", baseline)
	wc, cleanup, err := setupWorktree(root, "wrapped", repo, "", nil)
	if err != nil {
		t.Fatalf("runtime must accept the separate Git directory accepted by Git: %v", err)
	}
	cleanup()
	if _, err := os.Stat(wc.wtPath); !os.IsNotExist(err) {
		t.Fatalf("cleanup must use the same untrimmed registry path: %v", err)
	}
}
