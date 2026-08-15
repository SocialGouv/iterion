package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// cleanFixture builds a real git repository plus an iterion store whose
// worktrees are genuine linked worktrees of it, in the shape production
// creates them. The classification this command makes is entirely a set
// of claims about git state, so the tests exercise git rather than a
// model of it.
type cleanFixture struct {
	t     *testing.T
	root  string
	repo  string
	store string
}

func newCleanFixture(t *testing.T) *cleanFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	f := &cleanFixture{
		t:     t,
		root:  root,
		repo:  filepath.Join(root, "repo"),
		store: filepath.Join(root, "store"),
	}
	mustMkdir(t, f.repo)
	mustMkdir(t, filepath.Join(f.store, "worktrees"))
	f.initRepo(f.repo)
	return f
}

func (f *cleanFixture) initRepo(dir string) {
	f.t.Helper()
	f.git(dir, "init", "--initial-branch=main")
	mustWrite(f.t, filepath.Join(dir, "README.md"), "base\n")
	f.git(dir, "add", "README.md")
	f.git(dir, "commit", "-m", "base")
}

func (f *cleanFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := f.gitErr(dir, args...)
	if err != nil {
		f.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func (f *cleanFixture) gitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		"LC_ALL=C", "LANG=C")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// addWorktree creates <store>/worktrees/<runID> the way iterion does:
// `git worktree add <path> <sha>` with no -b, so HEAD is DETACHED
// (pkg/runtime/worktree.go). One commit is made on top. With no ref
// pointing at it, that commit is `unlanded`.
func (f *cleanFixture) addWorktree(runID string) string {
	f.t.Helper()
	path := filepath.Join(f.store, "worktrees", runID)
	base := f.git(f.repo, "rev-parse", "main")
	f.git(f.repo, "worktree", "add", path, base)
	mustWrite(f.t, filepath.Join(path, runID+".txt"), "work by "+runID+"\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "work by "+runID)
	return path
}

// promote mirrors finalizeWorktree: a branch is created in the REPO at
// the worktree's HEAD; the worktree itself stays detached. The commits
// are now held by a ref, but nothing was built on top of them.
func (f *cleanFixture) promote(runID string) {
	f.t.Helper()
	head := f.git(filepath.Join(f.store, "worktrees", runID), "rev-parse", "HEAD")
	f.git(f.repo, "branch", "--", "iterion/run/"+runID, head)
}

// mergeIntoMain builds main on top of the run's commits, so a ref whose
// tip is NOT the worktree's HEAD contains that HEAD.
func (f *cleanFixture) mergeIntoMain(runID string) {
	f.t.Helper()
	f.promote(runID)
	f.git(f.repo, "merge", "--no-ff", "-m", "merge "+runID, "iterion/run/"+runID)
}

// addSubmodule registers a submodule in the repo. A worktree created
// afterwards declares it but does not populate it, which is the state
// `git worktree add` always leaves.
func (f *cleanFixture) addSubmodule(at string) {
	f.t.Helper()
	sub := filepath.Join(f.root, "submodule-src")
	mustMkdir(f.t, sub)
	f.initRepo(sub)
	if _, err := f.gitErr(f.repo, "-c", "protocol.file.allow=always", "submodule", "add", sub, at); err != nil {
		f.t.Skip("submodules unavailable in this git configuration")
	}
	f.git(f.repo, "commit", "-m", "add submodule")
}

func (f *cleanFixture) seedRun(runID string, status store.RunStatus) {
	f.t.Helper()
	s, err := store.New(f.store)
	if err != nil {
		f.t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		f.t.Fatalf("CreateRun(%s): %v", runID, err)
	}
	if status != store.RunStatusRunning {
		if err := s.UpdateRunStatus(ctx, runID, status, ""); err != nil {
			f.t.Fatalf("UpdateRunStatus(%s): %v", runID, err)
		}
	}
}

func (f *cleanFixture) backdate(runID string, age time.Duration) {
	f.t.Helper()
	ts := time.Now().Add(-age)
	p := filepath.Join(f.store, "worktrees", runID)
	if err := os.Chtimes(p, ts, ts); err != nil {
		f.t.Fatalf("chtimes %s: %v", runID, err)
	}
}

func (f *cleanFixture) run(opts CleanOptions) CleanResult {
	f.t.Helper()
	if opts.StoreDir == "" && !opts.AllProjects {
		opts.StoreDir = f.store
	}
	if opts.Level == "" {
		opts.Level = CleanConservative
	}
	r, err := f.runErr(opts)
	if err != nil {
		f.t.Fatalf("RunClean: %v", err)
	}
	return r
}

func (f *cleanFixture) runErr(opts CleanOptions) (CleanResult, error) {
	f.t.Helper()
	var buf bytes.Buffer
	p := &Printer{W: &buf, Format: OutputJSON}
	err := RunClean(opts, p)
	if buf.Len() == 0 {
		return CleanResult{}, err
	}
	return decodeCleanResult(f.t, buf.Bytes()), err
}

func decodeCleanResult(t *testing.T, raw []byte) CleanResult {
	t.Helper()
	var r CleanResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode result: %v\nraw: %s", err, raw)
	}
	return r
}

func deletedPaths(r CleanResult) map[string]bool {
	out := map[string]bool{}
	for _, wt := range r.Deleted {
		out[filepath.Base(wt.Path)] = true
	}
	return out
}

func sparedReason(r CleanResult, runID string) string {
	for _, wt := range r.Spared {
		if filepath.Base(wt.Path) == runID {
			return wt.SkipReason
		}
	}
	return ""
}

func landingOf(r CleanResult, runID string) string {
	for _, wt := range append(append([]CleanedWorktree{}, r.Deleted...), r.Spared...) {
		if filepath.Base(wt.Path) == runID {
			return wt.Landing
		}
	}
	return ""
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s: %s is gone from disk (%v)", why, path, err)
	}
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s: %s is still on disk", why, path)
	}
}

// breakGit puts a `git` shim first on PATH that fails for one
// subcommand and delegates everything else to the real binary. The
// "we could not tell, so we must refuse" fallbacks cannot be reached by
// arranging repository state — only by making git fail.
func breakGit(t *testing.T, subcommand string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	// Match the SUBCOMMAND — the first argument that is not a global flag
	// — so breaking `status` does not also break `submodule status`.
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  case \"$a\" in -*) continue ;; esac\n" +
		"  if [ \"$a\" = \"" + subcommand + "\" ]; then\n    echo 'shim: forced failure' >&2\n    exit 128\n  fi\n" +
		"  break\ndone\nexec " + real + " \"$@\"\n"
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil { //nolint:gosec // test shim must be executable
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// rejectPathFormat shims a git that predates --path-format (2.31): the
// option is rejected, and the bare --git-common-dir answers relatively.
func rejectPathFormat(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do\n" +
		"  case \"$a\" in --path-format=*) echo \"error: unknown option \\`$a'\" >&2; exit 129 ;; esac\n" +
		"done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil { //nolint:gosec // test shim
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// breakGitArg fails any git invocation carrying a given argument.
func breakGitArg(t *testing.T, arg string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = \"" + arg + "\" ]; then\n" +
		"    echo 'shim: forced failure' >&2\n    exit 128\n  fi\ndone\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil { //nolint:gosec // test shim
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// --- guards that no level lifts -------------------------------------------

// A run that has not reached a terminal status owns its worktree, and no
// level may take it. This is the guard that protects a campaign in
// flight, and it must not depend on the worktree's mtime: a run can sit
// in a single agent turn for hours without writing to its checkout.
func TestClean_NeverDeletesWorktreeOfNonTerminalRun(t *testing.T) {
	for _, status := range []store.RunStatus{
		store.RunStatusRunning,
		store.RunStatusQueued,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newCleanFixture(t)
			f.addWorktree("live")
			f.mergeIntoMain("live") // merged: eligible on every other count
			f.seedRun("live", status)
			f.backdate("live", 90*24*time.Hour)

			r := f.run(CleanOptions{Level: CleanAggressive, Apply: true})
			if deletedPaths(r)["live"] {
				t.Fatalf("deleted the worktree of a %s run", status)
			}
			if got := sparedReason(r, "live"); got != skipRunActive {
				t.Fatalf("spared for %q, want %q", got, skipRunActive)
			}
			mustExist(t, filepath.Join(f.store, "worktrees", "live"), "run "+string(status))
		})
	}
}

// The scan is a snapshot. `iterion resume` reuses a run's existing
// worktree, so a run that was terminal when scanned can be running by
// the time the sweep reaches it.
func TestClean_RechecksRunStatusImmediatelyBeforeDeleting(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("resumed")
	f.mergeIntoMain("resumed")
	f.seedRun("resumed", store.RunStatusFinished) // terminal and not resumable at scan time
	f.backdate("resumed", 90*24*time.Hour)

	// Flip it to running between the scan and the deletion, which is what
	// a concurrent `iterion resume` does.
	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	opts := CleanOptions{StoreDir: f.store, Level: CleanAggressive, Apply: true}
	opts.Now = func() time.Time {
		_ = s.UpdateRunStatus(context.Background(), "resumed", store.RunStatusRunning, "")
		return time.Now()
	}
	var buf bytes.Buffer
	if err := RunClean(opts, &Printer{W: &buf, Format: OutputJSON}); err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	r := decodeCleanResult(t, buf.Bytes())
	if deletedPaths(r)["resumed"] {
		t.Fatal("deleted the worktree of a run that resumed during the sweep")
	}
	if got := sparedReason(r, "resumed"); got != skipRunActive {
		t.Fatalf("spared for %q, want %q", got, skipRunActive)
	}
	mustExist(t, filepath.Join(f.store, "worktrees", "resumed"), "resumed run")
}

// Commits nothing was built upon and no ref holds would survive only in
// the reflog. No level deletes them — that is the promise that makes the
// command usable without reading its source first.
func TestClean_NeverDeletesUnlandedWorktree(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("orphaned") // detached, never promoted: exactly production's failed run
	f.seedRun("orphaned", store.RunStatusFailed)

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if got := landingOf(r, "orphaned"); got != landingNowhere {
		t.Fatalf("landing %q, want %q", got, landingNowhere)
	}
	if got := sparedReason(r, "orphaned"); got != skipUnlanded {
		t.Fatalf("spared for %q, want %q", got, skipUnlanded)
	}
	mustExist(t, path, "unlanded worktree")
}

// Git answers for the nearest enclosing repository when the directory it
// is asked about is not itself a worktree. The project-local
// `<repo>/.iterion/` store puts the whole pool inside the operator's
// checkout, and `.iterion/` is conventionally gitignored — so the parent
// reads back clean and merged, and an arbitrary directory of uncommitted
// work looks like a landed worktree.
func TestClean_RefusesGitAnswersThatBelongToAnEnclosingRepo(t *testing.T) {
	f := newCleanFixture(t)
	// A project-local store: <repo>/.iterion/worktrees/<id>
	nestedStore := filepath.Join(f.repo, ".iterion")
	mustMkdir(t, filepath.Join(nestedStore, "worktrees", "data"))
	mustWrite(t, filepath.Join(nestedStore, "worktrees", "data", "notes.md"), "never committed anywhere\n")
	mustWrite(t, filepath.Join(f.repo, ".gitignore"), ".iterion/\n")
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-m", "ignore the store")

	// Sanity: git really does answer for the parent from in there.
	if top := f.git(filepath.Join(nestedStore, "worktrees", "data"), "rev-parse", "--show-toplevel"); !samePath(top, f.repo) {
		t.Fatalf("fixture is not reproducing the nesting: toplevel=%s", top)
	}

	r := f.run(CleanOptions{StoreDir: nestedStore, Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["data"] {
		t.Fatal("conservative deleted a directory classified from the enclosing repo's state")
	}
	if got := landingOf(r, "data"); got != landingOrphan {
		t.Errorf("landing %q, want %q — git's answer must be refused", got, landingOrphan)
	}
	mustExist(t, filepath.Join(nestedStore, "worktrees", "data", "notes.md"), "nested data dir")
}

// An orphan is not proof of junk: a checkout whose parent repository
// moved looks exactly like a stale directory and may hold a day of work.
func TestClean_OrphanNeedsAggressiveAndIsNeverReportedClean(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("stranded")
	f.mergeIntoMain("stranded")
	f.seedRun("stranded", store.RunStatusFinished)
	mustWrite(t, filepath.Join(path, "urgent.txt"), "uncommitted, and about to be judged junk\n")

	// Break the link the way moving the parent repository does.
	f.git(f.repo, "worktree", "list") // touch, so the registration exists
	if err := os.Rename(f.repo, f.repo+"-moved"); err != nil {
		t.Fatalf("rename repo: %v", err)
	}

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["stranded"] {
		t.Fatal("conservative deleted an orphan holding uncommitted work")
	}
	if got := landingOf(r, "stranded"); got != landingOrphan {
		t.Fatalf("landing %q, want %q", got, landingOrphan)
	}
	if got := sparedReason(r, "stranded"); got != skipLevel {
		t.Errorf("spared for %q, want %q", got, skipLevel)
	}
	for _, wt := range r.Spared {
		if filepath.Base(wt.Path) == "stranded" && !wt.Dirty {
			t.Error("an orphan whose tree git cannot read was reported clean")
		}
	}
	mustExist(t, filepath.Join(path, "urgent.txt"), "orphaned checkout")
}

// An INITIALISED submodule's commits live in the worktree's own admin
// directory, which goes when the registration does. Containment in the
// superproject proves nothing about them.
func TestClean_RefusesWorktreeCarryingAnInitialisedSubmodule(t *testing.T) {
	f := newCleanFixture(t)
	f.addSubmodule("vendor/lib")

	path := f.addWorktree("withsub")
	f.mergeIntoMain("withsub")
	f.seedRun("withsub", store.RunStatusFinished)
	f.git(path, "-c", "protocol.file.allow=always", "submodule", "update", "--init")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["withsub"] {
		t.Fatal("deleted a worktree carrying a submodule; containment does not cross the gitlink")
	}
	if got := sparedReason(r, "withsub"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	mustExist(t, path, "worktree with submodule")
}

// `git worktree add` never populates submodules, so "declared but not
// initialised" is their normal state in a run worktree. There is no
// working tree and no object of the submodule's own under the directory,
// so there is nothing to lose — and refusing on it made every worktree of
// every repository that merely declares a submodule permanently
// unreclaimable, under the misleading label `unlanded`.
func TestClean_UninitialisedSubmoduleDoesNotBlockCleaning(t *testing.T) {
	f := newCleanFixture(t)
	f.addSubmodule("vendor/lib")

	path := f.addWorktree("declared")
	f.mergeIntoMain("declared")
	f.seedRun("declared", store.RunStatusFinished)

	// Precondition: git does report it, with the not-initialised marker.
	if out := f.git(path, "submodule", "status"); !strings.HasPrefix(strings.TrimSpace(out), "-") {
		t.Fatalf("fixture is not reproducing an uninitialised submodule: %q", out)
	}
	if entries, err := os.ReadDir(filepath.Join(path, "vendor", "lib")); err == nil && len(entries) > 0 {
		t.Fatalf("submodule is populated after all: %d entries", len(entries))
	}

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["declared"] {
		t.Fatalf("a merged, clean worktree was refused over a submodule that was never initialised (spared: %q)",
			sparedReason(r, "declared"))
	}
	mustNotExist(t, path, "worktree with an uninitialised submodule")
}

// A plain clone dropped inside a worktree — a vendored checkout, the
// `.repos/<tool>` habit of keeping a dependency's source beside the code
// that uses it — is invisible to every question asked of the OUTER
// repository: `submodule status` says nothing, and it is normally
// gitignored, so the tree reads clean.
func TestClean_RefusesWorktreeHoldingAnEmbeddedRepo(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("embeds")
	f.mergeIntoMain("embeds")
	f.seedRun("embeds", store.RunStatusFinished)

	// Gitignored, exactly as such a directory normally is, so the outer
	// tree still reads clean.
	mustWrite(t, filepath.Join(f.repo, ".gitignore"), ".repos/\n")
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-m", "ignore .repos")

	embedded := filepath.Join(path, ".repos", "upstream-tool")
	mustMkdir(t, embedded)
	f.initRepo(embedded)
	mustWrite(t, filepath.Join(embedded, "patch.txt"), "unpushed local work\n")
	f.git(embedded, "add", ".")
	f.git(embedded, "commit", "-m", "local fix nobody else has")
	head := f.git(embedded, "rev-parse", "HEAD")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["embeds"] {
		t.Fatalf("deleted a worktree holding an embedded repository; commit %s would be gone", head)
	}
	if got := sparedReason(r, "embeds"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	mustExist(t, filepath.Join(embedded, "patch.txt"), "embedded repo")
}

// The gate state under worktrees/.state is shared across runs and is not
// any run's checkout.
func TestClean_SkipsDotPrefixedEntries(t *testing.T) {
	f := newCleanFixture(t)
	stateDir := filepath.Join(f.store, "worktrees", ".state")
	mustMkdir(t, stateDir)
	mustWrite(t, filepath.Join(stateDir, "app.jar"), "binary")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if r.Scanned != 0 {
		t.Fatalf("scanned %d entries, want 0 — .state must not be a candidate", r.Scanned)
	}
	mustExist(t, stateDir, ".state")
}

// --- "we could not tell, so we must refuse" --------------------------------

func TestClean_ForEachRefFailureIsTreatedAsUnlanded(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	breakGit(t, "for-each-ref")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["merged"] {
		t.Fatal("deleted although git could not answer the containment question")
	}
	if got := sparedReason(r, "merged"); got != skipUnlanded {
		t.Fatalf("spared for %q, want %q", got, skipUnlanded)
	}
	mustExist(t, path, "worktree git could not vouch for")
}

func TestClean_UnreadableStatusIsTreatedAsDirty(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	breakGit(t, "status")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["merged"] {
		t.Fatal("conservative deleted a worktree whose working tree git refused to report")
	}
	if got := sparedReason(r, "merged"); got != skipLevel {
		t.Fatalf("spared for %q, want %q", got, skipLevel)
	}
	mustExist(t, path, "worktree with unreadable status")
}

// "git could not answer" and "git says this is not a worktree" are
// different facts. Collapsing them turns a broken environment — git
// missing from a cron PATH, an unreadable config — into a store full of
// disposable leftovers, at the level that also deletes them.
func TestClean_BrokenGitIsAnErrorNotAStoreFullOfOrphans(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	dir := t.TempDir() // a PATH with no git in it at all
	t.Setenv("PATH", dir)

	_, err := f.runErr(CleanOptions{
		StoreDir: f.store, Level: CleanAggressive, OlderThan: 0, Apply: true,
	})
	if err == nil {
		t.Fatal("a sweep with no usable git reported success")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("error does not say git is the problem: %v", err)
	}
	mustExist(t, path, "worktree under a broken git")
}

// The status re-read closes a window whose width is the whole
// os.RemoveAll. `run` and `resume` hold a per-run lock for the run's
// lifetime; clean must take the same one, or "a live run keeps its
// worktree" is merely likely.
func TestClean_SkipsWorktreeWhoseRunLockIsHeld(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("held")
	f.mergeIntoMain("held")
	f.seedRun("held", store.RunStatusFinished) // terminal: eligible on status

	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	lock, err := s.LockRun(context.Background(), "held")
	if err != nil {
		t.Fatalf("LockRun: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["held"] {
		t.Fatal("deleted a worktree whose run lock another process holds")
	}
	if got := sparedReason(r, "held"); got != skipRunActive {
		t.Fatalf("spared for %q, want %q", got, skipRunActive)
	}
	mustExist(t, path, "locked run")
}

// iterion persists its own per-run checkpoints under refs/iterion/. Not
// looking there reported worktrees as `unlanded` — unrecoverable — when a
// ref did hold their commits. They are this run's bookkeeping though, so
// they lift it to own-branch, never to merged.
func TestClean_IterionCheckpointRefsAreOwnBranchNotUnlanded(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("checkpointed")
	f.seedRun("checkpointed", store.RunStatusFinished)
	head := f.git(path, "rev-parse", "HEAD")
	f.git(f.repo, "update-ref", "refs/iterion/runs/checkpointed/nodes/n/0", head)

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0})
	if got := landingOf(r, "checkpointed"); got != landingOwnBranch {
		t.Fatalf("landing %q, want %q — a checkpoint ref does hold the commits", got, landingOwnBranch)
	}
	if deletedPaths(r)["checkpointed"] {
		t.Error("conservative took a worktree held only by iterion's own checkpoint ref")
	}

	r = f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0})
	if !deletedPaths(r)["checkpointed"] {
		t.Error("aggressive did not take it either; the refs namespace is still unread")
	}
}

// %(objectname) on an annotated tag is the tag OBJECT's id, never the
// commit's, so a tag sitting exactly on HEAD compared unequal and read as
// work built on top — silently dropping an aggressive-only worktree to
// conservative-deletable.
func TestClean_AnnotatedTagOnHeadIsALabelNotAMerge(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("tagged")
	f.seedRun("tagged", store.RunStatusFinished)
	head := f.git(path, "rev-parse", "HEAD")
	f.git(f.repo, "tag", "-a", "v1.0.0", "-m", "release", head)

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if got := landingOf(r, "tagged"); got != landingOwnBranch {
		t.Fatalf("landing %q, want %q — an annotated tag AT the commit is a label", got, landingOwnBranch)
	}
	if deletedPaths(r)["tagged"] {
		t.Fatal("conservative took a worktree whose only ref is an annotated tag on its own HEAD")
	}
	mustExist(t, path, "tagged worktree")
}

// The .claude/ mirror is written BY iterion at run start, not produced by
// the run. Counting it as uncommitted work held merged worktrees back a
// whole level for nothing.
func TestClean_IterionsOwnScaffoldDoesNotCountAsUncommittedWork(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("scaffolded")
	f.mergeIntoMain("scaffolded")
	f.seedRun("scaffolded", store.RunStatusFinished)
	mustMkdir(t, filepath.Join(path, ".claude", "skills"))
	mustWrite(t, filepath.Join(path, ".claude", "skills", "x.md"), "mirrored by iterion\n")

	// Precondition: git does see it.
	if out := f.git(path, "status", "--porcelain"); !strings.Contains(out, ".claude") {
		t.Fatalf("fixture did not produce the scaffold: %q", out)
	}

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["scaffolded"] {
		t.Fatalf("a merged worktree was held back by iterion's own scaffold (spared: %q)",
			sparedReason(r, "scaffolded"))
	}

	// But real uncommitted work still counts.
	f2 := newCleanFixture(t)
	p2 := f2.addWorktree("real")
	f2.mergeIntoMain("real")
	f2.seedRun("real", store.RunStatusFinished)
	mustWrite(t, filepath.Join(p2, "notes.md"), "actual work\n")
	r2 := f2.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r2)["real"] {
		t.Fatal("the scaffold exclusion swallowed genuine uncommitted work")
	}
}

// An operator told only what the current level yields cannot see that the
// next one would free ten times as much.
func TestClean_SparedEntriesCarryTheirSize(t *testing.T) {
	f := newCleanFixture(t)
	p := f.addWorktree("heavy")
	f.mergeIntoMain("heavy")
	f.seedRun("heavy", store.RunStatusFinished)
	mustWrite(t, filepath.Join(p, "big.bin"), strings.Repeat("x", 4096))

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0})
	if got := sparedReason(r, "heavy"); got != skipLevel {
		t.Fatalf("spared for %q, want %q", got, skipLevel)
	}
	for _, wt := range r.Spared {
		if filepath.Base(wt.Path) == "heavy" && wt.Bytes < 4096 {
			t.Fatalf("spared entry reports %d bytes; a zero there reads as nothing to gain", wt.Bytes)
		}
	}
}

// A bare repository has no `.git` at all — it IS the git directory. A
// `.git`-name search walks straight past a vendored mirror or cache whose
// objects exist nowhere else.
func TestClean_RefusesWorktreeHoldingABareRepo(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("bare")
	f.mergeIntoMain("bare")
	f.seedRun("bare", store.RunStatusFinished)

	bare := filepath.Join(path, "vendor", "mirror.git")
	mustMkdir(t, bare)
	f.git(filepath.Dir(bare), "init", "--bare", "mirror.git")
	if !looksLikeGitDir(bare) {
		t.Fatalf("fixture did not produce a bare repo at %s", bare)
	}

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["bare"] {
		t.Fatal("deleted a worktree holding a bare repository")
	}
	if got := sparedReason(r, "bare"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	// Found by the walk, so the walk had already measured it; the contract
	// says a spared entry of this class reports nothing.
	for _, wt := range r.Spared {
		if filepath.Base(wt.Path) == "bare" && wt.Bytes != 0 {
			t.Errorf("a nested-repo reports %d bytes; the contract says 0", wt.Bytes)
		}
	}
	mustExist(t, bare, "bare repo")
}

// The classification is a photograph and the sweep runs for tens of
// seconds. The run lock says nothing about the writer that matters here:
// an operator, an editor, an agent working outside iterion.
func TestClean_RechecksTheTreeImmediatelyBeforeDeleting(t *testing.T) {
	f := newCleanFixture(t)
	for i, id := range []string{"first", "racy"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		f.backdate(id, time.Duration(90-i*10)*24*time.Hour) // first is swept first
	}
	racy := filepath.Join(f.store, "worktrees", "racy")

	// The whole scan runs before the first deletion, so writing while
	// `first` is being removed lands squarely in the window between
	// `racy`'s classification and its deletion.
	real := removeTree
	removeTree = func(p string) error {
		if filepath.Base(p) == "first" {
			mustWrite(t, filepath.Join(racy, "URGENT.md"), "written while the sweep was walking\n")
		}
		return real(p)
	}
	t.Cleanup(func() { removeTree = real })

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["racy"] {
		t.Fatal("deleted a worktree that became dirty after it was classified")
	}
	if got := sparedReason(r, "racy"); got != skipLevel {
		t.Fatalf("spared for %q, want %q", got, skipLevel)
	}
	mustExist(t, filepath.Join(racy, "URGENT.md"), "work written during the sweep")
}

// Porcelain renders a rename as one line, `XY ORIG -> DEST`. Judging it
// on ORIG lets a file moved OUT of the scaffold carry the whole entry
// with it, and the destination — real uncommitted work — is never seen.
func TestClean_ScaffoldExclusionJudgesARenameOnItsDestination(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("renamed")
	mustMkdir(t, filepath.Join(path, ".claude", "skills"))
	mustWrite(t, filepath.Join(path, ".claude", "skills", "s.md"), "mirrored\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "scaffold")
	f.mergeIntoMain("renamed")
	f.seedRun("renamed", store.RunStatusFinished)

	f.git(path, "mv", ".claude/skills/s.md", "DESIGN_NOTES.md")
	mustWrite(t, filepath.Join(path, "DESIGN_NOTES.md"), "mirrored\nand a day of my own notes\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["renamed"] {
		t.Fatal("the scaffold exclusion swallowed a rename out of .claude/ and deleted the work")
	}
	mustExist(t, filepath.Join(path, "DESIGN_NOTES.md"), "renamed-out work")
}

// The exclusion covers the mirror, not everything a run may write beside
// it under .claude/.
func TestClean_OnlyTheSkillsMirrorIsExcludedFromDirty(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("wrote")
	f.mergeIntoMain("wrote")
	f.seedRun("wrote", store.RunStatusFinished)
	mustMkdir(t, filepath.Join(path, ".claude"))
	// Deliberately not a *.local.json: a developer's global gitignore
	// commonly covers those, and this test is about the exclusion in the
	// code, not about the machine it runs on.
	runWritten := filepath.Join(path, ".claude", "notes-from-the-run.md")
	mustWrite(t, runWritten, "the run wrote this itself\n")
	if out := f.git(path, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(out, "notes-from-the-run") {
		t.Skipf("this machine's git ignores the probe file: %q", out)
	}

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["wrote"] {
		t.Fatal("conservative deleted a worktree whose run wrote under .claude/ outside the mirror")
	}
	mustExist(t, runWritten, "run-written file")
}

// %(*objectname) dereferences one level, so a tag on a tag peels to
// another tag object and compares unequal all over again.
func TestClean_TagOfTagOnHeadIsStillALabel(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("doubletag")
	f.seedRun("doubletag", store.RunStatusFinished)
	head := f.git(path, "rev-parse", "HEAD")
	f.git(f.repo, "tag", "-a", "v1", "-m", "one", head)
	f.git(f.repo, "tag", "-a", "v1-signed", "-m", "two", "v1")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if got := landingOf(r, "doubletag"); got != landingOwnBranch {
		t.Fatalf("landing %q, want %q — a tag of a tag on HEAD is still a label", got, landingOwnBranch)
	}
	mustExist(t, path, "double-tagged worktree")
}

// os.RemoveAll succeeds on a path that is already gone, so two sweeps
// would each claim the deletion and its bytes.
func TestClean_DoesNotClaimAWorktreeAnotherSweepAlreadyTook(t *testing.T) {
	f := newCleanFixture(t)
	for i, id := range []string{"first", "contended"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		f.backdate(id, time.Duration(90-i*10)*24*time.Hour)
	}
	contended := filepath.Join(f.store, "worktrees", "contended")

	real := removeTree
	removeTree = func(p string) error {
		if filepath.Base(p) == "first" {
			_ = os.RemoveAll(contended) // the other sweep got there first
		}
		return real(p)
	}
	t.Cleanup(func() { removeTree = real })

	// aggressive, so the pre-delete dirty re-check (which a vanished path
	// also trips) is not what spares it: this is about the guard on the
	// path having already gone.
	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if r.DeletedCount != 1 {
		t.Fatalf("claimed %d deletions; only `first` was actually removed by this sweep", r.DeletedCount)
	}
	if deletedPaths(r)["contended"] {
		t.Fatal("claimed a worktree another sweep had already taken")
	}
	// And it must be accounted for: silently dropping it makes the totals
	// stop adding up, and filing it under `unlanded` would be a verdict
	// about work rather than a statement that it was already gone.
	if got := sparedReason(r, "contended"); got != skipVanished {
		t.Fatalf("reported as %q, want %q", got, skipVanished)
	}
	for _, wt := range r.Spared {
		if filepath.Base(wt.Path) == "contended" && wt.Bytes != 0 {
			t.Errorf("an already-gone entry reports %d bytes; they were reclaimed by the other sweep, "+
				"and showing them reads as space still to gain", wt.Bytes)
		}
	}
	if r.Scanned != len(r.Deleted)+len(r.Spared)+len(r.Failed) {
		t.Errorf("scanned=%d but deleted+spared+failed=%d — an entry went unaccounted for",
			r.Scanned, len(r.Deleted)+len(r.Spared)+len(r.Failed))
	}
	var want int64
	for _, wt := range r.Deleted {
		want += wt.Bytes
	}
	if r.BytesReclaimed != want {
		t.Fatalf("BytesReclaimed = %d, want %d", r.BytesReclaimed, want)
	}
}

// A git that answers `version` but fails per directory — dubious
// ownership, a corrupt object, EACCES on .git — must not read as a
// directory that is not a repository, which aggressive deletes.
func TestClean_PerDirectoryGitFailureIsNotAnOrphan(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("unreadable")
	f.mergeIntoMain("unreadable")
	f.seedRun("unreadable", store.RunStatusFinished)

	breakGit(t, "rev-parse")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if got := landingOf(r, "unreadable"); got == landingOrphan {
		t.Fatal("a git failure that is not `not a git repository` was read as an orphan")
	}
	if deletedPaths(r)["unreadable"] {
		t.Fatal("deleted a worktree git could not answer for")
	}
	mustExist(t, path, "worktree git could not answer for")
}

func TestClean_SubmoduleStatusFailureIsRefused(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("subfail")
	f.mergeIntoMain("subfail")
	f.seedRun("subfail", store.RunStatusFinished)

	breakGit(t, "submodule")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["subfail"] {
		t.Fatal("deleted although git could not say whether a submodule was present")
	}
	if got := sparedReason(r, "subfail"); got != skipUnlanded {
		t.Fatalf("spared for %q, want %q", got, skipUnlanded)
	}
	mustExist(t, path, "worktree with unanswerable submodule status")
}

// The walk is what proves there is no repository hiding in the tree. A
// walk that could not finish has not proved it.
func TestClean_UnreadableSubtreeIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newCleanFixture(t)
	path := f.addWorktree("blind")
	f.mergeIntoMain("blind")
	f.seedRun("blind", store.RunStatusFinished)

	hidden := filepath.Join(path, "hidden")
	mustMkdir(t, hidden)
	mustWrite(t, filepath.Join(hidden, "f"), "x")
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o700) })

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["blind"] {
		t.Fatal("deleted a worktree whose tree could not be fully read")
	}
	if got := sparedReason(r, "blind"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	mustExist(t, path, "worktree with an unreadable subtree")
}

// git suffixes colliding worktree basenames, so `<id>` and `<id>1` may
// live in two different stores — and the unsuffixed one may be the
// operator's. Matching on the recorded gitdir is the only way to know.
func TestClean_DropsTheRegistrationThatNamesTheDeletedPath(t *testing.T) {
	f := newCleanFixture(t)
	// The operator's worktree, registered FIRST so it takes the plain name.
	operator := filepath.Join(f.root, "operator", "shared")
	mustMkdir(t, filepath.Dir(operator))
	f.git(f.repo, "worktree", "add", "-b", "operator-work", operator)
	mustWrite(t, filepath.Join(operator, "staged.txt"), "staged\n")
	f.git(operator, "add", "staged.txt")

	// iterion's worktree, same basename: git gives it a suffixed admin dir.
	f.addWorktree("shared")
	f.mergeIntoMain("shared")
	f.seedRun("shared", store.RunStatusFinished)

	adminRoot := filepath.Join(f.repo, ".git", "worktrees")
	before, err := os.ReadDir(adminRoot)
	if err != nil || len(before) != 2 {
		t.Fatalf("fixture did not produce two registrations: %v (%d)", err, len(before))
	}

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if r.RegistrationsPruned != 1 {
		t.Fatalf("dropped %d registration(s), want exactly 1", r.RegistrationsPruned)
	}
	// The operator's worktree must still work, index and all.
	out, err := f.gitErr(operator, "status", "--porcelain")
	if err != nil {
		t.Fatalf("the operator's worktree is broken after the sweep: %v\n%s", err, out)
	}
	if !strings.Contains(out, "staged.txt") {
		t.Fatalf("the operator's worktree lost its staged work: %q", out)
	}
}

// The dry run must not reach the store either.
func TestClean_DryRunKeepsRunRecordsEvenWithWithRuns(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	r := f.run(CleanOptions{OlderThan: 0, WithRuns: true}) // no Apply
	if len(r.Deleted) != 1 || r.Deleted[0].RunDeleted {
		t.Fatalf("dry run reports a deleted run record: %+v", r.Deleted)
	}
	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := s.LoadRun(context.Background(), "merged"); err != nil {
		t.Fatalf("dry run deleted the run record: %v", err)
	}
}

// The dry run never reaches the lock or the status re-read, so guard 1
// has to hold on its own in the listing an operator reads before
// deciding.
func TestClean_DryRunAlsoSparesNonTerminalRuns(t *testing.T) {
	for _, status := range []store.RunStatus{
		store.RunStatusQueued,
		store.RunStatusPausedWaitingHuman,
		store.RunStatusPausedOperator,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newCleanFixture(t)
			f.addWorktree("live")
			f.mergeIntoMain("live")
			f.seedRun("live", status)

			r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0}) // no Apply
			if r.DeletedCount != 0 {
				t.Fatalf("dry run offers to delete the worktree of a %s run", status)
			}
			if got := sparedReason(r, "live"); got != skipRunActive {
				t.Fatalf("spared for %q, want %q", got, skipRunActive)
			}
		})
	}
}

// A checkpoint ref that CONTAINS the head — a later turn of the same run
// — still is not another line of work adopting it.
func TestClean_ALaterIterionCheckpointIsStillNotAMerge(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("turns")
	f.seedRun("turns", store.RunStatusFinished)
	head := f.git(path, "rev-parse", "HEAD")
	// A later commit, recorded as the run's next checkpoint.
	mustWrite(t, filepath.Join(path, "next.txt"), "turn 2\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "turn 2")
	later := f.git(path, "rev-parse", "HEAD")
	f.git(path, "reset", "--hard", head)
	f.git(f.repo, "update-ref", "refs/iterion/runs/turns/nodes/n/1", later)

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if got := landingOf(r, "turns"); got != landingOwnBranch {
		t.Fatalf("landing %q, want %q — a checkpoint of the same run is not an adoption", got, landingOwnBranch)
	}
	if deletedPaths(r)["turns"] {
		t.Fatal("conservative took a worktree held only by iterion's own later checkpoint")
	}
}

// A leaked lock would make every later sweep a no-op.
func TestClean_ReleasesTheRunLock(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("first")
	f.mergeIntoMain("first")
	f.seedRun("first", store.RunStatusFinished)
	f.addWorktree("second")
	f.mergeIntoMain("second")
	f.seedRun("second", store.RunStatusFinished)

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true, WithRuns: false})
	if r.DeletedCount != 2 {
		t.Fatalf("deleted %d, want 2 — a leaked lock would have stopped the second", r.DeletedCount)
	}
	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	lock, err := s.LockRun(context.Background(), "first")
	if err != nil {
		t.Fatalf("the run lock was not released: %v", err)
	}
	_ = lock.Unlock()
}

// keep-last is a floor on what survives, not a quota on the scan: an
// entry already spared for another reason must not consume a slot.
func TestClean_KeepLastDoesNotSpendSlotsOnAlreadySparedEntries(t *testing.T) {
	f := newCleanFixture(t)
	for i, id := range []string{"old", "mid"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		f.backdate(id, time.Duration(90-i*10)*24*time.Hour)
	}
	// The most recent entry is spared for another reason entirely.
	f.addWorktree("live")
	f.mergeIntoMain("live")
	f.seedRun("live", store.RunStatusRunning)

	r := f.run(CleanOptions{OlderThan: 0, KeepLast: 1, Apply: true})
	if got := sparedReason(r, "mid"); got != skipKeepLast {
		t.Fatalf("mid spared for %q, want %q — keep-last must still hold back a real candidate", got, skipKeepLast)
	}
	if got := sparedReason(r, "live"); got != skipRunActive {
		t.Errorf("live spared for %q, want %q — its real reason must not be masked", got, skipRunActive)
	}
	if !deletedPaths(r)["old"] {
		t.Error("the oldest candidate was not taken")
	}
}

// A commit leaves a CLEAN tree, so re-asking only "is it dirty" waves
// through the one change that creates something to lose — and deleting
// takes the worktree's administrative directory, leaving the new commit
// held by nothing.
func TestClean_SparesAWorktreeThatCommittedDuringTheSweep(t *testing.T) {
	f := newCleanFixture(t)
	for i, id := range []string{"first", "committer"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		f.backdate(id, time.Duration(90-i*10)*24*time.Hour)
	}
	target := filepath.Join(f.store, "worktrees", "committer")

	var newHead string
	real := removeTree
	removeTree = func(p string) error {
		if filepath.Base(p) == "first" {
			mustWrite(t, filepath.Join(target, "day-of-work.md"), "an operator's commit\n")
			f.git(target, "add", ".")
			f.git(target, "commit", "-m", "operator: a day of work")
			newHead = f.git(target, "rev-parse", "HEAD")
			// Land it too, so the LANDING is still `merged` and only the
			// changed HEAD can be what spares the worktree.
			f.git(f.repo, "branch", "-f", "committed-work", newHead)
			f.git(f.repo, "merge", "--no-ff", "-m", "merge the operator's work", "committed-work")
		}
		return real(p)
	}
	t.Cleanup(func() { removeTree = real })

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["committer"] {
		t.Fatal("deleted a worktree whose HEAD moved after it was classified")
	}
	mustExist(t, target, "worktree that committed mid-sweep")
	if got := f.git(f.repo, "cat-file", "-t", newHead); got != "commit" {
		t.Fatalf("the commit made during the sweep is not reachable: %q", got)
	}
}

// A repository can appear inside the tree between classification and
// deletion just as easily as a commit can.
func TestClean_SparesAWorktreeThatGainedANestedRepoDuringTheSweep(t *testing.T) {
	f := newCleanFixture(t)
	// Ignored in the base repo, so both worktrees inherit it and the outer
	// tree stays clean when a repository appears inside.
	mustWrite(t, filepath.Join(f.repo, ".gitignore"), ".repos/\n")
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-m", "ignore .repos")

	for i, id := range []string{"first", "gainer"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		f.backdate(id, time.Duration(90-i*10)*24*time.Hour)
	}
	target := filepath.Join(f.store, "worktrees", "gainer")

	real := removeTree
	removeTree = func(p string) error {
		if filepath.Base(p) == "first" {
			embedded := filepath.Join(target, ".repos", "tool")
			mustMkdir(t, embedded)
			f.initRepo(embedded) // gitignored: the outer tree stays clean
		}
		return real(p)
	}
	t.Cleanup(func() { removeTree = real })

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["gainer"] {
		t.Fatal("deleted a worktree that gained a nested repository during the sweep")
	}
	if got := sparedReason(r, "gainer"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	mustExist(t, filepath.Join(target, ".repos", "tool"), "repo cloned mid-sweep")
}

// A self-contained clone dropped into the pool answers every containment
// question from refs and objects that live inside itself — and they are
// destroyed along with the directory. `merged` means "reachable
// independently of what this worktree holds", which such a repository can
// never satisfy however confidently git answers.
func TestClean_RefusesASelfContainedRepoInThePool(t *testing.T) {
	f := newCleanFixture(t)
	path := filepath.Join(f.store, "worktrees", "cloned")
	f.git(f.root, "clone", "-q", f.repo, path)
	if info, err := os.Stat(filepath.Join(path, ".git")); err != nil || !info.IsDir() {
		t.Skipf("clone did not produce a .git directory: %v", err)
	}
	f.seedRun("cloned", store.RunStatusFinished)

	// Work that exists in this clone and nowhere else, with HEAD left on a
	// commit another of its own branches contains — the exact shape that
	// reads as `merged`.
	f.git(path, "checkout", "-q", "-b", "feature")
	mustWrite(t, filepath.Join(path, "precious.txt"), "exists in no other object store\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "precious")
	precious := f.git(path, "rev-parse", "HEAD")
	f.git(path, "checkout", "-q", "main")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["cloned"] {
		t.Fatalf("deleted a self-contained repository; commit %s existed nowhere else", precious)
	}
	if got := sparedReason(r, "cloned"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	mustExist(t, path, "self-contained repo")
	// The commit lives only in this clone's own object store.
	if out, err := f.gitErr(f.repo, "cat-file", "-t", precious); err == nil && strings.TrimSpace(out) == "commit" {
		t.Fatal("fixture is not isolating the commit: the parent repo holds it too")
	}
	if out := f.git(path, "cat-file", "-t", precious); strings.TrimSpace(out) != "commit" {
		t.Fatalf("the clone lost its own commit: %q", out)
	}
}

// --path-format=absolute needs git 2.31. On an older git the option is
// rejected and the bare form answers relatively, so a guard buried in an
// `err == nil` would simply vanish — and the self-contained clone it
// protects would read `merged` again.
func TestClean_RefusesASelfContainedRepoOnGitWithoutPathFormat(t *testing.T) {
	f := newCleanFixture(t)
	path := filepath.Join(f.store, "worktrees", "cloned")
	f.git(f.root, "clone", "-q", f.repo, path)
	f.seedRun("cloned", store.RunStatusFinished)

	rejectPathFormat(t)

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["cloned"] {
		t.Fatal("a git too old for --path-format made the self-contained-repo guard disappear")
	}
	if got := sparedReason(r, "cloned"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	mustExist(t, path, "self-contained repo under an old git")
}

// And a git that cannot answer the question at all must refuse, not
// classify: it is the only thing standing between such a clone and its
// own destruction.
func TestClean_UnanswerableCommonDirIsRefused(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	breakGitArg(t, "--git-common-dir")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["merged"] {
		t.Fatal("deleted although git could not say where the repository lives")
	}
	mustExist(t, path, "worktree whose common dir is unknown")
}

// `IsTerminal()` answers whether a poller should stop refreshing, not
// whether anything still owns the checkout. `iterion resume` restarts a
// failed_resumable or a cancelled run in its existing worktree — sweeping
// it destroys the resume while every other guard nods along, because the
// commits are merged and nothing is lost except the ability to continue.
func TestClean_SparesWorktreesOfResumableRuns(t *testing.T) {
	// paused_operator is dormant but not terminal, so the ordinary guard
	// already holds it and --include-resumable must not reach it: only a
	// run a poller would call finished is released here.
	for _, status := range []store.RunStatus{
		store.RunStatusFailedResumable,
		store.RunStatusCancelled,
	} {
		t.Run(string(status), func(t *testing.T) {
			f := newCleanFixture(t)
			path := f.addWorktree("resumable")
			f.mergeIntoMain("resumable") // merged and clean: eligible on every other count
			f.seedRun("resumable", status)

			// Dry run first: it never reaches the deletion-time gate, so
			// this is the only thing pinning the classification — and it
			// is the listing an operator reads before typing --apply.
			dry := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0})
			if dry.DeletedCount != 0 {
				t.Fatalf("the dry run offers to delete a %s run's worktree", status)
			}
			if got := sparedReason(dry, "resumable"); got != skipResumable {
				t.Fatalf("dry run spared for %q, want %q", got, skipResumable)
			}

			r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
			if deletedPaths(r)["resumable"] {
				t.Fatalf("deleted the worktree of a %s run, which `iterion resume` restarts in place", status)
			}
			if got := sparedReason(r, "resumable"); got != skipResumable {
				t.Fatalf("spared for %q, want %q", got, skipResumable)
			}
			// The size is what an operator weighs against giving up the
			// resume, so it has to be reported.
			for _, wt := range r.Spared {
				if wt.SkipReason == skipResumable && wt.Bytes == 0 {
					t.Error("a resumable worktree reports no size; nothing to weigh the resume against")
				}
			}
			mustExist(t, path, "resumable run's worktree")

			// --include-resumable is how the operator says the resume is
			// not wanted.
			r = f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true, IncludeResumable: true})
			if !deletedPaths(r)["resumable"] {
				t.Fatalf("--include-resumable did not release a %s run's worktree (spared: %q)",
					status, sparedReason(r, "resumable"))
			}
			mustNotExist(t, path, "resumable worktree under --include-resumable")
		})
	}
}

// A repository whose `.git` is a POINTER FILE — a linked worktree of some
// other repository, a `clone --separate-git-dir` — is as invisible to the
// outer repository's questions as one with a `.git` directory.
func TestClean_RefusesWorktreeHoldingARepoWhoseGitIsAFile(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("holds")
	mustWrite(t, filepath.Join(f.repo, ".gitignore"), "vendor/\n")
	f.git(f.repo, "add", ".gitignore")
	f.git(f.repo, "commit", "-m", "ignore vendor")
	f.mergeIntoMain("holds")
	f.seedRun("holds", store.RunStatusFinished)

	// Another repository, checked out INSIDE the worktree as a linked
	// worktree: its `.git` is a file, and its commits live in that other
	// repository's object store.
	other := filepath.Join(f.root, "other")
	mustMkdir(t, other)
	f.initRepo(other)
	embedded := filepath.Join(path, "vendor", "lib")
	mustMkdir(t, filepath.Dir(embedded))
	f.git(other, "worktree", "add", embedded)
	if info, err := os.Stat(filepath.Join(embedded, ".git")); err != nil || info.IsDir() {
		t.Skipf("linked worktree did not produce a .git file: %v", err)
	}

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["holds"] {
		t.Fatal("deleted a worktree holding a repository whose .git is a pointer file")
	}
	if got := sparedReason(r, "holds"); got != skipNested {
		t.Fatalf("spared for %q, want %q", got, skipNested)
	}
	mustExist(t, embedded, "embedded linked worktree")
}

// The lock has to be HELD across the removal, not merely taken: the
// window it closes is the whole os.RemoveAll.
func TestClean_HoldsTheRunLockAcrossTheRemoval(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("locked")
	f.mergeIntoMain("locked")
	f.seedRun("locked", store.RunStatusFinished)

	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	heldDuringRemoval := false
	real := removeTree
	removeTree = func(p string) error {
		if lock, err := s.LockRun(context.Background(), "locked"); err != nil {
			heldDuringRemoval = true
		} else {
			_ = lock.Unlock()
		}
		return real(p)
	}
	t.Cleanup(func() { removeTree = real })

	f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if !heldDuringRemoval {
		t.Fatal("the run lock was not held while the worktree was being removed")
	}
}

// The registration holds the worktree's index. Dropping it after a
// removal that failed leaves a half-deleted checkout that git can no
// longer open — the precise damage the targeted prune exists to avoid.
func TestClean_DoesNotDropTheRegistrationOfAFailedDeletion(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("doomed")
	f.mergeIntoMain("doomed")
	f.seedRun("doomed", store.RunStatusFinished)

	real := removeTree
	removeTree = func(string) error { return errors.New("simulated: another owner's file") }
	t.Cleanup(func() { removeTree = real })

	r, err := f.runErr(CleanOptions{
		StoreDir: f.store, Level: CleanConservative, OlderThan: 0, Apply: true,
	})
	if err == nil {
		t.Fatal("the failed deletion did not surface an error")
	}
	if r.RegistrationsPruned != 0 {
		t.Fatalf("dropped %d registration(s) for a deletion that failed", r.RegistrationsPruned)
	}
	mustExist(t, filepath.Join(f.repo, ".git", "worktrees", "doomed"), "registration of a failed deletion")
}

// A worktree that never had a run record must not acquire a deletion
// tombstone: DeleteRun creates the run directory before marking it, and
// the marker makes every later write to that id fail.
func TestClean_WithRunsDoesNotInventRecordsForOrphans(t *testing.T) {
	f := newCleanFixture(t)
	orphan := filepath.Join(f.store, "worktrees", "stale")
	mustMkdir(t, orphan)
	mustWrite(t, filepath.Join(orphan, "leftover.txt"), "junk\n")
	mustMkdir(t, filepath.Join(f.store, "runs"))

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true, WithRuns: true})
	if !deletedPaths(r)["stale"] {
		t.Fatalf("the orphan was not swept (spared: %q)", sparedReason(r, "stale"))
	}
	for _, wt := range r.Deleted {
		if wt.RunDeleted {
			t.Error("reported a run record as deleted for a worktree that never had one")
		}
	}
	if _, err := os.Stat(filepath.Join(f.store, "runs", "stale")); !os.IsNotExist(err) {
		t.Fatal("--with-runs left a tombstone for a run that never existed")
	}
}

// Inspecting a directory must not turn it into a managed store: that is
// what a later `iterion` invocation reads to decide where its runs live.
func TestClean_DryRunDoesNotProvisionTheStore(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "worktrees"))

	var buf bytes.Buffer
	if err := RunClean(CleanOptions{
		StoreDir: dir, Level: CleanConservative, OlderThan: 0,
	}, &Printer{W: &buf, Format: OutputJSON}); err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs")); !os.IsNotExist(err) {
		t.Error("a dry run created runs/ — enough to promote the directory to a managed store")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Error("a dry run wrote a .gitignore into its target")
	}
}

// The re-derivation spends several git calls and a full walk. A sweep
// that took the directory in that gap must not be claimed — nor its
// bytes, which the other sweep reclaimed.
func TestClean_DoesNotClaimAWorktreeTakenDuringTheReDerivation(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("racy")
	f.mergeIntoMain("racy")
	f.seedRun("racy", store.RunStatusFinished)

	afterEligibility = func(p string) { _ = os.RemoveAll(p) }
	t.Cleanup(func() { afterEligibility = func(string) {} })
	_ = duringEligibility

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if r.DeletedCount != 0 || r.BytesReclaimed != 0 {
		t.Fatalf("claimed %d deletion(s) and %d bytes for a directory another sweep took",
			r.DeletedCount, r.BytesReclaimed)
	}
	if got := sparedReason(r, "racy"); got != skipVanished {
		t.Fatalf("reported as %q, want %q", got, skipVanished)
	}
	for _, wt := range r.Spared {
		if filepath.Base(wt.Path) == "racy" && wt.Bytes != 0 {
			t.Errorf("reports %d bytes already reclaimed by the other sweep", wt.Bytes)
		}
	}
}

// A directory that vanishes WHILE the verdict is being re-derived makes
// git fall silent, and silence reads as `unlanded` — this command's alarm
// verdict. An operator would go hunting for work that was never lost.
func TestClean_AVanishedWorktreeIsNotReportedUnlanded(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("racy")
	f.mergeIntoMain("racy")
	f.seedRun("racy", store.RunStatusFinished)

	duringEligibility = func(p string) { _ = os.RemoveAll(p) }
	t.Cleanup(func() { duringEligibility = func(string) {} })

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if got := sparedReason(r, "racy"); got != skipVanished {
		t.Fatalf("reported as %q, want %q — it was taken, not judged", got, skipVanished)
	}
	if r.DeletedCount != 0 || r.BytesReclaimed != 0 {
		t.Errorf("claimed %d deletion(s), %d bytes", r.DeletedCount, r.BytesReclaimed)
	}
}

// Every answer git gives is absolute; a --store-dir does not have to be,
// and `--store-dir .iterion` is the documented incantation for the
// project-local layout. Comparing the two then fails for every worktree
// at once: all of them read `orphan`, which aggressive deletes, and the
// nested-repo guard cannot even form a relative path.
func TestClean_RelativeStoreDirClassifiesLikeAnAbsoluteOne(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)
	f.addWorktree("nowhere") // detached, never promoted
	f.seedRun("nowhere", store.RunStatusFinished)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(filepath.Dir(f.store)); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	r := f.run(CleanOptions{StoreDir: filepath.Base(f.store), Level: CleanAggressive, OlderThan: 0, Apply: true})
	if got := landingOf(r, "nowhere"); got != landingNowhere {
		t.Fatalf("under a relative --store-dir, landing = %q, want %q — guard 2 must still hold", got, landingNowhere)
	}
	if deletedPaths(r)["nowhere"] {
		t.Fatal("a relative --store-dir let an unlanded worktree be deleted")
	}
	if got := landingOf(r, "merged"); got != landingMerged {
		t.Errorf("merged worktree reads %q under a relative --store-dir", got)
	}
	mustExist(t, filepath.Join(f.store, "worktrees", "nowhere"), "unlanded under a relative store dir")
}

// The mirror's markers sit at an exact depth. A run that names a skill
// `.iterion-managed` must not vanish from the tree's dirtiness.
func TestClean_ManagedDirIsMatchedAtItsDepthOnly(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("deepname")
	deep := filepath.Join(path, ".claude", "skills", "mine", ".iterion-managed")
	mustMkdir(t, deep)
	mustWrite(t, filepath.Join(deep, "notes.md"), "committed\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "a skill of the run's own")
	f.mergeIntoMain("deepname")
	f.seedRun("deepname", store.RunStatusFinished)
	// Tracked and modified: the untracked-directory rule cannot apply, so
	// only a segment match at any depth could hide it.
	mustWrite(t, filepath.Join(deep, "notes.md"), "a day of the run's own notes\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["deepname"] {
		t.Fatal("a nested .iterion-managed/ was matched as iterion's bookkeeping")
	}
	mustExist(t, filepath.Join(deep, "notes.md"), "run-written notes")
}

// The fallback branch of gitCommonDir must classify an ORDINARY linked
// worktree correctly too — a test that only ever exercises it on a
// self-contained clone proves nothing about the common case.
func TestClean_OrdinaryWorktreeStillClassifiesOnGitWithoutPathFormat(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	rejectPathFormat(t)

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if got := landingOf(r, "merged"); got != landingMerged {
		t.Fatalf("landing = %q on a git without --path-format, want %q", got, landingMerged)
	}
	if !deletedPaths(r)["merged"] {
		t.Fatalf("an ordinary linked worktree was refused on an older git (spared: %q)",
			sparedReason(r, "merged"))
	}
	mustNotExist(t, path, "ordinary worktree on an older git")
	if r.RegistrationsPruned != 1 {
		t.Errorf("dropped %d registration(s), want 1 — the fallback path must still locate the repo",
			r.RegistrationsPruned)
	}
}

// git answered --show-toplevel for THIS directory, so it does know the
// worktree: an unresolvable HEAD — an unborn branch, a `checkout --orphan`
// an agent left behind — is "we could not tell", not "this is not a
// worktree". The difference decides whether a tree full of uncommitted
// work is deletable at aggressive.
func TestClean_UnresolvableHeadIsNotAnOrphan(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("unborn")
	f.seedRun("unborn", store.RunStatusFinished)
	f.git(path, "checkout", "-q", "--orphan", "fresh-start")
	mustWrite(t, filepath.Join(path, "precious.txt"), "a day of uncommitted work\n")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if got := landingOf(r, "unborn"); got == landingOrphan {
		t.Fatalf("landing = %q: only HEAD was unresolvable, and git knows this directory", got)
	}
	if deletedPaths(r)["unborn"] {
		t.Fatal("deleted a worktree whose HEAD git could not resolve")
	}
	mustExist(t, filepath.Join(path, "precious.txt"), "work under an unresolvable HEAD")
}

// The mirror only lays down files it owns and preserves any it finds
// diverged, so a TRACKED file's diff under one of its directories came
// from the repository, not from iterion.
func TestClean_TrackedFilesUnderTheMirrorStillCountAsWork(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("versioned")
	mustMkdir(t, filepath.Join(path, ".claude", "agents"))
	mustWrite(t, filepath.Join(path, ".claude", "agents", "reviewer.md"), "committed config\n")
	mustWrite(t, filepath.Join(path, ".claude", "settings.json"), "{}\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "version the project's claude config")
	f.mergeIntoMain("versioned")
	f.seedRun("versioned", store.RunStatusFinished)

	// The run's deliverable: a change to those versioned files.
	for _, rel := range []string{".claude/agents/reviewer.md", ".claude/settings.json"} {
		t.Run(rel, func(t *testing.T) {
			mustWrite(t, filepath.Join(path, filepath.FromSlash(rel)), "the run rewrote this\n")
			t.Cleanup(func() { f.git(path, "checkout", "--", ".") })

			r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
			if deletedPaths(r)["versioned"] {
				t.Fatalf("a tracked %s was mistaken for iterion's mirror", rel)
			}
			mustExist(t, filepath.Join(path, filepath.FromSlash(rel)), "modified tracked config")
		})
	}
}

// `.iterion-managed/` under `.claude/` is iterion's bookkeeping about what
// it mirrored. The mirror rewrites it every run, tracked or not, so it is
// nobody's work at any status.
func TestClean_IterionManagedBookkeepingIsNeverWork(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("markers")
	marker := ".claude/skills/.iterion-managed/s.SKILL.md.sha256"
	mustMkdir(t, filepath.Dir(filepath.Join(path, filepath.FromSlash(marker))))
	mustWrite(t, filepath.Join(path, filepath.FromSlash(marker)), "deadbeef\n")
	f.git(path, "add", ".")
	f.git(path, "commit", "-m", "version the markers")
	f.mergeIntoMain("markers")
	f.seedRun("markers", store.RunStatusFinished)

	// A new bundle refreshes the marker in place.
	mustWrite(t, filepath.Join(path, filepath.FromSlash(marker)), "cafebabe\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["markers"] {
		t.Fatalf("iterion's own marker counted as the run's work (spared: %q)", sparedReason(r, "markers"))
	}
}

// `.claude/settings.json` is rewritten in place by iterion; `.orig`,
// `.bak` and `.rej` beside it are a failed merge or an editor's backup.
func TestClean_ScaffoldFilesAreMatchedExactlyNotByPrefix(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("backup")
	f.mergeIntoMain("backup")
	f.seedRun("backup", store.RunStatusFinished)
	mustMkdir(t, filepath.Join(path, ".claude"))
	mustWrite(t, filepath.Join(path, ".claude", "settings.json.orig"), "the pre-merge settings someone needs\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["backup"] {
		t.Fatal("a .orig beside settings.json was swallowed by a prefix match")
	}
	mustExist(t, filepath.Join(path, ".claude", "settings.json.orig"), "merge backup")
}

// The mirror is iterion's, wherever the run's own tree puts a `.claude/`
// of its own: a scaffold path is anchored at the worktree root.
func TestClean_ANestedClaudeDirectoryIsNotTheMirror(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("subproject")
	f.mergeIntoMain("subproject")
	f.seedRun("subproject", store.RunStatusFinished)
	mustMkdir(t, filepath.Join(path, "apps", "web", ".claude", "skills"))
	mustWrite(t, filepath.Join(path, "apps", "web", ".claude", "skills", "s.md"), "the run scaffolded a sub-project\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["subproject"] {
		t.Fatal("a .claude/ nested inside the tree was counted as iterion's own mirror")
	}
	mustExist(t, filepath.Join(path, "apps", "web", ".claude", "skills", "s.md"), "sub-project scaffold")
}

// iterion's mirror does not land in .claude/skills/ alone: plugin
// contributions go to commands/ and agents/, and the hooks injector
// writes settings.json and its sidecar.
func TestClean_TheWholeMirrorIsExcludedFromDirty(t *testing.T) {
	for _, rel := range []string{
		".claude/skills/s.md",
		".claude/commands/c.md",
		".claude/agents/a.md",
		".claude/.iterion-managed/plugin-hooks.json",
		".claude/settings.json",
	} {
		t.Run(rel, func(t *testing.T) {
			f := newCleanFixture(t)
			path := f.addWorktree("mirrored")
			f.mergeIntoMain("mirrored")
			f.seedRun("mirrored", store.RunStatusFinished)
			mustMkdir(t, filepath.Dir(filepath.Join(path, rel)))
			mustWrite(t, filepath.Join(path, rel), "written by iterion\n")

			r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
			if !deletedPaths(r)["mirrored"] {
				t.Fatalf("iterion's own %s was counted as the run's uncommitted work (spared: %q)",
					rel, sparedReason(r, "mirrored"))
			}
		})
	}
}

// git quotes a path containing " -> ", so cutting the raw line lands
// inside the quoted source and hands back a fragment of it as the
// destination.
func TestClean_RenameSplitStepsOverAQuotedSource(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("quoted")
	// The arrow sits BEFORE something that looks like the scaffold, so a
	// naive cut at the first " -> " hands back `.claude/skills/…` and the
	// entry passes for iterion's own litter. git quotes the whole source.
	weird := "foo -> .claude/skills/bar"
	mustMkdir(t, filepath.Dir(filepath.Join(path, weird)))
	if err := os.WriteFile(filepath.Join(path, weird), []byte("tracked\n"), 0o644); err != nil {
		t.Skipf("this filesystem rejects the probe filename: %v", err)
	}
	if _, err := f.gitErr(path, "add", "--", weird); err != nil {
		t.Skip("git rejects the probe filename")
	}
	f.git(path, "commit", "-m", "add a path containing an arrow")
	f.mergeIntoMain("quoted")
	f.seedRun("quoted", store.RunStatusFinished)

	f.git(path, "mv", "--", weird, "REAL-WORK.md")
	mustWrite(t, filepath.Join(path, "REAL-WORK.md"), "tracked\nplus a day of my own\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if deletedPaths(r)["quoted"] {
		t.Fatal("a quoted rename source was mistaken for the destination and real work was deleted")
	}
	mustExist(t, filepath.Join(path, "REAL-WORK.md"), "renamed work")
}

// The sweep must delete THROUGH removeAllForce, not merely have it
// available: the read-only module cache is the common case, not a corner.
func TestClean_SweepRemovesThroughTheForcingRemover(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newCleanFixture(t)
	path := f.addWorktree("modcache")
	mustWrite(t, filepath.Join(path, ".gitignore"), "go/\n")
	f.git(path, "add", ".gitignore")
	f.git(path, "commit", "-m", "ignore the module cache")
	f.mergeIntoMain("modcache")
	f.seedRun("modcache", store.RunStatusFinished)

	cache := filepath.Join(path, "go", "pkg", "mod", "toolchain")
	mustMkdir(t, cache)
	mustWrite(t, filepath.Join(cache, "go"), "binary")
	if err := os.Chmod(cache, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(cache, 0o700) })

	r, err := f.runErr(CleanOptions{
		StoreDir: f.store, Level: CleanConservative, OlderThan: 0, Apply: true,
	})
	if err != nil {
		t.Fatalf("the sweep failed on a tree holding a read-only directory: %v", err)
	}
	if r.FailedCount != 0 {
		t.Fatalf("FailedCount = %d, want 0", r.FailedCount)
	}
	mustNotExist(t, path, "worktree with a read-only module cache")
}

// An inherited GIT_DIR makes git answer for a different repository while
// --show-toplevel still names this directory, so the identity guard
// passes and a foreign repo's refs decide this worktree's fate.
func TestClean_IgnoresAnInheritedGitDir(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("unlanded") // detached, never promoted
	f.seedRun("unlanded", store.RunStatusFinished)

	// A second repository whose main is far ahead of anything here.
	other := filepath.Join(f.root, "other")
	mustMkdir(t, other)
	f.initRepo(other)
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["unlanded"] {
		t.Fatal("an inherited GIT_DIR let a foreign repository's refs decide this worktree's fate")
	}
	mustExist(t, path, "worktree judged under an inherited GIT_DIR")
}

// A store reached through a symlink must classify identically, or the
// whole pool reads as `orphan` — spared at conservative, and deleted at
// aggressive without git ever having been consulted.
func TestClean_ClassifiesTheSameThroughASymlinkedStore(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	link := filepath.Join(t.TempDir(), "store-link")
	if err := os.Symlink(f.store, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	r := f.run(CleanOptions{StoreDir: link, Level: CleanConservative, OlderThan: 0})
	if got := landingOf(r, "merged"); got != landingMerged {
		t.Fatalf("landing through a symlinked store = %q, want %q", got, landingMerged)
	}
}

// The counts and the evidence are what an operator reads before --apply.
func TestClean_ReportsIgnoredCountAndContainmentEvidence(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("evidence")
	mustWrite(t, filepath.Join(path, ".gitignore"), "build/\n")
	f.git(path, "add", ".gitignore")
	f.git(path, "commit", "-m", "ignore build")
	f.mergeIntoMain("evidence")
	f.seedRun("evidence", store.RunStatusFinished)
	mustMkdir(t, filepath.Join(path, "build"))
	mustWrite(t, filepath.Join(path, "build", "out.bin"), "artifact")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0})
	if len(r.Deleted) != 1 {
		t.Fatalf("expected one candidate, got %d", len(r.Deleted))
	}
	if r.Deleted[0].IgnoredEntries == 0 {
		t.Error("gitignored paths were not counted; the operator cannot see what goes")
	}
	if len(r.Deleted[0].ContainedBy) == 0 {
		t.Error("contained_by is empty; the evidence for `merged` is not reported")
	}
	for _, ref := range r.Deleted[0].ContainedBy {
		if strings.TrimSpace(ref) == "" {
			t.Error("contained_by holds an empty ref name")
		}
	}
}

// --- the levels ------------------------------------------------------------

// Landing is decided by whether a ref was BUILT UPON the commits or
// merely points at them — which is what "merged" means, and what a
// name-based comparison of the worktree's own branch could never express
// on a detached checkout.
func TestClean_LevelsFollowWhatGitCanProve(t *testing.T) {
	f := newCleanFixture(t)

	f.addWorktree("merged") // merged, clean -> conservative
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	dirty := f.addWorktree("merged-dirty") // merged, dirty -> moderate
	f.mergeIntoMain("merged-dirty")
	mustWrite(t, filepath.Join(dirty, "scratch.txt"), "uncommitted\n")
	f.seedRun("merged-dirty", store.RunStatusFinished)

	f.addWorktree("promoted") // promoted only -> own-branch -> aggressive
	f.promote("promoted")
	f.seedRun("promoted", store.RunStatusFinished)

	orphan := filepath.Join(f.store, "worktrees", "stale") // orphan -> aggressive
	mustMkdir(t, orphan)
	mustWrite(t, filepath.Join(orphan, "leftover.txt"), "junk\n")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if got := landingOf(r, "merged"); got != landingMerged {
		t.Errorf("merged landing = %q, want %q", got, landingMerged)
	}
	if got := landingOf(r, "promoted"); got != landingOwnBranch {
		t.Errorf("promoted-only landing = %q, want %q — a ref AT the commit is not work built upon it", got, landingOwnBranch)
	}
	got := deletedPaths(r)
	if !got["merged"] {
		t.Error("conservative did not take a merged, clean worktree")
	}
	for _, spared := range []string{"merged-dirty", "promoted", "stale"} {
		if got[spared] {
			t.Errorf("conservative took %q", spared)
		}
		if reason := sparedReason(r, spared); reason != skipLevel {
			t.Errorf("%s spared for %q, want %q", spared, reason, skipLevel)
		}
	}

	// --apply, not dry run: the pre-deletion re-check is only on the
	// apply path, and a guard that spares everything there would leave
	// the dry run looking perfectly healthy.
	r = f.run(CleanOptions{Level: CleanModerate, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["merged-dirty"] {
		t.Errorf("moderate --apply did not take a merged, dirty worktree (spared: %q)",
			sparedReason(r, "merged-dirty"))
	}
	if deletedPaths(r)["promoted"] || deletedPaths(r)["stale"] {
		t.Error("moderate took own-branch or orphan")
	}
	mustNotExist(t, dirty, "dirty merged worktree at moderate")

	r = f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["promoted"] || !deletedPaths(r)["stale"] {
		t.Errorf("aggressive --apply did not take own-branch and orphan (promoted: %q, stale: %q)",
			sparedReason(r, "promoted"), sparedReason(r, "stale"))
	}
	mustNotExist(t, orphan, "orphan at aggressive")
}

// Taking an own-branch worktree is only defensible because the commits
// stay reachable from the ref in the parent repository.
func TestClean_AggressiveTakesOwnBranchAndCommitsSurvive(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("solo")
	f.promote("solo")
	f.seedRun("solo", store.RunStatusFinished)
	head := f.git(path, "rev-parse", "HEAD")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["solo"] {
		t.Fatal("aggressive did not take an own-branch worktree")
	}
	mustNotExist(t, path, "deleted worktree")
	if got := f.git(f.repo, "rev-parse", "iterion/run/solo"); got != head {
		t.Fatalf("branch tip is %s, want the deleted worktree's HEAD %s", got, head)
	}
}

// --- the report must match the disk ---------------------------------------

// Assertions on the report alone cannot see a deletion that removes the
// wrong path: a sweep that reports correctly and deletes the parent
// directory would pass every report-only test.
func TestClean_SparedSurviveOnDiskWhenSomethingIsDeleted(t *testing.T) {
	f := newCleanFixture(t)

	f.addWorktree("takeme") // the one that must go
	f.mergeIntoMain("takeme")
	f.seedRun("takeme", store.RunStatusFinished)

	f.addWorktree("live") // spared: run-active
	f.mergeIntoMain("live")
	f.seedRun("live", store.RunStatusRunning)

	f.addWorktree("solo") // spared: needs-higher-level
	f.promote("solo")
	f.seedRun("solo", store.RunStatusFinished)

	f.addWorktree("nowhere") // spared: unlanded
	f.seedRun("nowhere", store.RunStatusFailed)

	stateDir := filepath.Join(f.store, "worktrees", ".state")
	mustMkdir(t, stateDir)
	mustWrite(t, filepath.Join(stateDir, "app.jar"), "binary")

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["takeme"] || r.DeletedCount != 1 {
		t.Fatalf("expected exactly takeme deleted, got %v", deletedPaths(r))
	}
	mustNotExist(t, filepath.Join(f.store, "worktrees", "takeme"), "deleted worktree")
	for _, survivor := range []string{"live", "solo", "nowhere", ".state"} {
		mustExist(t, filepath.Join(f.store, "worktrees", survivor), "spared "+survivor)
	}
}

// --- registration hygiene --------------------------------------------------

// Deleting the directory must drop this worktree's registration — and
// only this one. `git worktree prune` would also drop the registration of
// any worktree whose path is momentarily absent (an unmounted volume, a
// stopped container's bind mount), discarding its index and its staged
// work.
func TestClean_DropsOwnRegistrationAndSparesAbsentForeignOnes(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("mine")
	f.mergeIntoMain("mine")
	f.seedRun("mine", store.RunStatusFinished)

	// A worktree of the same repo that belongs to the operator, currently
	// not present at its recorded path.
	foreign := filepath.Join(f.root, "foreign")
	f.git(f.repo, "worktree", "add", "-b", "operator-work", foreign)
	mustWrite(t, filepath.Join(foreign, "staged.txt"), "staged work\n")
	f.git(foreign, "add", "staged.txt")
	hidden := filepath.Join(f.root, ".foreign-unmounted")
	if err := os.Rename(foreign, hidden); err != nil {
		t.Fatalf("hide foreign worktree: %v", err)
	}

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if r.RegistrationsPruned != 1 {
		t.Fatalf("dropped %d registration(s), want 1", r.RegistrationsPruned)
	}
	mustNotExist(t, filepath.Join(f.repo, ".git", "worktrees", "mine"), "own registration")
	mustExist(t, filepath.Join(f.repo, ".git", "worktrees", "foreign"), "foreign registration")

	// Bring it back: it must still be a usable worktree with its index.
	if err := os.Rename(hidden, foreign); err != nil {
		t.Fatalf("restore foreign worktree: %v", err)
	}
	out, err := f.gitErr(foreign, "status", "--porcelain")
	if err != nil {
		t.Fatalf("foreign worktree is broken after the sweep: %v\n%s", err, out)
	}
	if !strings.Contains(out, "staged.txt") {
		t.Fatalf("foreign worktree lost its staged work: %q", out)
	}
}

// --- failure handling ------------------------------------------------------

// A read-only subtree is not a reason to give up half way. The Go module
// cache is laid down at 0555 by the go tool, so any worktree that fetched
// a module holds hundreds of such directories: a plain os.RemoveAll walks
// in, destroys everything up to the first one, and fails — leaving a
// mangled multi-gigabyte tree that every later sweep re-mangles.
func TestClean_RemovesTreesHoldingReadOnlyDirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	tree := filepath.Join(root, "worktree")
	modcache := filepath.Join(tree, "go", "pkg", "mod", "golang.org", "toolchain")
	mustMkdir(t, modcache)
	mustWrite(t, filepath.Join(tree, "keep.txt"), "x")
	mustWrite(t, filepath.Join(modcache, "go"), "binary")
	if err := os.Chmod(modcache, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(modcache, 0o700) })

	if err := os.RemoveAll(tree); err == nil {
		t.Skip("this platform lets RemoveAll through a read-only directory")
	}
	if err := removeAllForce(tree); err != nil {
		t.Fatalf("removeAllForce: %v", err)
	}
	mustNotExist(t, tree, "tree with a read-only subtree")
}

// A failed deletion must not abort the sweep: returning early strands the
// deletions already made with no report at all.
func TestClean_ContinuesAndReportsAfterAFailedDeletion(t *testing.T) {
	f := newCleanFixture(t)
	for i, id := range []string{"aaa", "bbb", "ccc"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		f.backdate(id, time.Duration(90-i)*24*time.Hour) // aaa oldest, ccc newest
	}
	// bbb sits in the MIDDLE of the sweep order, so "the sweep carried on"
	// is what the assertions below actually observe.
	failing := filepath.Join(f.store, "worktrees", "bbb")
	real := removeTree
	removeTree = func(p string) error {
		if p == failing {
			return errors.New("simulated: another owner's file")
		}
		return real(p)
	}
	t.Cleanup(func() { removeTree = real })

	r, err := f.runErr(CleanOptions{
		StoreDir: f.store, Level: CleanConservative, OlderThan: 0, Apply: true,
	})
	if err == nil {
		t.Fatal("a failed deletion did not surface an error")
	}
	if r.Scanned != 3 {
		t.Fatalf("scanned %d, want 3 — the report must be emitted despite the failure", r.Scanned)
	}
	if r.FailedCount != 1 || len(r.Failed) != 1 {
		t.Fatalf("FailedCount=%d len(Failed)=%d, want 1 and 1", r.FailedCount, len(r.Failed))
	}
	// A machine consumer reading deleted_count must see deletions, not
	// attempts: the failure belongs in its own list.
	if r.DeletedCount != 2 {
		t.Errorf("DeletedCount = %d, want 2 — failures must not be counted as deletions", r.DeletedCount)
	}
	// ccc comes after bbb in the sweep order and must still have been done.
	mustNotExist(t, filepath.Join(f.store, "worktrees", "aaa"), "first deletion")
	mustNotExist(t, filepath.Join(f.store, "worktrees", "ccc"), "deletion after the failure")
	failed := r.Failed[0]
	if filepath.Base(failed.Path) != "bbb" {
		t.Errorf("the failed entry is %q, want bbb", filepath.Base(failed.Path))
	}
	if failed.Error == "" {
		t.Error("the failing worktree carries no error in the report")
	}
	if failed.Deleted {
		t.Error("a worktree that could not be removed is reported as deleted")
	}
	// Exact, not a threshold: a bound loose enough never to fire is not an
	// assertion. Only what was actually removed may be counted.
	var want int64
	for _, wt := range r.Deleted {
		want += wt.Bytes
	}
	if r.BytesReclaimed != want {
		t.Errorf("BytesReclaimed = %d, want %d (a failed deletion must not count)", r.BytesReclaimed, want)
	}
}

// --- filters and effects ---------------------------------------------------

func TestClean_DryRunIsTheDefaultAndDeletesNothing(t *testing.T) {
	f := newCleanFixture(t)
	path := f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	r := f.run(CleanOptions{OlderThan: 0}) // Apply left false
	if !r.DryRun {
		t.Error("result does not report a dry run")
	}
	if r.DeletedCount != 1 {
		t.Fatalf("reported %d candidates, want 1", r.DeletedCount)
	}
	if r.Deleted[0].Deleted {
		t.Error("a dry run marked an entry as deleted")
	}
	if r.BytesReclaimed <= 0 {
		t.Error("dry run reported no reclaimable bytes; the preview is the whole point")
	}
	mustExist(t, path, "dry run")
}

// Both directions matter: --older-than must spare what is recent AND let
// through what is old. A filter that spares everything passes a
// spare-only test.
func TestClean_OlderThanSparesRecentAndTakesOld(t *testing.T) {
	f := newCleanFixture(t)
	for _, id := range []string{"fresh", "ancient"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
	}
	f.backdate("ancient", 8*24*time.Hour)

	r := f.run(CleanOptions{OlderThan: 168 * time.Hour, Apply: true})
	if deletedPaths(r)["fresh"] {
		t.Error("deleted a worktree younger than --older-than")
	}
	if got := sparedReason(r, "fresh"); got != skipTooRecent {
		t.Errorf("fresh spared for %q, want %q", got, skipTooRecent)
	}
	if !deletedPaths(r)["ancient"] {
		t.Fatal("--older-than spared a worktree older than the threshold; the filter lets nothing through")
	}
	mustNotExist(t, filepath.Join(f.store, "worktrees", "ancient"), "old worktree")
	mustExist(t, filepath.Join(f.store, "worktrees", "fresh"), "recent worktree")
}

func TestClean_KeepLastSparesTheMostRecentEligible(t *testing.T) {
	f := newCleanFixture(t)
	for i, id := range []string{"old", "mid", "new"} {
		f.addWorktree(id)
		f.mergeIntoMain(id)
		f.seedRun(id, store.RunStatusFinished)
		f.backdate(id, time.Duration(90-i*10)*24*time.Hour)
	}

	r := f.run(CleanOptions{OlderThan: 0, KeepLast: 1, Apply: true})
	got := deletedPaths(r)
	if !got["old"] || !got["mid"] {
		t.Errorf("expected the two oldest to go, deleted %v", got)
	}
	if got["new"] {
		t.Error("--keep-last 1 did not spare the most recent worktree")
	}
	if reason := sparedReason(r, "new"); reason != skipKeepLast {
		t.Errorf("spared for %q, want %q", reason, skipKeepLast)
	}
	mustExist(t, filepath.Join(f.store, "worktrees", "new"), "kept worktree")
}

// The run record is evidence — the journal of what the agent did. It is
// small, and it outlives the checkout by default.
func TestClean_KeepsRunRecordUnlessWithRuns(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	f.run(CleanOptions{OlderThan: 0, Apply: true})
	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := s.LoadRun(context.Background(), "merged"); err != nil {
		t.Fatalf("run record was deleted without --with-runs: %v", err)
	}
}

func TestClean_WithRunsDeletesThePairedRecord(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	r := f.run(CleanOptions{OlderThan: 0, Apply: true, WithRuns: true})
	if len(r.Deleted) != 1 || !r.Deleted[0].RunDeleted {
		t.Fatalf("--with-runs did not report the run record as deleted: %+v", r.Deleted)
	}
	s, err := store.New(f.store)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := s.LoadRun(context.Background(), "merged"); err == nil {
		t.Fatal("--with-runs left the run record behind")
	}
}

// A worktree whose run.json cannot be read must be spared: "unreadable"
// is not "absent", and guessing the permissive way is how a sweep eats a
// live run.
func TestClean_SparesWorktreeWhoseRunRecordIsUnreadable(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	runJSON := filepath.Join(f.store, "runs", "merged", "run.json")
	if _, err := os.Stat(runJSON); err != nil {
		t.Fatalf("run.json not at the expected path — fixture drift, not a skip: %v", err)
	}
	mustWrite(t, runJSON, "{ this is not json")

	r := f.run(CleanOptions{Level: CleanAggressive, OlderThan: 0, Apply: true})
	if deletedPaths(r)["merged"] {
		t.Fatal("deleted a worktree whose run record could not be read")
	}
	if got := sparedReason(r, "merged"); got != skipRunActive {
		t.Fatalf("spared for %q, want %q", got, skipRunActive)
	}
	mustExist(t, filepath.Join(f.store, "worktrees", "merged"), "unreadable run record")
}

// --- store resolution ------------------------------------------------------

// --all-projects is the widest blast radius the command has: every store
// under the iterion data dir, including projects the operator was not
// thinking about.
func TestClean_AllProjectsSweepsEveryStoreAndKeepLastIsPerStore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)

	projects := map[string]*cleanFixture{}
	for _, name := range []string{"alpha", "beta"} {
		root := filepath.Join(home, "projects", name)
		mustMkdir(t, filepath.Join(root, "worktrees"))
		repo := filepath.Join(t.TempDir(), "repo-"+name)
		mustMkdir(t, repo)
		f := &cleanFixture{t: t, root: filepath.Dir(repo), repo: repo, store: root}
		f.initRepo(repo)
		for i, id := range []string{name + "-old", name + "-new"} {
			f.addWorktree(id)
			f.mergeIntoMain(id)
			f.seedRun(id, store.RunStatusFinished)
			f.backdate(id, time.Duration(90-i*10)*24*time.Hour)
		}
		projects[name] = f
	}
	// A project store with no worktrees dir must not be listed.
	mustMkdir(t, filepath.Join(home, "projects", "gamma"))

	var buf bytes.Buffer
	err := RunClean(CleanOptions{
		Level: CleanConservative, OlderThan: 0, AllProjects: true, KeepLast: 1, Apply: true,
	}, &Printer{W: &buf, Format: OutputJSON})
	if err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	r := decodeCleanResult(t, buf.Bytes())

	if len(r.Stores) != 2 {
		t.Fatalf("swept %d stores %v, want the 2 with worktrees/", len(r.Stores), r.Stores)
	}
	if r.Scanned != 4 {
		t.Fatalf("scanned %d, want 4 across both stores", r.Scanned)
	}
	// keep-last 1 must spare the newest of EACH store, not one overall.
	for name, f := range projects {
		mustNotExist(t, filepath.Join(f.store, "worktrees", name+"-old"), name+" oldest")
		mustExist(t, filepath.Join(f.store, "worktrees", name+"-new"), name+" kept by keep-last")
	}
	if r.DeletedCount != 2 {
		t.Fatalf("deleted %d, want 1 per store", r.DeletedCount)
	}
}

// A store directory the operator named explicitly and that does not
// exist is a typo, not an empty store. Reporting success would have a
// cron happily cleaning nothing forever.
func TestClean_ExplicitStoreDirMustExist(t *testing.T) {
	var buf bytes.Buffer
	err := RunClean(CleanOptions{
		StoreDir: filepath.Join(t.TempDir(), "does-not-exist"),
		Level:    CleanConservative,
	}, &Printer{W: &buf, Format: OutputJSON})
	if err == nil {
		t.Fatal("a nonexistent --store-dir was reported as nothing to clean")
	}
	if !strings.Contains(err.Error(), "store-dir") {
		t.Errorf("error does not name the flag: %v", err)
	}
}

// --- output and validation -------------------------------------------------

// The table is what an operator reads before typing --apply.
func TestClean_TableOutputNamesWhatItWouldDelete(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	var buf bytes.Buffer
	if err := RunClean(CleanOptions{
		StoreDir: f.store, Level: CleanConservative, OlderThan: 0,
	}, &Printer{W: &buf, Format: OutputHuman}); err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"merged", "would delete", "dry run", "--apply"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output does not mention %q:\n%s", want, out)
		}
	}
}

func TestClean_DryRunDoesNotClaimRunRecordsWereDeleted(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	var buf bytes.Buffer
	if err := RunClean(CleanOptions{
		StoreDir: f.store, Level: CleanConservative, OlderThan: 0, WithRuns: true,
	}, &Printer{W: &buf, Format: OutputHuman}); err != nil {
		t.Fatalf("RunClean: %v", err)
	}
	if strings.Contains(buf.String(), "run records deleted") {
		t.Errorf("dry run claims run records were deleted:\n%s", buf.String())
	}
}

// Both negatives fail open: a negative --older-than makes the age filter
// a no-op, a negative --keep-last makes the floor a no-op. Refusing them
// is the only thing between a typo and a wider sweep than intended.
func TestClean_RejectsNegativeBounds(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("merged")
	f.mergeIntoMain("merged")
	f.seedRun("merged", store.RunStatusFinished)

	for name, opts := range map[string]CleanOptions{
		"older-than": {OlderThan: -time.Hour},
		"keep-last":  {KeepLast: -1},
	} {
		t.Run(name, func(t *testing.T) {
			o := opts
			o.StoreDir, o.Level, o.Apply = f.store, CleanConservative, true
			if _, err := f.runErr(o); err == nil {
				t.Fatalf("a negative --%s was accepted", name)
			}
			mustExist(t, filepath.Join(f.store, "worktrees", "merged"), "worktree after a rejected flag")
		})
	}
}

// Taking the run lock creates the run directory, so a sweep that locks
// before checking would resurrect the records `runs prune` removed — and
// a resurrected, unreadable record reads as `running`, which would spare
// that worktree for good.
func TestClean_DoesNotResurrectRunRecords(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("recordless")
	f.mergeIntoMain("recordless")
	// deliberately no seedRun: this is a worktree whose run was pruned

	r := f.run(CleanOptions{Level: CleanConservative, OlderThan: 0, Apply: true})
	if !deletedPaths(r)["recordless"] {
		t.Fatalf("a worktree with no run record was not swept (spared: %q)", sparedReason(r, "recordless"))
	}
	if _, err := os.Stat(filepath.Join(f.store, "runs", "recordless")); !os.IsNotExist(err) {
		t.Fatal("the sweep recreated the run directory of a run that no longer exists")
	}
}

// The warning that a tree may be half-removed is the one line telling an
// operator to go and look. Only --json was ever asserted.
func TestClean_HumanOutputWarnsAboutAPartialDeletion(t *testing.T) {
	f := newCleanFixture(t)
	f.addWorktree("doomed")
	f.mergeIntoMain("doomed")
	f.seedRun("doomed", store.RunStatusFinished)

	real := removeTree
	removeTree = func(p string) error { return errors.New("simulated: another owner's file") }
	t.Cleanup(func() { removeTree = real })

	var buf bytes.Buffer
	err := RunClean(CleanOptions{
		StoreDir: f.store, Level: CleanConservative, OlderThan: 0, Apply: true,
	}, &Printer{W: &buf, Format: OutputHuman})
	if err == nil {
		t.Fatal("a failed deletion did not surface an error")
	}
	if !strings.Contains(buf.String(), "may be partially deleted") {
		t.Fatalf("the human report does not warn about a partial deletion:\n%s", buf.String())
	}
}

func TestClean_RejectsUnknownLevel(t *testing.T) {
	f := newCleanFixture(t)
	var buf bytes.Buffer
	err := RunClean(CleanOptions{StoreDir: f.store, Level: CleanLevel("nuke")},
		&Printer{W: &buf, Format: OutputJSON})
	if err == nil {
		t.Fatal("an unknown level was accepted")
	}
	if !strings.Contains(err.Error(), "conservative") {
		t.Errorf("error does not name the allowed levels: %v", err)
	}
}

func TestClean_RejectsAllProjectsWithStoreDir(t *testing.T) {
	var buf bytes.Buffer
	err := RunClean(CleanOptions{
		StoreDir:    t.TempDir(),
		Level:       CleanConservative,
		AllProjects: true,
	}, &Printer{W: &buf, Format: OutputJSON})
	if err == nil {
		t.Fatal("--all-projects with --store-dir was accepted")
	}
}

// A store with no worktrees directory is an ordinary state, not a failure.
func TestClean_EmptyStoreIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := RunClean(CleanOptions{
		StoreDir: dir,
		Level:    CleanConservative,
	}, &Printer{W: &buf, Format: OutputJSON}); err != nil {
		t.Fatalf("RunClean on an empty store: %v", err)
	}
	r := decodeCleanResult(t, buf.Bytes())
	if r.Scanned != 0 || r.DeletedCount != 0 {
		t.Fatalf("empty store reported scanned=%d deleted=%d", r.Scanned, r.DeletedCount)
	}
}
