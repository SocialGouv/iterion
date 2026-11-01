package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// throwAwayWorkDirs lists what defaultWorkDir left directly under dir.
func throwAwayWorkDirs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), throwAwayWorkDirPrefix) {
			left = append(left, e.Name())
		}
	}
	return left
}

// An engine built without WithWorkDir, under `go test` in the package
// directory, works in a throw-away directory (#1803) that must not outlive
// the call that needed it. A Run that pauses and the Resume that finishes it
// — on the same engine, and on a fresh engine that only has the run record —
// leave nothing in $TMPDIR. The resume used to remove its directory when a
// helper returned, then mirror its skills into the removed path, re-creating
// it with no owner (#2016: 10 970 left behind in three days).
func TestThrowAwayWorkDirDoesNotOutliveTheCallThatNeededIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		freshEngine bool
	}{
		{name: "same engine resumes", freshEngine: false},
		{name: "fresh engine resumes from the run record", freshEngine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			skill := &Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "witness.md", Content: []byte("# witness\n")}}}

			// The mirror must really write into the throw-away directory while
			// each call runs, or there is nothing to leak and the test proves
			// nothing: every node records whether it saw the mirrored skill.
			var eng, resumer *Engine
			mirrored := map[string]bool{}
			sawSkill := func(node string, e **Engine) func(map[string]any) (map[string]any, error) {
				return func(map[string]any) (map[string]any, error) {
					wd := (*e).workDir
					_, err := os.Stat(filepath.Join(wd, ".claude", "skills", "witness.md"))
					mirrored[node] = err == nil && strings.HasPrefix(filepath.Base(wd), throwAwayWorkDirPrefix)
					return map[string]any{"summary": "needs review", "result": "integrated"}, nil
				}
			}
			exec := newStubExecutor()
			exec.on("analyze", sawSkill("analyze", &eng))
			exec.on("integrate", sawSkill("integrate", &resumer))

			s := tmpStore(t)
			eng = New(humanWorkflow(), s, exec, WithContributions(skill))
			if err := eng.Run(context.Background(), "run-throwaway", nil); !errors.Is(err, ErrRunPaused) {
				t.Fatalf("Run: want ErrRunPaused, got %v", err)
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the paused Run, throw-away workdirs are left in $TMPDIR: %v", left)
			}

			resumer = eng
			if tc.freshEngine {
				resumer = New(humanWorkflow(), s, exec, WithContributions(skill))
			}
			if err := resumer.Resume(context.Background(), "run-throwaway", map[string]any{"approve": true}); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the Resume, throw-away workdirs are left in $TMPDIR: %v", left)
			}
			for _, node := range []string{"analyze", "integrate"} {
				if !mirrored[node] {
					t.Fatalf("node %s did not run in a throw-away workdir holding the mirrored skill — the scenario no longer exercises the leak (mirrored=%v)", node, mirrored)
				}
			}
		})
	}
}

// A resume after a failure restores its workspace in a helper of its own
// (restoreResumeWorkspace): the resumed node runs in a live throw-away dir
// holding the mirrored skill, and nothing is left once Resume returns — on the
// same engine and on a fresh one.
func TestThrowAwayWorkDir_AFailureResumeReleasesIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		freshEngine bool
	}{
		{name: "same engine resumes", freshEngine: false},
		{name: "fresh engine resumes from the run record", freshEngine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			skill := &Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "witness.md", Content: []byte("# witness\n")}}}

			var eng, resumer *Engine
			failOnce, resumedWithSkill := true, false
			exec := newStubExecutor()
			exec.on("step_a", func(map[string]any) (map[string]any, error) { return map[string]any{"result": "a"}, nil })
			exec.on("step_b", func(map[string]any) (map[string]any, error) {
				if failOnce {
					failOnce = false
					return nil, errors.New("transient failure")
				}
				wd := resumer.workDir
				_, err := os.Stat(filepath.Join(wd, ".claude", "skills", "witness.md"))
				resumedWithSkill = err == nil && strings.HasPrefix(filepath.Base(wd), throwAwayWorkDirPrefix)
				return map[string]any{"result": "b"}, nil
			})

			s := tmpStore(t)
			eng = New(charResumeWF(), s, exec, WithContributions(skill))
			if err := eng.Run(context.Background(), "run-failure-resume", nil); err == nil {
				t.Fatal("Run: want the transient failure of step_b, got nil")
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the failed Run, throw-away workdirs are left in $TMPDIR: %v", left)
			}

			resumer = eng
			if tc.freshEngine {
				resumer = New(charResumeWF(), s, exec, WithContributions(skill))
			}
			if err := resumer.Resume(context.Background(), "run-failure-resume", nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
				t.Fatalf("after the failure Resume, throw-away workdirs are left in $TMPDIR: %v", left)
			}
			if !resumedWithSkill {
				t.Fatal("step_b did not resume in a live throw-away workdir holding the mirrored skill — the scenario no longer exercises the failure-resume lifecycle")
			}
		})
	}
}

// A parent built without WithWorkDir hands its throw-away dir to a child the
// way the host runners do (WithWorkDir(req.WorkDir)). The child never records
// that path on its run: resumed after the parent released it, it would adopt
// the removed path and its skill mirror would re-create it with no owner.
func TestThrowAwayWorkDir_AChildNeverRecordsItsParentsThrowAway(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	skill := &Contributions{Plugin: []ContributionFile{{Kind: "skills", Name: "witness.md", Content: []byte("# witness\n")}}}
	exec := newStubExecutor()
	exec.on("analyze", func(map[string]any) (map[string]any, error) { return map[string]any{"summary": "needs review"}, nil })
	exec.on("integrate", func(map[string]any) (map[string]any, error) { return map[string]any{"result": "integrated"}, nil })
	s := tmpStore(t)

	var handed string
	runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		handed = req.WorkDir
		child := New(humanWorkflow(), s, exec, WithWorkDir(req.WorkDir), WithParentRunID(req.ParentRunID), WithContributions(skill))
		if err := child.Run(ctx, "child-run", nil); err != nil && !errors.Is(err, ErrRunPaused) {
			return nil, err
		}
		return map[string]any{}, nil
	}
	parent := New(&ir.Workflow{
		Name: "parent", Worktree: "none", Entry: "run_child",
		Nodes: map[string]ir.Node{
			"run_child": &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "run_child"}, Source: "child.bot"},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "run_child", To: "done"}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{}, Loops: map[string]*ir.Loop{},
	}, s, exec, WithSubbotRunner(runner))
	if err := parent.Run(context.Background(), "parent-run", nil); err != nil {
		t.Fatalf("parent Run: %v", err)
	}
	if filepath.Dir(handed) != tmp || !strings.HasPrefix(filepath.Base(handed), throwAwayWorkDirPrefix) {
		t.Fatalf("the parent handed %q to its child, want its throw-away workdir under %s — the scenario no longer exercises a handed-down throw-away", handed, tmp)
	}
	child, err := s.LoadRun(context.Background(), "child-run")
	if err != nil {
		t.Fatal(err)
	}
	if child.WorkDir == handed {
		t.Fatalf("the child recorded its parent's throw-away workdir %s on its run", handed)
	}

	resumer := New(humanWorkflow(), s, exec, WithContributions(skill))
	if err := resumer.Resume(context.Background(), "child-run", map[string]any{"approve": true}); err != nil {
		t.Fatalf("resume the child: %v", err)
	}
	if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
		t.Fatalf("after the child's resume, throw-away workdirs are left in $TMPDIR: %v", left)
	}
}

// The refusal is the CHECKOUT, not one package directory inside it. Every
// package's tests run with their own package dir as cwd, so a guard keyed on
// `pkg/runtime` alone left every other package handing the engine the
// developer's repository: `worktree: auto` then registered a per-run worktree
// in its `.git`, and 32 such registrations were found on one machine (#2781 —
// each costs sandbox deny paths, and E2BIG at ~600 has already killed two
// sessions). A cwd anywhere in the checkout — a sibling package tree, the
// module root itself — gets a throw-away workdir; a cwd outside it keeps the
// `iterion run` contract and works where it stands.
func TestDefaultWorkDirRefusesEveryCwdInsideTheCheckoutNotJustThisPackage(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Derived by walking up from the test's own cwd, never from the
	// predicate under test: <checkout>/pkg/runtime -> <checkout>/pkg ->
	// <checkout>.
	siblingTree := filepath.Dir(cwd)
	checkoutRoot := filepath.Dir(siblingTree)
	if _, statErr := os.Stat(filepath.Join(checkoutRoot, "go.mod")); statErr != nil {
		t.Fatalf("%s holds no go.mod (%v): the test no longer runs from <checkout>/pkg/runtime and proves nothing", checkoutRoot, statErr)
	}

	for _, tc := range []struct {
		name      string
		dir       string
		throwAway bool
	}{
		{name: "a sibling package tree of the checkout", dir: siblingTree, throwAway: true},
		{name: "the checkout root itself", dir: checkoutRoot, throwAway: true},
		{name: "a directory outside the checkout", dir: t.TempDir(), throwAway: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			t.Chdir(tc.dir)

			e := &Engine{}
			e.defaultWorkDir()
			t.Cleanup(e.releaseTempWorkDir)

			if !tc.throwAway {
				if e.workDirTemp != "" {
					t.Fatalf("cwd %s is outside the checkout yet the engine took the throw-away workdir %s — a run must work where the operator stands", tc.dir, e.workDirTemp)
				}
				if !samePath(e.workDir, resolvedPath(tc.dir)) && !samePath(e.workDir, tc.dir) {
					t.Fatalf("cwd %s gave workDir %s, want the cwd itself", tc.dir, e.workDir)
				}
				return
			}
			if e.workDirTemp == "" {
				t.Fatalf("cwd %s is inside the checkout %s and the engine kept it as its workDir — `worktree: auto` registers a worktree in its .git and a skill mirror writes into it", tc.dir, checkoutRoot)
			}
			if e.workDir != e.workDirTemp {
				t.Fatalf("workDir %s is not the throw-away dir %s the engine recorded", e.workDir, e.workDirTemp)
			}
			if filepath.Dir(e.workDirTemp) != tmp || !strings.HasPrefix(filepath.Base(e.workDirTemp), throwAwayWorkDirPrefix) {
				t.Fatalf("throw-away workdir %s is not a %s* directory under $TMPDIR %s", e.workDirTemp, throwAwayWorkDirPrefix, tmp)
			}
		})
	}
}

// What a refused cwd is handed instead must be verified, not assumed of the
// ambient $TMPDIR. A temp dir inside a repository is one the engine's own
// resolvers call a git repository, so `worktree: auto` would register a
// per-run worktree there rather than degrade in place — the very defect the
// refusal removes, re-entered through its escape hatch. A relative TMPDIR
// makes os.MkdirTemp return a relative path, which resolves against the cwd
// just refused and reaches every git and tool call below as a path that
// moves with the process. Each candidate is checked, and an unfit one is
// removed and named.
func TestThrowAwayWorkDirIsAbsoluteAndOutsideEveryRepository(t *testing.T) {
	t.Run("a temp dir in no repository is adopted", func(t *testing.T) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		tree := filepath.Join(t.TempDir(), "checkout")
		if err := os.MkdirAll(tree, 0o750); err != nil {
			t.Fatal(err)
		}

		dir, err := checkoutAt(tree).throwAwayWorkDir()
		if err != nil {
			t.Fatalf("throwAwayWorkDir with $TMPDIR %s, in no repository and outside the checkout: %v", tmp, err)
		}
		if !filepath.IsAbs(dir) {
			t.Fatalf("throw-away workdir %q is not absolute", dir)
		}
		if filepath.Dir(dir) != tmp || !strings.HasPrefix(filepath.Base(dir), throwAwayWorkDirPrefix) {
			t.Fatalf("throw-away workdir %s is not a %s* directory under $TMPDIR %s", dir, throwAwayWorkDirPrefix, tmp)
		}
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("throw-away workdir %s was not created: %v", dir, statErr)
		}
	})

	t.Run("a temp dir inside a git repository is refused", func(t *testing.T) {
		repo := gittest.SourceRepo(t)
		tmp := filepath.Join(repo, "scratchtmp")
		if err := os.MkdirAll(tmp, 0o750); err != nil {
			t.Fatal(err)
		}
		// Git's own answer, not ours: the registry a `worktree: auto` run
		// started in $TMPDIR would write into.
		common := strings.TrimSpace(gittest.Run(t, tmp, "rev-parse", "--path-format=absolute", "--git-common-dir"))
		if !samePath(filepath.Dir(common), resolvedPath(repo)) {
			t.Fatalf("git reports %s as the registry of $TMPDIR %s, want the one of %s — the scenario no longer puts $TMPDIR inside a repository", common, tmp, repo)
		}
		t.Setenv("TMPDIR", tmp)
		tree := filepath.Join(t.TempDir(), "checkout")
		if err := os.MkdirAll(tree, 0o750); err != nil {
			t.Fatal(err)
		}

		dir, err := checkoutAt(tree).throwAwayWorkDir()
		if err == nil {
			t.Fatalf("throwAwayWorkDir adopted %s, inside the repository %s: a `worktree: auto` run there registers a per-run worktree in %s instead of degrading in place", dir, repo, common)
		}
		if dir != "" {
			t.Fatalf("the refused candidate was returned anyway: %q", dir)
		}
		if !strings.Contains(err.Error(), resolvedPath(repo)) {
			t.Fatalf("the refusal does not name the repository %s: %v", repo, err)
		}
		if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
			t.Fatalf("the refused candidate was left behind in %s: %v", tmp, left)
		}
	})

	t.Run("a relative temp dir is refused", func(t *testing.T) {
		tree := filepath.Join(t.TempDir(), "checkout")
		tmp := filepath.Join(tree, "reltmp")
		if err := os.MkdirAll(tmp, 0o750); err != nil {
			t.Fatal(err)
		}
		t.Chdir(tree)
		t.Setenv("TMPDIR", "reltmp")

		dir, err := checkoutAt(tree).throwAwayWorkDir()
		if err == nil {
			t.Fatalf("throwAwayWorkDir adopted %q from a relative $TMPDIR: made absolute it is %s, inside the checkout the cwd was refused for", dir, filepath.Join(tmp, filepath.Base(dir)))
		}
		if dir != "" {
			t.Fatalf("the refused candidate was returned anyway: %q", dir)
		}
		if !strings.Contains(err.Error(), resolvedPath(tree)) {
			t.Fatalf("the refusal does not name the checkout %s: %v", tree, err)
		}
		if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
			t.Fatalf("the refused candidate was left behind in %s: %v", tmp, left)
		}
	})
}

// A refused cwd is handed nothing the engine has not verified, through the
// engine itself: with $TMPDIR inside a repository, the throw-away workdir
// would be the defect again, so no workspace is adopted at all and the
// degradation is logged with its cure.
func TestDefaultWorkDirAdoptsNoThrowAwayWorkDirInsideARepository(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if !compiledCheckout().refuses(cwd) {
		t.Fatalf("the test's own cwd %s is not refused: it no longer runs from iterion's checkout and proves nothing", cwd)
	}
	repo := gittest.SourceRepo(t)
	tmp := filepath.Join(repo, "scratchtmp")
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)

	e := &Engine{}
	e.defaultWorkDir()
	t.Cleanup(e.releaseTempWorkDir)

	if e.workDirTemp != "" {
		t.Fatalf("the engine adopted the throw-away workdir %s, inside the repository %s $TMPDIR points into: its `worktree: auto` run registers a per-run worktree in that repository", e.workDirTemp, repo)
	}
	if insideTree(e.workDir, repo) {
		t.Fatalf("workDir %s is inside the repository %s $TMPDIR points into", e.workDir, repo)
	}
	if !samePath(e.workDir, cwd) && !samePath(e.workDir, resolvedPath(cwd)) {
		t.Fatalf("workDir %s is neither a verified throw-away dir nor the cwd %s the engine logs it kept", e.workDir, cwd)
	}
	if left := throwAwayWorkDirs(t, tmp); len(left) != 0 {
		t.Fatalf("the refused candidate was left behind in %s: %v", tmp, left)
	}
}

// The registration lands in the REGISTRY, so that is what the refusal keys
// on. A session compiles the binary from a linked worktree: the main
// checkout of the same repository is not under that tree, a sibling worktree
// is elsewhere again, and `git worktree add` from any of them writes into
// the one `.git` they share — what this guard keeps clean (#2781). The tree
// is only one spelling of the repository. A repository nested under the
// compiled tree owns its registry and its files, and keeps the run.
func TestRefusalKeysOnTheRepositoryNotOnTheCompiledTree(t *testing.T) {
	tmp := t.TempDir()
	if repo := gitlib.FindMainRepoRoot(tmp); repo != "" {
		t.Fatalf("the test temp root %s is inside the git repository %s: this fixture needs a temp root outside every repository (point $TMPDIR at one)", tmp, repo)
	}
	var (
		main     = filepath.Join(tmp, "main")
		sub      = filepath.Join(main, "pkg", "runtime")
		compiled = filepath.Join(tmp, "compiled")
		sibling  = filepath.Join(tmp, "sibling")
		nested   = filepath.Join(compiled, "scratchtmp", "own")
		other    = filepath.Join(tmp, "other")
		plain    = filepath.Join(tmp, "plain")
		link     = filepath.Join(tmp, "link-to-main")
		noGit    = filepath.Join(tmp, "notree")
	)
	for _, dir := range []string{main, sub, other, plain, filepath.Join(noGit, "pkg")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	gittest.InitRepo(t, main)
	gittest.InitRepo(t, other)
	gittest.Run(t, main, "worktree", "add", "--detach", compiled)
	gittest.Run(t, main, "worktree", "add", "--detach", sibling)
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	gittest.InitRepo(t, nested)
	if err := os.Symlink(main, link); err != nil {
		t.Fatal(err)
	}

	// The binary is compiled from a linked worktree, the shape every fleet
	// session runs in — and the shape the tree-keyed refusal missed.
	c := checkoutAt(compiled)
	if !samePath(c.repo, resolvedPath(main)) {
		t.Fatalf("checkoutAt(%s).repo = %q, want the main checkout %s: the fixture is not a linked worktree of it and proves nothing", compiled, c.repo, main)
	}
	if samePath(c.tree, c.repo) {
		t.Fatalf("the compiled tree %s and its repository %s are the same path: the fixture no longer separates them", c.tree, c.repo)
	}

	for _, tc := range []struct {
		name    string
		dir     string
		refused bool
	}{
		{name: "the compiled worktree itself", dir: compiled, refused: true},
		{name: "the main checkout of the same repository", dir: main, refused: true},
		{name: "a subdirectory of the main checkout", dir: sub, refused: true},
		{name: "a symlinked spelling of the main checkout", dir: link, refused: true},
		{name: "a sibling linked worktree", dir: sibling, refused: true},
		{name: "a repository nested in the compiled tree", dir: nested, refused: false},
		{name: "another repository", dir: other, refused: false},
		{name: "a directory in no repository", dir: plain, refused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			switch got := c.refuses(tc.dir); {
			case tc.refused && !got:
				t.Fatalf("%s (%s) is kept as the run's workspace: `git worktree add` there registers in the `.git` of %s, which the compiled checkout %s shares, and nothing comes back for the entry", tc.name, tc.dir, c.repo, c.tree)
			case !tc.refused && got:
				t.Fatalf("%s (%s) is refused: it registers in a registry of its own and the test owns its files, so the run must work where it stands", tc.name, tc.dir)
			}
		})
	}

	// A source tree in no repository at all: nothing can register anywhere,
	// and the tree's own files are what the refusal still protects (#1803).
	t.Run("a checkout in no repository protects its own tree", func(t *testing.T) {
		c := checkoutAt(noGit)
		if c.repo != "" {
			t.Fatalf("checkoutAt(%s).repo = %q, want none: the fixture is inside a repository and proves nothing", noGit, c.repo)
		}
		if !c.refuses(filepath.Join(noGit, "pkg")) {
			t.Fatalf("a package directory of the checkout %s is kept as the run's workspace: a skill mirror writes into the developer's files", noGit)
		}
		if c.refuses(other) {
			t.Fatalf("the repository %s is refused by a checkout with none of its own", other)
		}
	})
}
