package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/backend/ambient"
)

// The two names every `.env` of these witnesses spells. The witness carries
// no refusal rule, so it says whether a file was APPLIED; the route is
// refused by class, and its refusal line names the file it came from — the
// line the ticket (#3428) saw reporting the enclosing checkout's `.env` to a
// command run in a worktree nested inside it.
const (
	gitBoundaryWitness = "DOTENV_GIT_BOUNDARY_WITNESS"
	gitBoundaryRoute   = "DOTENV_GIT_BOUNDARY_URL"
)

// isolateGitBoundaryEnv puts both names and the provenance record under the
// test's control (the loader plants with os.Setenv, which t.Setenv does not
// restore), keeps an operator's route opt-in from deciding whether the route
// is refused, and gives git a home of the test's own: gittest already cuts
// the global and system config off, and HOME/XDG_CONFIG_HOME leave nothing
// of the operator's for git to fall back on.
func isolateGitBoundaryEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	clearEnv(t, envtrust.DotenvEndpointsOptIn)
	clearEnv(t, gitBoundaryWitness)
	clearEnv(t, gitBoundaryRoute)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// writeBoundaryDotEnv writes a `.env` in dir planting the witness (value
// says whose file it is) and the route, and returns its path.
func writeBoundaryDotEnv(t *testing.T, dir, whose string) string {
	t.Helper()
	path := filepath.Join(dir, ".env")
	body := gitBoundaryWitness + "=" + whose + "\n" + gitBoundaryRoute + "=https://" + whose + ".example\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// mkdirIn creates dir/rel and returns it.
func mkdirIn(t *testing.T, dir, rel string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// nestedGitTree builds the ticket's layout with real git: a checkout, and a
// second git working tree INSIDE it — a linked worktree where this repo's
// sessions put theirs (`.claude/worktrees/<name>`), or a submodule. Both
// carry a `.git` FILE, which is what bounds the nested tree. It returns the
// enclosing checkout and the nested tree's root; neither holds a `.env`.
func nestedGitTree(t *testing.T, layout string) (parent, nested string) {
	t.Helper()
	parent = t.TempDir()
	gittest.InitRepo(t, parent)
	switch layout {
	case "linked worktree":
		nested = filepath.Join(parent, ".claude", "worktrees", "wt")
		gittest.Run(t, parent, "worktree", "add", "--quiet", "--detach", nested, "HEAD")
	case "submodule":
		source := t.TempDir()
		gittest.InitRepo(t, source)
		gittest.Run(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", source, "mods/sub")
		nested = filepath.Join(parent, "mods", "sub")
	default:
		t.Fatalf("unknown layout %q", layout)
	}
	info, err := os.Stat(filepath.Join(nested, ".git"))
	if err != nil || info.IsDir() {
		t.Fatalf("the %s at %s carries no .git file (err %v): the layout is not the one under test", layout, nested, err)
	}
	if !ambient.IsRepoRoot(nested) || !ambient.IsRepoRoot(parent) {
		t.Fatalf("git built a layout the boundary does not recognise: parent %v, %s %v", ambient.IsRepoRoot(parent), layout, ambient.IsRepoRoot(nested))
	}
	return parent, nested
}

var nestedGitLayouts = []string{"linked worktree", "submodule"}

// The ticket's witness: a `.env` only in the enclosing checkout, and a
// command run from inside a git working tree nested in it — from a
// subdirectory, so the walk climbs within the tree before it reaches the
// tree's root. Nothing of the enclosing file reaches the process: not the
// witness, not even a refusal line naming that file, since the walk never
// opens it. The file configures the enclosing tree; the command runs on
// another.
func TestTheDotEnvWalkStopsAtTheNestedGitTreesRoot(t *testing.T) {
	for _, layout := range nestedGitLayouts {
		t.Run(layout, func(t *testing.T) {
			isolateGitBoundaryEnv(t)
			parent, nested := nestedGitTree(t, layout)
			parentEnv := writeBoundaryDotEnv(t, parent, "parent")
			t.Chdir(mkdirIn(t, nested, filepath.Join("pkg", "deep")))

			said := captureStderr(t, loadDotEnvFromCwd)

			if got, set := os.LookupEnv(gitBoundaryWitness); set {
				t.Errorf("%s = %q from the enclosing checkout's %s: the walk crossed the %s's root", gitBoundaryWitness, got, parentEnv, layout)
			}
			if envtrust.Planted(gitBoundaryWitness) {
				t.Errorf("%s reads as planted: the enclosing checkout's .env was applied to the %s", gitBoundaryWitness, layout)
			}
			if strings.Contains(said, parentEnv) {
				t.Errorf("the loader reported the enclosing checkout's %s to a command run in the %s: %s", parentEnv, layout, said)
			}
		})
	}
}

// The control: run from the enclosing checkout — its root, or a directory of
// its own that holds no nested tree — and its `.env` is applied, the route
// refused by class with its file named. The walk still climbs within a tree.
func TestTheDotEnvWalkAppliesTheEnclosingCheckoutsFileFromItsOwnTree(t *testing.T) {
	for _, from := range []string{".", filepath.Join("docs", "guide")} {
		t.Run(from, func(t *testing.T) {
			isolateGitBoundaryEnv(t)
			parent, _ := nestedGitTree(t, "linked worktree")
			parentEnv := writeBoundaryDotEnv(t, parent, "parent")
			t.Chdir(mkdirIn(t, parent, from))

			said := captureStderr(t, loadDotEnvFromCwd)

			if got := os.Getenv(gitBoundaryWitness); got != "parent" {
				t.Errorf("%s = %q, want %q: run from the checkout itself, its own .env must apply", gitBoundaryWitness, got, "parent")
			}
			if !envtrust.Planted(gitBoundaryWitness) {
				t.Errorf("%s does not read as planted after the checkout's .env filled it", gitBoundaryWitness)
			}
			if !strings.Contains(said, gitBoundaryRoute) || !strings.Contains(said, parentEnv) {
				t.Errorf("the route refusal does not name %s from %s — it said: %s", gitBoundaryRoute, parentEnv, said)
			}
		})
	}
}

// The boundary is inclusive: the nested tree's root is the last directory
// the walk reads, so a `.env` there applies to a command run anywhere in that
// tree — and wins over the enclosing checkout's, which the walk never reaches.
func TestTheDotEnvAtTheNestedGitTreesRootApplies(t *testing.T) {
	for _, layout := range nestedGitLayouts {
		t.Run(layout, func(t *testing.T) {
			isolateGitBoundaryEnv(t)
			parent, nested := nestedGitTree(t, layout)
			writeBoundaryDotEnv(t, parent, "parent")
			writeBoundaryDotEnv(t, nested, "nested")
			t.Chdir(mkdirIn(t, nested, "pkg"))

			captureStderr(t, loadDotEnvFromCwd)

			if got := os.Getenv(gitBoundaryWitness); got != "nested" {
				t.Errorf("%s = %q, want %q: the %s's own root .env must apply", gitBoundaryWitness, got, "nested", layout)
			}
		})
	}
}

// Outside any git tree the walk keeps going to the filesystem root: a `.env`
// two directories up still applies. The premise is checked rather than
// assumed — a temp dir inside some checkout would make this case another.
func TestTheDotEnvWalkOutsideAnyGitTreeReachesTheNearestFile(t *testing.T) {
	isolateGitBoundaryEnv(t)
	outer := t.TempDir()
	for dir := outer; ; dir = filepath.Dir(dir) {
		if ambient.IsRepoRoot(dir) {
			t.Skipf("the temp dir %s sits in the git working tree at %s: nothing under it is outside a git tree", outer, dir)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	outerEnv := writeBoundaryDotEnv(t, outer, "outer")
	t.Chdir(mkdirIn(t, outer, filepath.Join("a", "b")))

	said := captureStderr(t, loadDotEnvFromCwd)

	if got := os.Getenv(gitBoundaryWitness); got != "outer" {
		t.Errorf("%s = %q, want %q: outside any git tree the walk must reach the nearest .env above", gitBoundaryWitness, got, "outer")
	}
	if !strings.Contains(said, outerEnv) {
		t.Errorf("the route refusal does not name %s — it said: %s", outerEnv, said)
	}
}
