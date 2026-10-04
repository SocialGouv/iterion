package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOwningDirStopsAtTheRepositoryRoot: the walk-up answers for the tree
// the file lives in. Past the repository root it would attribute a loose
// file to whatever a directory of the operator's above the repository
// happens to mark — the #1367 case, where `dsl migrate --check bots/`
// handed a board_smoke.bot with no bundle to the operator's own
// skills/-carrying directory and would have raised THAT manifest's floor.
// The root itself is still checked — a bundle may sit at it, inside the
// same repository as the file — and a tree with no .git at all walks to
// the filesystem root as ever.
func TestOwningDirStopsAtTheRepositoryRoot(t *testing.T) {
	tmp := t.TempDir()
	// A directory of the operator's above the repository, marked as a
	// bundle (main.bot + skills/): what the walk must never reach from
	// inside the repository.
	opDir := filepath.Join(tmp, "home")
	writeBundleMarker(t, opDir, DirSkills)
	if err := os.WriteFile(filepath.Join(opDir, MainBotFile), []byte("workflow op:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(opDir, "lab", "repo")
	loose := filepath.Join(repo, "bots", "smoke", "board_smoke.bot")
	if err := os.MkdirAll(filepath.Dir(loose), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loose, []byte("workflow smoke:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sanity: without a repository boundary the walk reaches the operator's
	// directory — the attribution the boundary exists to refuse.
	if got := owningDirUnbounded(loose); got != opDir {
		t.Fatalf("the fixture no longer arms the case: the unbounded walk = %q, want the operator's %q", got, opDir)
	}
	for _, gitEntry := range []string{"dir", "file"} {
		t.Run(".git as a "+gitEntry, func(t *testing.T) {
			dotGit := filepath.Join(repo, ".git")
			if gitEntry == "dir" {
				if err := os.MkdirAll(dotGit, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(dotGit, []byte("gitdir: /elsewhere/repo.git\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := OwningDir(loose); got != "" {
				t.Errorf("OwningDir crossed the repository root: %q, want \"\" — the loose file is nobody's", got)
			}
			// A bundle AT the repository root is above the file inside the
			// same repository: still its bundle.
			writeBundleMarker(t, repo, DirSkills)
			if err := os.WriteFile(filepath.Join(repo, MainBotFile), []byte("workflow root:\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := OwningDir(loose); got != repo {
				t.Errorf("OwningDir = %q with a bundle at the repository root, want %q", got, repo)
			}
			if err := os.RemoveAll(dotGit); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(repo, MainBotFile)); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(repo, DirSkills)); err != nil {
				t.Fatal(err)
			}
		})
	}
	// No repository anywhere: the walk reaches an ancestor bundle, as ever.
	if got := OwningDir(loose); got != opDir {
		t.Errorf("OwningDir without any .git = %q, want the ancestor bundle %q", got, opDir)
	}
}

// owningDirUnbounded is the pre-bound walk, kept here so the test proves
// its fixture would have attributed the file without the boundary.
func owningDirUnbounded(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		main := filepath.Join(dir, MainBotFile)
		if info, statErr := os.Stat(main); statErr == nil && info.Mode().IsRegular() && DirForMainBot(main) != "" {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
}
